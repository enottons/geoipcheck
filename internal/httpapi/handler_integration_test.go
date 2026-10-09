//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/geoip"
	"github.com/enottons/geoipcheck/internal/httpapi"
	"github.com/enottons/geoipcheck/internal/testutil/logtest"
	"github.com/enottons/geoipcheck/internal/testutil/metricstest"
)

// These tests own what the unit tests can't show: the real net/http stack,
// the real domain and reader behind it, and that metrics are wired. Decode
// and mapping rules are owned by handler_test.go.

type stack struct {
	srv    *httptest.Server
	reader *geoip.Reader
	reg    *prometheus.Registry
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{reader: &geoip.Reader{}, reg: prometheus.NewRegistry()}
	_, logger := logtest.New()
	s.srv = httptest.NewServer(httpapi.NewHandler(httpapi.Config{
		Checker: countrycheck.NewService(s.reader),
		Metrics: apimetrics.New(s.reg),
		Logger:  logger,
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stack) loadTestDB(t *testing.T) {
	t.Helper()
	db, err := geoip.LoadFile("../../testdata/GeoLite2-Country-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	s.reader.Store(db)
}

func (s *stack) post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.srv.URL+"/v1/country-checks", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "it-1")
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHTTPStack_checkAgainstRealDatabase(t *testing.T) {
	s := newStack(t)
	s.loadTestDB(t)

	resp := s.post(t, `{"ip":"81.2.69.142","allowed_countries":["us","gb"]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["country"] != "GB" || body["decision"] != "allowed" {
		t.Errorf("body=%v", body)
	}

	allowed := metricstest.Find(t, s.reg, "geoipcheck_checks_total",
		map[string]string{"transport": "http", "decision": "allowed", "reason": "country_allowed"}).GetCounter().GetValue()
	if allowed != 1 {
		t.Errorf("checks_total{allowed}=%v, want 1", allowed)
	}
	if n := metricstest.Find(t, s.reg, "geoipcheck_request_duration_seconds",
		map[string]string{"transport": "http", "operation": "check_country", "code": "200"}).GetHistogram().GetSampleCount(); n != 1 {
		t.Errorf("request_duration count{200}=%d, want 1", n)
	}
}

func TestHTTPStack_emptyReaderIs503(t *testing.T) {
	// The chain geoip.ErrNotLoaded → countrycheck.ErrDatabaseUnavailable →
	// 503 only exists with the real reader and service.
	s := newStack(t)

	resp := s.post(t, `{"ip":"81.2.69.142","allowed_countries":["GB"]}`)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	if n := metricstest.Find(t, s.reg, "geoipcheck_request_duration_seconds",
		map[string]string{"transport": "http", "operation": "check_country", "code": "503"}).GetHistogram().GetSampleCount(); n != 1 {
		t.Errorf("request_duration count{503}=%d, want 1", n)
	}
}
