// Package apimetrics holds the request metrics shared by the HTTP and gRPC
// adapters. They live in one package so both transports emit the same
// metric names, told apart by the transport label.
package apimetrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/enottons/geoipcheck/internal/countrycheck"
)

// Transport is the transport label value. It is a type so an adapter
// can't emit a misspelled label.
type Transport string

// Transport label values.
const (
	TransportHTTP Transport = "http"
	TransportGRPC Transport = "grpc"
)

// Operation label values, shared by both transports.
const (
	OperationCheckCountry = "check_country"
	// OperationUnmatched labels requests for no known route or method, so
	// that traffic is visible without one label value per path.
	OperationUnmatched = "unmatched"
)

// Metrics records completed checks and request latency.
type Metrics struct {
	checks   *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// New registers the metrics on reg. It panics if they are already
// registered, like prometheus.MustRegister.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "geoipcheck_checks_total",
			Help: "Completed country checks by decision and reason.",
		}, []string{"transport", "decision", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "geoipcheck_request_duration_seconds",
			Help: "Request latency by transport, operation and status code.",
			// Lookups are sub-millisecond; the low buckets carry the signal.
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"transport", "operation", "code"}),
	}
	reg.MustRegister(m.checks, m.duration)
	// Pre-created so rate() over an outcome that hasn't happened yet reads
	// 0, not "no data".
	for _, transport := range []Transport{TransportHTTP, TransportGRPC} {
		for _, o := range countrycheck.Outcomes() {
			m.checks.WithLabelValues(string(transport), string(o.Decision), string(o.Reason))
		}
	}
	return m
}

// Check counts one successful check.
func (m *Metrics) Check(transport Transport, r countrycheck.Result) {
	m.checks.WithLabelValues(string(transport), string(r.Decision), string(r.Reason)).Inc()
}

// Request observes one finished request. code is the HTTP status
// ("200") or the gRPC code name ("InvalidArgument").
func (m *Metrics) Request(transport Transport, operation, code string, d time.Duration) {
	m.duration.WithLabelValues(string(transport), operation, code).Observe(d.Seconds())
}
