// Package admin serves the operational endpoints on their own port:
// /healthz (liveness), /readyz (readiness) and /metrics (Prometheus). They
// are kept off the API port so they are never exposed through the gateway.
package admin

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	errNotLoaded = errors.New("geoip database not loaded")
	errDraining  = errors.New("shutting down")
)

// Readiness decides whether this replica should receive traffic (D10):
// ready once a valid database is loaded, and not ready again once shutdown
// starts so the load balancer drains it first. Database staleness does not
// affect readiness. It is safe for concurrent use.
type Readiness struct {
	dbLoaded func() bool
	draining atomic.Bool
}

// NewReadiness returns a Readiness that asks dbLoaded whether a database
// is in memory.
func NewReadiness(dbLoaded func() bool) *Readiness {
	return &Readiness{dbLoaded: dbLoaded}
}

// StartDraining marks the replica not ready. It cannot be undone.
func (r *Readiness) StartDraining() { r.draining.Store(true) }

// Check returns nil when ready, or the reason it is not.
func (r *Readiness) Check() error {
	switch {
	case r.draining.Load():
		return errDraining
	case !r.dbLoaded():
		return errNotLoaded
	default:
		return nil
	}
}

// Config wires the admin handler. All fields are required.
type Config struct {
	Readiness *Readiness
	Gatherer  prometheus.Gatherer
	Logger    *slog.Logger
}

// NewHandler returns the admin endpoints.
func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	// Liveness only says the process can serve HTTP. Tying it to the
	// database would get pods restarted during a MaxMind outage, which
	// fixes nothing.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := cfg.Readiness.Check(); err != nil {
			writeText(w, http.StatusServiceUnavailable, "not ready: "+err.Error())
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(cfg.Gatherer, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelError),
	}))
	return mux
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
