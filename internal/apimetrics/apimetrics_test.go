package apimetrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/enottons/geoipcheck/internal/countrycheck"
)

func TestNew_precreatesEveryOutcomePerTransport(t *testing.T) {
	m := New(prometheus.NewRegistry())
	if got, want := testutil.CollectAndCount(m.checks), 2*len(countrycheck.Outcomes()); got != want {
		t.Fatalf("checks series=%d, want %d", got, want)
	}
}

func TestCheck_countsByTransportDecisionReason(t *testing.T) {
	m := New(prometheus.NewRegistry())
	m.Check(TransportHTTP, countrycheck.Result{Decision: countrycheck.DecisionDenied, Reason: countrycheck.ReasonCountryNotAllowed})

	if got := testutil.ToFloat64(m.checks.WithLabelValues("http", "denied", "country_not_allowed")); got != 1 {
		t.Errorf("http denied=%v, want 1", got)
	}
	if got := testutil.ToFloat64(m.checks.WithLabelValues("grpc", "denied", "country_not_allowed")); got != 0 {
		t.Errorf("grpc denied=%v, want 0", got)
	}
}

func TestRequest_observesLatencyUnderItsLabels(t *testing.T) {
	m := New(prometheus.NewRegistry())
	m.Request(TransportGRPC, OperationCheckCountry, "OK", 2*time.Millisecond)

	metric, ok := m.duration.WithLabelValues("grpc", "check_country", "OK").(prometheus.Metric)
	if !ok {
		t.Fatal("duration series is not a prometheus.Metric")
	}
	var pb dto.Metric
	if err := metric.Write(&pb); err != nil {
		t.Fatal(err)
	}
	if got := pb.GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("sample count=%d, want 1", got)
	}
	if got := pb.GetHistogram().GetSampleSum(); got != 0.002 {
		t.Errorf("sample sum=%v, want 0.002s", got)
	}
}
