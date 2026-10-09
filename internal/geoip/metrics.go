package geoip

import "github.com/prometheus/client_golang/prometheus"

// reloads_total result label values.
const (
	reloadSuccess = "success"
	reloadFailure = "failure"
)

// Metrics are the database lifecycle metrics. Staleness alerts are built
// on db_build_timestamp_seconds (see docs/operations.md); readiness does not
// depend on freshness (D10).
type Metrics struct {
	buildTimestamp prometheus.Gauge
	loaded         prometheus.Gauge
	reloads        *prometheus.CounterVec
}

// NewMetrics creates the metrics and registers them with reg. It panics on
// duplicate registration, which is a wiring bug.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		buildTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "geoipcheck_db_build_timestamp_seconds",
			Help: "Build time of the loaded GeoIP database, as Unix seconds. 0 until a database is loaded.",
		}),
		loaded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "geoipcheck_db_loaded",
			Help: "1 if a GeoIP database is loaded and serving lookups, else 0.",
		}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "geoipcheck_db_reloads_total",
			Help: "GeoIP database load attempts, by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(m.buildTimestamp, m.loaded, m.reloads)
	// Pre-create both series so rate() and absent() alerts work from the
	// first scrape instead of after the first failure.
	m.reloads.WithLabelValues(reloadSuccess)
	m.reloads.WithLabelValues(reloadFailure)
	return m
}
