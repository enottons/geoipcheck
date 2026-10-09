// Package logtest captures slog records so tests can assert on log output
// without parsing text. Test-only; production code must not import it.
package logtest

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

// Record is a captured log line with its attributes flattened. Group
// attributes are flattened with "." separators, e.g. "db.path".
type Record struct {
	Level   slog.Level
	Message string
	Attrs   map[string]slog.Value
}

// Handler is a slog.Handler that keeps every record in memory. It is safe
// for concurrent use. It follows the slog.Handler rules that matter for
// assertions: empty attrs are dropped, empty-key groups are inlined, and
// WithGroup("") is a no-op.
type Handler struct {
	mu      *sync.Mutex
	records *[]Record
	preset  map[string]slog.Value // from WithAttrs, already flattened
	group   string
}

// New returns a Handler and a logger that writes to it. All levels are
// captured.
func New() (*Handler, *slog.Logger) {
	h := &Handler{mu: &sync.Mutex{}, records: &[]Record{}, preset: map[string]slog.Value{}}
	return h, slog.New(h)
}

// Enabled reports true for every level.
func (h *Handler) Enabled(context.Context, slog.Level) bool { return true }

// Handle captures r along with any attrs added through WithAttrs.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	attrs := maps.Clone(h.preset)
	r.Attrs(func(a slog.Attr) bool {
		flatten(attrs, h.group, a)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, Record{Level: r.Level, Message: r.Message, Attrs: attrs})
	return nil
}

// WithAttrs returns a Handler that adds as to every record, qualified by
// the current group.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	next := *h
	next.preset = maps.Clone(h.preset)
	for _, a := range as {
		flatten(next.preset, h.group, a)
	}
	return &next
}

// WithGroup returns a Handler that qualifies later attrs with name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.group = join(h.group, name)
	return &next
}

// Records returns a copy of everything captured so far.
func (h *Handler) Records() []Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(*h.records)
}

// Find returns the records whose message equals msg.
func (h *Handler) Find(msg string) []Record {
	var out []Record
	for _, r := range h.Records() {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

func flatten(dst map[string]slog.Value, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		// An empty-key group is inlined into its parent.
		for _, ga := range a.Value.Group() {
			flatten(dst, join(prefix, a.Key), ga)
		}
		return
	}
	dst[join(prefix, a.Key)] = a.Value
}

func join(prefix, key string) string {
	switch {
	case prefix == "":
		return key
	case key == "":
		return prefix
	default:
		return prefix + "." + key
	}
}
