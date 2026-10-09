package logtest

import (
	"log/slog"
	"testing"
)

func TestHandler_flattensGroupsAndAttrs(t *testing.T) {
	h, logger := New()
	logger.With("svc", "x").WithGroup("db").With("path", "/a").
		Info("msg", "size", 1, slog.Group("meta", "type", "T"))

	got := h.Find("msg")
	if len(got) != 1 {
		t.Fatalf("records=%+v", h.Records())
	}
	want := map[string]string{"svc": "x", "db.path": "/a", "db.size": "1", "db.meta.type": "T"}
	for k, v := range want {
		if got[0].Attrs[k].String() != v {
			t.Errorf("%s=%q, want %q (all: %v)", k, got[0].Attrs[k], v, got[0].Attrs)
		}
	}
	if len(got[0].Attrs) != len(want) {
		t.Errorf("attrs=%v, want exactly %v", got[0].Attrs, want)
	}
}

func TestHandler_followsSlogHandlerRules(t *testing.T) {
	h, logger := New()
	logger.WithGroup("").Info("msg",
		slog.Attr{},                      // empty attr: dropped
		slog.Group("", slog.Int("n", 1)), // empty-key group: inlined
		slog.Group("g"),                  // empty group: dropped
	)

	attrs := h.Find("msg")[0].Attrs
	if len(attrs) != 1 || attrs["n"].Int64() != 1 {
		t.Fatalf("attrs=%v, want only n=1", attrs)
	}
}
