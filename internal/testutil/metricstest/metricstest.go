// Package metricstest reads single series out of a Prometheus gatherer.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Find returns the series of the named family whose labels are exactly
// labels, or nil if there is none. The dto getters are nil-safe, so
// Find(...).GetCounter().GetValue() reads 0 for a missing series.
func Find(t testing.TB, g prometheus.Gatherer, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	for _, m := range family(t, g, name).GetMetric() {
		if labelsEqual(m.GetLabel(), labels) {
			return m
		}
	}
	return nil
}

// Count returns how many series the named family has; 0 if it is absent.
func Count(t testing.TB, g prometheus.Gatherer, name string) int {
	t.Helper()
	return len(family(t, g, name).GetMetric())
}

func family(t testing.TB, g prometheus.Gatherer, name string) *dto.MetricFamily {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func labelsEqual(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, l := range got {
		if v, ok := want[l.GetName()]; !ok || v != l.GetValue() {
			return false
		}
	}
	return true
}
