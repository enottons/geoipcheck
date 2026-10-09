package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestFind_matchesExactLabelSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "things_total", Help: "h"}, []string{"a", "b"})
	reg.MustRegister(c)
	c.WithLabelValues("1", "2").Add(3)
	c.WithLabelValues("1", "9").Add(5)

	if got := Find(t, reg, "things_total", map[string]string{"a": "1", "b": "2"}).GetCounter().GetValue(); got != 3 {
		t.Errorf("a=1,b=2: %v, want 3", got)
	}
	if m := Find(t, reg, "things_total", map[string]string{"a": "1"}); m != nil {
		t.Errorf("a partial label set matched %v", m)
	}
	if got := Find(t, reg, "missing_total", nil).GetCounter().GetValue(); got != 0 {
		t.Errorf("missing family read %v, want 0", got)
	}
	if got := Count(t, reg, "things_total"); got != 2 {
		t.Errorf("Count(things_total)=%d, want 2", got)
	}
	if got := Count(t, reg, "missing_total"); got != 0 {
		t.Errorf("Count(missing_total)=%d, want 0", got)
	}
}
