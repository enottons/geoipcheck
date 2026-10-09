package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestResolve_keepsWellFormedIncomingID(t *testing.T) {
	for _, id := range []string{"abc-123", "6f1c2a1e-6b7a-4c1e-9d0e-0f6b8c9a1d2e", strings.Repeat("a", MaxLen)} {
		if got := Resolve(id); got != id {
			t.Errorf("Resolve(%q)=%q, want it kept", id, got)
		}
	}
}

func TestResolve_replacesUnusableID(t *testing.T) {
	for _, id := range []string{"", "has space", "line\nbreak", "tab\t", strings.Repeat("a", MaxLen+1), "héllo"} {
		got := Resolve(id)
		if got == id || len(got) != 26 {
			t.Errorf("Resolve(%q)=%q, want a fresh 26-char ID", id, got)
		}
	}
}

func TestNew_isUnique(t *testing.T) {
	if a, b := New(), New(); a == b {
		t.Fatal("two generated IDs collided")
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := NewContext(context.Background(), "abc")
	if got := FromContext(ctx); got != "abc" {
		t.Errorf("FromContext=%q", got)
	}
	if got := FromContext(context.Background()); got != "" {
		t.Errorf("FromContext(empty)=%q", got)
	}
}
