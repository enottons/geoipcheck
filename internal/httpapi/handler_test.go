package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/testutil/logtest"
	"github.com/enottons/geoipcheck/internal/testutil/metricstest"
)

// fakeChecker returns a canned result or error and records what it saw.
type fakeChecker struct {
	res   countrycheck.Result
	err   error
	panic any
	got   []countrycheck.Request
}

func (f *fakeChecker) Check(_ context.Context, req countrycheck.Request) (countrycheck.Result, error) {
	f.got = append(f.got, req)
	if f.panic != nil {
		panic(f.panic)
	}
	return f.res, f.err
}

type apiHarness struct {
	handler http.Handler
	checker *fakeChecker
	logs    *logtest.Handler
	reg     *prometheus.Registry
}

func newAPI(t *testing.T, checker *fakeChecker) *apiHarness {
	t.Helper()
	logs, logger := logtest.New()
	reg := prometheus.NewRegistry()
	return &apiHarness{
		handler: NewHandler(Config{Checker: checker, Metrics: apimetrics.New(reg), Logger: logger}),
		checker: checker,
		logs:    logs,
		reg:     reg,
	}
}

// requestCount returns how many HTTP requests were observed with this
// operation and code label.
func (h *apiHarness) requestCount(t *testing.T, operation, code string) uint64 {
	t.Helper()
	return metricstest.Find(t, h.reg, "geoipcheck_request_duration_seconds",
		map[string]string{"transport": "http", "operation": operation, "code": code}).GetHistogram().GetSampleCount()
}

func (h *apiHarness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *apiHarness) accessLog(t *testing.T) logtest.Record {
	t.Helper()
	logs := h.logs.Find("http request")
	if len(logs) != 1 {
		t.Fatalf("want one access log line, got %+v", h.logs.Records())
	}
	return logs[0]
}

func postJSON(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, checkCountryPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeProblemBody(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type=%q, want application/problem+json; body=%s", ct, rec.Body)
	}
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v; body=%s", err, rec.Body)
	}
	return p
}

var gbAllowed = countrycheck.Result{
	IP:       netip.MustParseAddr("81.2.69.142"),
	Country:  "GB",
	Decision: countrycheck.DecisionAllowed,
	Reason:   countrycheck.ReasonCountryAllowed,
}

func TestCheckCountry_success(t *testing.T) {
	api := newAPI(t, &fakeChecker{res: gbAllowed})

	rec := api.do(t, postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB","us"]}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q", ct)
	}
	want := `{"ip":"81.2.69.142","country":"GB","decision":"allowed","reason":"country_allowed"}` + "\n"
	if diff := cmp.Diff(want, rec.Body.String()); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}
	wantReq := []countrycheck.Request{{IP: "81.2.69.142", AllowedCountries: []string{"GB", "us"}}}
	if diff := cmp.Diff(wantReq, api.checker.got); diff != "" {
		t.Errorf("request passed to checker (-want +got):\n%s", diff)
	}
}

func TestCheckCountry_unresolvedCountryIsNull(t *testing.T) {
	api := newAPI(t, &fakeChecker{res: countrycheck.Result{
		IP:       netip.MustParseAddr("10.0.0.1"),
		Decision: countrycheck.DecisionUnknown,
		Reason:   countrycheck.ReasonNonRoutableAddress,
	}})

	rec := api.do(t, postJSON(`{"ip":"10.0.0.1","allowed_countries":["GB"]}`))

	want := `{"ip":"10.0.0.1","country":null,"decision":"unknown","reason":"non_routable_address"}` + "\n"
	if diff := cmp.Diff(want, rec.Body.String()); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}
}

func TestCheckCountry_domainErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		want       problem
	}{
		{
			name: "validation error carries code, field and suggestion",
			err: &countrycheck.ValidationError{
				Field: "allowed_countries[0]", Code: countrycheck.CodeInvalidCountryCode,
				Message: `"UK" is not an ISO 3166-1 alpha-2 code; did you mean "GB"?`, Suggestion: "GB",
			},
			wantStatus: http.StatusBadRequest,
			want: problem{
				Title: "Bad Request", Status: 400, Code: "invalid_country_code", Field: "allowed_countries[0]",
				Detail: `"UK" is not an ISO 3166-1 alpha-2 code; did you mean "GB"?`, Suggestion: "GB",
			},
		},
		{
			name:       "database unavailable",
			err:        fmt.Errorf("%w: not loaded", countrycheck.ErrDatabaseUnavailable),
			wantStatus: http.StatusServiceUnavailable,
			want: problem{
				Title: "Service Unavailable", Status: 503, Code: countrycheck.CodeDatabaseUnavailable,
				Detail: "geoip database is not available; retry later",
			},
		},
		{
			name:       "client went away",
			err:        fmt.Errorf("country lookup: %w", context.Canceled),
			wantStatus: statusClientClosedRequest,
			want:       problem{Title: "Client Closed Request", Status: 499, Code: CodeClientClosedRequest},
		},
		{
			name:       "deadline exceeded",
			err:        fmt.Errorf("country lookup: %w", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
			want:       problem{Title: "Gateway Timeout", Status: 504, Code: CodeDeadlineExceeded},
		},
		{
			name:       "anything else is internal and not leaked",
			err:        errors.New("secret internal detail"),
			wantStatus: http.StatusInternalServerError,
			want:       problem{Title: "Internal Server Error", Status: 500, Code: CodeInternal, Detail: "internal error"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAPI(t, &fakeChecker{err: c.err})
			req := postJSON(`{"ip":"81.2.69.142","allowed_countries":["UK"]}`)
			req.Header.Set(requestIDHeader, "req-1")

			rec := api.do(t, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status=%d, want %d", rec.Code, c.wantStatus)
			}
			c.want.RequestID = "req-1"
			if diff := cmp.Diff(c.want, decodeProblemBody(t, rec), cmpopts.IgnoreUnexported(problem{})); diff != "" {
				t.Errorf("problem (-want +got):\n%s", diff)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("internal error text leaked to the client: %s", rec.Body)
			}
		})
	}
}

func TestCheckCountry_unavailableSetsRetryAfter(t *testing.T) {
	api := newAPI(t, &fakeChecker{err: countrycheck.ErrDatabaseUnavailable})
	rec := api.do(t, postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`))
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After=%q, want 5", got)
	}
}

func TestCheckCountry_decodeErrors(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
		wantField   string
	}{
		{"missing content type", "", `{}`, 415, CodeUnsupportedMediaType, ""},
		{"wrong content type", "text/plain", `{}`, 415, CodeUnsupportedMediaType, ""},
		{"content type with charset is fine but body empty", "application/json; charset=utf-8", ``, 400, CodeMalformedJSON, ""},
		{"not JSON", "application/json", `ip=1.2.3.4`, 400, CodeMalformedJSON, ""},
		{"truncated", "application/json", `{"ip":"1.2.3.4"`, 400, CodeMalformedJSON, ""},
		{"array instead of object", "application/json", `[]`, 400, CodeMalformedJSON, ""},
		{"wrong field type", "application/json", `{"ip":42,"allowed_countries":["GB"]}`, 400, CodeMalformedJSON, "ip"},
		{"wrong element type", "application/json", `{"ip":"1.2.3.4","allowed_countries":["GB",1]}`, 400, CodeMalformedJSON, "allowed_countries[1]"},
		{"unknown field", "application/json", `{"ip":"1.2.3.4","allowed_countries":["GB"],"customer_id":"c1"}`, 400, CodeUnknownField, "customer_id"},
		{"trailing object", "application/json", `{"ip":"1.2.3.4","allowed_countries":["GB"]}{}`, 400, CodeMalformedJSON, ""},
		{"trailing garbage", "application/json", `{"ip":"1.2.3.4","allowed_countries":["GB"]} x`, 400, CodeMalformedJSON, ""},
		{"too large", "application/json", `{"ip":"` + strings.Repeat("1", maxBodyBytes) + `"}`, 413, CodeRequestTooLarge, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAPI(t, &fakeChecker{res: gbAllowed})
			req := httptest.NewRequest(http.MethodPost, checkCountryPath, strings.NewReader(c.body))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}

			rec := api.do(t, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", rec.Code, c.wantStatus, rec.Body)
			}
			p := decodeProblemBody(t, rec)
			if p.Code != c.wantCode || p.Field != c.wantField {
				t.Errorf("code=%q field=%q, want %q %q (detail %q)", p.Code, p.Field, c.wantCode, c.wantField, p.Detail)
			}
			if p.Detail == "" {
				t.Error("decode problems must explain themselves in detail")
			}
			if len(api.checker.got) != 0 {
				t.Error("checker must not be called for an undecodable request")
			}
		})
	}
}

func TestCheckCountry_trailingWhitespaceIsAllowed(t *testing.T) {
	api := newAPI(t, &fakeChecker{res: gbAllowed})
	rec := api.do(t, postJSON("{\"ip\":\"81.2.69.142\",\"allowed_countries\":[\"GB\"]}\n\n"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestRouting_unknownPathIsProblem404(t *testing.T) {
	api := newAPI(t, &fakeChecker{})
	rec := api.do(t, httptest.NewRequest(http.MethodGet, "/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
	if p := decodeProblemBody(t, rec); p.Code != CodeNotFound {
		t.Errorf("code=%q", p.Code)
	}
}

func TestRouting_wrongMethodIsProblem405WithAllow(t *testing.T) {
	api := newAPI(t, &fakeChecker{})
	rec := api.do(t, httptest.NewRequest(http.MethodGet, checkCountryPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow=%q, want POST", got)
	}
	if p := decodeProblemBody(t, rec); p.Code != CodeMethodNotAllowed {
		t.Errorf("code=%q", p.Code)
	}
}

func TestRequestID_echoedWhenValidGeneratedOtherwise(t *testing.T) {
	api := newAPI(t, &fakeChecker{res: gbAllowed})

	req := postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`)
	req.Header.Set(requestIDHeader, "gw-abc-123")
	if got := api.do(t, req).Header().Get(requestIDHeader); got != "gw-abc-123" {
		t.Errorf("echoed ID=%q, want gw-abc-123", got)
	}

	req = postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`)
	req.Header.Set(requestIDHeader, "bad id with spaces")
	if got := api.do(t, req).Header().Get(requestIDHeader); got == "" || strings.Contains(got, " ") {
		t.Errorf("replacement ID=%q, want a generated one", got)
	}
}

func TestAccessLog_successFields(t *testing.T) {
	api := newAPI(t, &fakeChecker{res: gbAllowed})
	req := postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB","US","CA"]}`)
	req.Header.Set(requestIDHeader, "req-7")

	api.do(t, req)

	rec := api.accessLog(t)
	if rec.Level != slog.LevelInfo {
		t.Errorf("level=%v, want info", rec.Level)
	}
	want := map[string]string{
		"request_id":       "req-7",
		"method":           "POST",
		"path":             checkCountryPath,
		"status":           "200",
		"client_ip_prefix": "81.2.69.0/24",
		"allowlist_size":   "3",
		"country":          "GB",
		"decision":         "allowed",
		"reason":           "country_allowed",
	}
	for k, v := range want {
		if got := rec.Attrs[k].String(); got != v {
			t.Errorf("%s=%q, want %q", k, got, v)
		}
	}
	if _, ok := rec.Attrs["duration"]; !ok {
		t.Error("duration missing")
	}
}

func TestAccessLog_neverContainsRawIPOrAllowlist(t *testing.T) {
	api := newAPI(t, &fakeChecker{err: &countrycheck.ValidationError{Code: countrycheck.CodeInvalidCountryCode, Message: "bad"}})

	api.do(t, postJSON(`{"ip":"81.2.69.142","allowed_countries":["ZZ"]}`))

	for _, r := range api.logs.Records() {
		for k, v := range r.Attrs {
			if s := v.String(); strings.Contains(s, "81.2.69.142") || strings.Contains(s, "ZZ") {
				t.Errorf("%q attr %s=%q leaks request data", r.Message, k, s)
			}
		}
	}
}

func TestAccessLog_decodeFailureHasNoRequestFields(t *testing.T) {
	// A body that never decoded has no allowlist; logging size 0 would read
	// as a real empty allowlist.
	api := newAPI(t, &fakeChecker{})
	api.do(t, postJSON(`not json`))

	rec := api.accessLog(t)
	for _, k := range []string{"allowlist_size", "client_ip_prefix", "decision"} {
		if _, ok := rec.Attrs[k]; ok {
			t.Errorf("%s logged for an undecodable request", k)
		}
	}
	if got := rec.Attrs["error_code"].String(); got != CodeMalformedJSON {
		t.Errorf("error_code=%q", got)
	}
}

func TestAccessLog_errorFields(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantLevel slog.Level
		wantCode  string
		wantCause bool
	}{
		{"client error stays at info", &countrycheck.ValidationError{Code: countrycheck.CodeInvalidIP, Message: "bad"}, slog.LevelInfo, countrycheck.CodeInvalidIP, false},
		{"client hang-up stays at info", context.Canceled, slog.LevelInfo, CodeClientClosedRequest, true},
		{"timeout is a server fault", context.DeadlineExceeded, slog.LevelError, CodeDeadlineExceeded, true},
		{"server error is logged with its cause", errors.New("boom"), slog.LevelError, CodeInternal, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newAPI(t, &fakeChecker{err: c.err})
			api.do(t, postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`))

			rec := api.accessLog(t)
			if rec.Level != c.wantLevel {
				t.Errorf("level=%v, want %v", rec.Level, c.wantLevel)
			}
			if got := rec.Attrs["error_code"].String(); got != c.wantCode {
				t.Errorf("error_code=%q, want %q", got, c.wantCode)
			}
			if _, ok := rec.Attrs["decision"]; ok {
				t.Error("decision must be absent when the check failed")
			}
			if _, got := rec.Attrs["error"]; got != c.wantCause {
				t.Errorf("error attr present=%v, want %v", got, c.wantCause)
			}
		})
	}
}

func TestMetrics_unmatchedRoutesShareOneOperationLabel(t *testing.T) {
	api := newAPI(t, &fakeChecker{})
	api.do(t, httptest.NewRequest(http.MethodGet, "/a", nil))
	api.do(t, httptest.NewRequest(http.MethodGet, "/b/c", nil))
	api.do(t, httptest.NewRequest(http.MethodGet, checkCountryPath, nil))

	if got := api.requestCount(t, apimetrics.OperationUnmatched, "404"); got != 2 {
		t.Errorf("unmatched 404 count=%d, want 2", got)
	}
	if got := api.requestCount(t, apimetrics.OperationUnmatched, "405"); got != 1 {
		t.Errorf("unmatched 405 count=%d, want 1", got)
	}
}

func TestRecover_panicBecomes500AndIsLogged(t *testing.T) {
	api := newAPI(t, &fakeChecker{panic: "kaboom"})
	req := postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`)
	req.Header.Set(requestIDHeader, "req-p")

	rec := api.do(t, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if p := decodeProblemBody(t, rec); p.Code != CodeInternal || p.RequestID != "req-p" {
		t.Errorf("problem=%+v", p)
	}
	panics := api.logs.Find("panic serving request")
	if len(panics) != 1 || panics[0].Attrs["stack"].String() == "" || panics[0].Attrs["request_id"].String() != "req-p" {
		t.Fatalf("want one panic log with stack and request_id, got %+v", panics)
	}
	if got := api.accessLog(t).Attrs["status"].Int64(); got != 500 {
		t.Errorf("access log status=%d, want 500", got)
	}
}

func TestRecover_panicAfterHeadersAbortsAndIsLogged(t *testing.T) {
	// Once the status is on the wire a 500 can't be sent. The response must
	// be aborted so the client can't mistake the partial body for a whole one.
	logs, logger := logtest.New()
	h := withAccessLog(logger, apimetrics.New(prometheus.NewRegistry()), withRecover(logger,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ip":`))
			panic("kaboom")
		})))

	v := recoverFrom(func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })

	// Identity, not errors.Is: net/http only suppresses the log for this exact value.
	if v != http.ErrAbortHandler { //nolint:errorlint // see above
		t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
	}
	access := logs.Find("http request")
	if len(access) != 1 {
		t.Fatalf("want one access log line, got %+v", logs.Records())
	}
	a := access[0]
	if a.Level != slog.LevelError || !a.Attrs["aborted"].Bool() || a.Attrs["status"].Int64() != 200 || a.Attrs["error"].String() != "panic: kaboom" {
		t.Errorf("access log=%+v", a.Attrs)
	}
}

func TestRecover_abortHandlerIsRepanickedAndLogged(t *testing.T) {
	api := newAPI(t, &fakeChecker{panic: http.ErrAbortHandler})

	v := recoverFrom(func() { api.do(t, postJSON(`{"ip":"81.2.69.142","allowed_countries":["GB"]}`)) })

	// Identity, not errors.Is: net/http only suppresses the log for this exact value.
	if v != http.ErrAbortHandler { //nolint:errorlint // see above
		t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
	}
	a := api.accessLog(t)
	if !a.Attrs["aborted"].Bool() {
		t.Errorf("aborted missing: %+v", a.Attrs)
	}
	if _, ok := a.Attrs["status"]; ok {
		t.Error("no status was sent, so none may be logged")
	}
	if got := api.requestCount(t, "check_country", codeAborted); got != 1 {
		t.Errorf("aborted request count=%d, want 1", got)
	}
}

func recoverFrom(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestUnknownFieldMessageFormat(t *testing.T) {
	// decodeProblem parses this text because encoding/json has no typed
	// error for it. If Go changes the wording, this fails first.
	dec := json.NewDecoder(strings.NewReader(`{"x":1}`))
	dec.DisallowUnknownFields()
	err := dec.Decode(&struct{}{})
	if err == nil || !strings.HasPrefix(err.Error(), unknownFieldPrefix) {
		t.Fatalf("got %v, want prefix %q", err, unknownFieldPrefix)
	}
}
