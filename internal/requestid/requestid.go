// Package requestid carries the per-request correlation ID (D14). Both
// transports accept the caller's ID when it is well formed and generate one
// otherwise, so every log line and response has exactly one ID.
package requestid

import (
	"context"
	"crypto/rand"
)

// MaxLen bounds an accepted incoming ID. Gateways use UUIDs or similar; an
// ID longer than this is more likely abuse than correlation.
const MaxLen = 128

type ctxKey struct{}

// Resolve returns incoming when it is a usable ID, or a new random ID.
// Usable means 1..MaxLen bytes of visible ASCII (0x21–0x7E): no spaces or
// control characters, so it cannot break log lines or header framing.
func Resolve(incoming string) string {
	if valid(incoming) {
		return incoming
	}
	return New()
}

// New returns a random 26-character base32 ID (128 bits).
func New() string {
	return rand.Text()
}

// NewContext returns a context carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the ID stored by NewContext, or "".
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}
	for i := range len(id) {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	return true
}
