package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/enottons/geoipcheck/internal/testutil/logtest"
)

type adminHarness struct {
	handler   http.Handler
	readiness *Readiness
	loaded    *atomic.Bool
	reg       *prometheus.Registry
}

func newAdmin(t *testing.T) *adminHarness {
	t.Helper()
	h := &adminHarness{loaded: &atomic.Bool{}, reg: prometheus.NewRegistry()}
	h.readiness = NewReadiness(h.loaded.Load)
	_, logger := logtest.New()
	h.handler = NewHandler(Config{Readiness: h.readiness, Gatherer: h.reg, Logger: logger})
	return h
}

func (h *adminHarness) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz_alwaysOK(t *testing.T) {
	h := newAdmin(t)
	h.readiness.StartDraining()
	if rec := h.get("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("status=%d: liveness must not depend on readiness", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	h := newAdmin(t)

	rec := h.get("/readyz")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not loaded") {
		t.Fatalf("before load: %d %q", rec.Code, rec.Body)
	}

	h.loaded.Store(true)
	if rec := h.get("/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("after load: %d %q", rec.Code, rec.Body)
	}

	h.readiness.StartDraining()
	rec = h.get("/readyz")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting down") {
		t.Fatalf("while draining: %d %q", rec.Code, rec.Body)
	}
}

func TestMetrics_servesRegistry(t *testing.T) {
	h := newAdmin(t)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "geoipcheck_test_total", Help: "t"})
	h.reg.MustRegister(c)
	c.Inc()

	rec := h.get("/metrics")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "geoipcheck_test_total 1") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestAdmin_rejectsWrites(t *testing.T) {
	h := newAdmin(t)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/readyz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rec.Code)
	}
}
