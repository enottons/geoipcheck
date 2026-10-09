package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
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

// fakeStream stands in for the transport so grpc.SetHeader works without a
// connection.
type fakeStream struct {
	grpc.ServerTransportStream
	header metadata.MD
}

func (s *fakeStream) SetHeader(md metadata.MD) error {
	s.header = metadata.Join(s.header, md)
	return nil
}

type harness struct {
	checker     *fakeChecker
	svc         *service
	stats       *accessLog
	interceptor grpc.UnaryServerInterceptor
	logs        *logtest.Handler
	reg         *prometheus.Registry
}

func newHarness(checker *fakeChecker) *harness {
	logs, logger := logtest.New()
	reg := prometheus.NewRegistry()
	metrics := apimetrics.New(reg)
	return &harness{
		checker:     checker,
		svc:         &service{checker: checker, metrics: metrics},
		stats:       &accessLog{logger: logger, metrics: metrics},
		interceptor: unaryInterceptor(logger),
		logs:        logs,
		reg:         reg,
	}
}

// requestDuration is the latency family; one name so a typo can't make a
// "no series" assertion pass vacuously.
const requestDuration = "geoipcheck_request_duration_seconds"

// invoke drives one call the way grpc-go does for a registered unary
// method: TagRPC, InHeader, interceptor and handler, End. requestID, when
// set, is sent as incoming metadata.
func (h *harness) invoke(method string, req any, requestID string, handler grpc.UnaryHandler) (any, metadata.MD, error) {
	ctx, stream, end := h.begin(method, requestID)
	resp, err := h.interceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, handler)
	end(err)
	return resp, stream.header, err
}

// reject drives a call grpc-go fails before the interceptor: an oversized
// message, or an unknown method. Only the stats handler sees it.
func (h *harness) reject(method, requestID string, err error) {
	_, _, end := h.begin(method, requestID)
	end(err)
}

func (h *harness) begin(method, requestID string) (context.Context, *fakeStream, func(error)) {
	md := metadata.MD{}
	if requestID != "" {
		md = metadata.Pairs(requestIDKey, requestID)
	}
	stream := &fakeStream{}
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ctx = grpc.NewContextWithServerTransportStream(ctx, stream)
	ctx = h.stats.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: method})
	start := time.Now()
	h.stats.HandleRPC(ctx, &stats.InHeader{FullMethod: method, Header: md})
	return ctx, stream, func(err error) {
		h.stats.HandleRPC(ctx, &stats.End{BeginTime: start, EndTime: time.Now(), Error: err})
	}
}

func (h *harness) call(t *testing.T, req *pb.CheckCountryRequest, requestID string) (*pb.CheckCountryResponse, metadata.MD, error) {
	t.Helper()
	resp, header, err := h.invoke(checkCountryMethod, req, requestID, func(ctx context.Context, req any) (any, error) {
		return h.svc.CheckCountry(ctx, req.(*pb.CheckCountryRequest))
	})
	out, _ := resp.(*pb.CheckCountryResponse)
	return out, header, err
}

func (h *harness) accessLog(t *testing.T) logtest.Record {
	t.Helper()
	logs := h.logs.Find("grpc request")
	if len(logs) != 1 {
		t.Fatalf("want one access log line, got %+v", h.logs.Records())
	}
	return logs[0]
}

func (h *harness) requestCount(t *testing.T, operation, code string) uint64 {
	t.Helper()
	return metricstest.Find(t, h.reg, requestDuration,
		map[string]string{"transport": "grpc", "operation": operation, "code": code}).GetHistogram().GetSampleCount()
}

var (
	gbRequest = &pb.CheckCountryRequest{Ip: "81.2.69.142", AllowedCountries: []string{"GB", "us"}}
	gbAllowed = countrycheck.Result{
		IP:       netip.MustParseAddr("81.2.69.142"),
		Country:  "GB",
		Decision: countrycheck.DecisionAllowed,
		Reason:   countrycheck.ReasonCountryAllowed,
	}
)

func TestCheckCountry_success(t *testing.T) {
	h := newHarness(&fakeChecker{res: gbAllowed})

	resp, _, err := h.call(t, gbRequest, "")

	if err != nil {
		t.Fatal(err)
	}
	want := &pb.CheckCountryResponse{
		Ip: "81.2.69.142", Country: "GB",
		Decision: pb.Decision_DECISION_ALLOWED, Reason: pb.Reason_REASON_COUNTRY_ALLOWED,
	}
	if diff := cmp.Diff(want, resp, protocmp.Transform()); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	wantReq := []countrycheck.Request{{IP: "81.2.69.142", AllowedCountries: []string{"GB", "us"}}}
	if diff := cmp.Diff(wantReq, h.checker.got); diff != "" {
		t.Errorf("request passed to checker (-want +got):\n%s", diff)
	}
}

func TestToProto_everyOutcomeHasAMapping(t *testing.T) {
	for _, o := range countrycheck.Outcomes() {
		resp, err := toProto(countrycheck.Result{Decision: o.Decision, Reason: o.Reason})
		if err != nil {
			t.Errorf("%v: %v", o, err)
			continue
		}
		if resp.GetDecision() == pb.Decision_DECISION_UNSPECIFIED || resp.GetReason() == pb.Reason_REASON_UNSPECIFIED {
			t.Errorf("%v mapped to an unspecified value: %v", o, resp)
		}
	}
}

func TestCheckCountry_unmappedOutcomeIsInternal(t *testing.T) {
	// Sending DECISION_UNSPECIFIED would look like a valid answer.
	h := newHarness(&fakeChecker{res: countrycheck.Result{Decision: "maybe", Reason: countrycheck.ReasonCountryAllowed}})

	_, _, err := h.call(t, gbRequest, "")

	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code=%v, want Internal", got)
	}
	if got := h.accessLog(t).Attrs["error"].String(); !strings.Contains(got, `"maybe"`) {
		t.Errorf("error attr=%q, want the unmapped value", got)
	}
}

func TestCheckCountry_statusMapping(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantCode    codes.Code
		wantMessage string
		wantDetails []any
	}{
		{
			name: "validation error carries code, field and suggestion",
			err: &countrycheck.ValidationError{
				Field: "allowed_countries[0]", Code: countrycheck.CodeInvalidCountryCode,
				Message: `"UK" is not an ISO 3166-1 alpha-2 code; did you mean "GB"?`, Suggestion: "GB",
			},
			wantCode:    codes.InvalidArgument,
			wantMessage: `"UK" is not an ISO 3166-1 alpha-2 code; did you mean "GB"?`,
			wantDetails: []any{
				&errdetails.ErrorInfo{
					Reason: "invalid_country_code", Domain: ErrorDomain,
					Metadata: map[string]string{"field": "allowed_countries[0]", "suggestion": "GB"},
				},
				&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
					Field: "allowed_countries[0]", Reason: "invalid_country_code",
					Description: `"UK" is not an ISO 3166-1 alpha-2 code; did you mean "GB"?`,
				}}},
			},
		},
		{
			name:        "validation error without a field has no BadRequest",
			err:         &countrycheck.ValidationError{Code: countrycheck.CodeAllowedCountriesMissing, Message: "required"},
			wantCode:    codes.InvalidArgument,
			wantMessage: "required",
			wantDetails: []any{&errdetails.ErrorInfo{Reason: "allowed_countries_required", Domain: ErrorDomain}},
		},
		{
			name:        "database unavailable carries a retry delay",
			err:         fmt.Errorf("%w: not loaded", countrycheck.ErrDatabaseUnavailable),
			wantCode:    codes.Unavailable,
			wantMessage: "geoip database is not available; retry later",
			wantDetails: []any{
				&errdetails.ErrorInfo{Reason: "database_unavailable", Domain: ErrorDomain},
				&errdetails.RetryInfo{RetryDelay: durationpb.New(5 * time.Second)},
			},
		},
		{
			name:        "client went away",
			err:         fmt.Errorf("lookup: %w", context.Canceled),
			wantCode:    codes.Canceled,
			wantMessage: "request cancelled",
		},
		{
			name:        "deadline exceeded",
			err:         fmt.Errorf("lookup: %w", context.DeadlineExceeded),
			wantCode:    codes.DeadlineExceeded,
			wantMessage: "deadline exceeded",
		},
		{
			name:        "anything else is internal and not leaked",
			err:         errors.New("secret internal detail"),
			wantCode:    codes.Internal,
			wantMessage: "internal error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(&fakeChecker{err: c.err})

			_, _, err := h.call(t, gbRequest, "")

			st := status.Convert(err)
			if st.Code() != c.wantCode || st.Message() != c.wantMessage {
				t.Errorf("status=%v %q, want %v %q", st.Code(), st.Message(), c.wantCode, c.wantMessage)
			}
			if diff := cmp.Diff(c.wantDetails, st.Details(), protocmp.Transform(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("details (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRequestID_echoedWhenValidGeneratedOtherwise(t *testing.T) {
	h := newHarness(&fakeChecker{res: gbAllowed})

	_, header, _ := h.call(t, gbRequest, "gw-abc-123")
	if got := header.Get(requestIDKey); len(got) != 1 || got[0] != "gw-abc-123" {
		t.Errorf("echoed ID=%v, want [gw-abc-123]", got)
	}

	_, header, _ = h.call(t, gbRequest, "bad id with spaces")
	if got := header.Get(requestIDKey); len(got) != 1 || got[0] == "" || strings.Contains(got[0], " ") {
		t.Errorf("replacement ID=%v, want one generated ID", got)
	}
}

func TestAccessLog_successFields(t *testing.T) {
	h := newHarness(&fakeChecker{res: gbAllowed})

	if _, _, err := h.call(t, gbRequest, "req-7"); err != nil {
		t.Fatal(err)
	}

	rec := h.accessLog(t)
	if rec.Level != slog.LevelInfo {
		t.Errorf("level=%v, want info", rec.Level)
	}
	want := map[string]string{
		"request_id":       "req-7",
		"grpc_method":      checkCountryMethod,
		"grpc_code":        "OK",
		"client_ip_prefix": "81.2.69.0/24",
		"allowlist_size":   "2",
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
	if got := h.requestCount(t, apimetrics.OperationCheckCountry, "OK"); got != 1 {
		t.Errorf("request count{OK}=%d, want 1", got)
	}
}

func TestAccessLog_neverContainsRawIPOrAllowlist(t *testing.T) {
	h := newHarness(&fakeChecker{err: &countrycheck.ValidationError{Code: countrycheck.CodeInvalidCountryCode, Message: "bad"}})

	_, _, _ = h.call(t, &pb.CheckCountryRequest{Ip: "81.2.69.142", AllowedCountries: []string{"ZZ"}}, "")

	for _, r := range h.logs.Records() {
		for k, v := range r.Attrs {
			if s := v.String(); strings.Contains(s, "81.2.69.142") || strings.Contains(s, "ZZ") {
				t.Errorf("%q attr %s=%q leaks request data", r.Message, k, s)
			}
		}
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
		{"client hang-up stays at info", context.Canceled, slog.LevelInfo, "", true},
		{"timeout is a server fault", context.DeadlineExceeded, slog.LevelError, "", true},
		{"unavailable is a server fault", countrycheck.ErrDatabaseUnavailable, slog.LevelError, countrycheck.CodeDatabaseUnavailable, true},
		{"server error is logged with its cause", errors.New("boom"), slog.LevelError, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(&fakeChecker{err: c.err})
			_, _, _ = h.call(t, gbRequest, "")

			rec := h.accessLog(t)
			if rec.Level != c.wantLevel {
				t.Errorf("level=%v, want %v", rec.Level, c.wantLevel)
			}
			// Only codes the client also sees in ErrorInfo are logged; the
			// gRPC code attr covers the rest.
			got, ok := rec.Attrs["error_code"]
			if c.wantCode == "" && ok {
				t.Errorf("error_code=%q, want none", got)
			} else if c.wantCode != "" && got.String() != c.wantCode {
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

func TestRecover_panicBecomesInternalAndIsLogged(t *testing.T) {
	h := newHarness(&fakeChecker{panic: "kaboom"})

	_, _, err := h.call(t, gbRequest, "req-p")

	if st := status.Convert(err); st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("status=%v", st)
	}
	panics := h.logs.Find("panic serving request")
	if len(panics) != 1 || panics[0].Attrs["stack"].String() == "" || panics[0].Attrs["request_id"].String() != "req-p" {
		t.Fatalf("want one panic log with stack and request_id, got %+v", panics)
	}
	a := h.accessLog(t)
	if a.Attrs["grpc_code"].String() != "Internal" || a.Attrs["error"].String() != "panic: kaboom" {
		t.Errorf("access log=%+v", a.Attrs)
	}
}

type fakeReadiness struct{ err error }

func (f *fakeReadiness) Check() error { return f.err }

func TestHealth_followsReadiness(t *testing.T) {
	ready := &fakeReadiness{}
	hs := &health{readiness: ready}
	ctx := context.Background()

	for _, svc := range services {
		resp, err := hs.Check(ctx, &healthpb.HealthCheckRequest{Service: svc})
		if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("Check(%q) while ready = %v, %v; want SERVING", svc, resp, err)
		}
	}

	ready.err = errors.New("shutting down")
	resp, err := hs.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("Check while not ready = %v, %v; want NOT_SERVING", resp, err)
	}
	list, err := hs.List(ctx, &healthpb.HealthListRequest{})
	if err != nil || len(list.GetStatuses()) != len(services) {
		t.Fatalf("List = %v, %v", list, err)
	}
	for name, st := range list.GetStatuses() {
		if st.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
			t.Errorf("List[%q]=%v, want NOT_SERVING", name, st.GetStatus())
		}
	}
}

func TestHealth_unknownServiceIsNotFound(t *testing.T) {
	hs := &health{readiness: &fakeReadiness{}}
	_, err := hs.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "nope.v1.Nope"})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code=%v, want NotFound", got)
	}
}

func TestAccessLog_healthCallsAreNotLoggedOrTimed(t *testing.T) {
	h := newHarness(&fakeChecker{})
	hs := &health{readiness: &fakeReadiness{}}

	_, _, err := h.invoke(healthMethodPrefix+"Check", &healthpb.HealthCheckRequest{}, "",
		func(ctx context.Context, req any) (any, error) {
			return hs.Check(ctx, req.(*healthpb.HealthCheckRequest))
		})

	if err != nil {
		t.Fatal(err)
	}
	if recs := h.logs.Records(); len(recs) != 0 {
		t.Errorf("health call logged: %+v", recs)
	}
	if n := metricstest.Count(t, h.reg, requestDuration); n != 0 {
		t.Errorf("health call timed: %d series", n)
	}
}

func TestAccessLog_callRejectedBeforeHandlerIsStillLogged(t *testing.T) {
	// grpc-go rejects oversized and undecodable messages before the
	// handler. The call has an ID and a code but no request fields.
	h := newHarness(&fakeChecker{})

	h.reject(checkCountryMethod, "req-r", status.Error(codes.ResourceExhausted, "message too large"))

	rec := h.accessLog(t)
	if rec.Attrs["request_id"].String() != "req-r" || rec.Attrs["grpc_code"].String() != "ResourceExhausted" {
		t.Errorf("access log=%+v", rec.Attrs)
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("level=%v, want info: the client sent a bad message", rec.Level)
	}
	if _, ok := rec.Attrs["allowlist_size"]; ok {
		t.Error("allowlist_size logged for a call the handler never saw")
	}
	if got := h.requestCount(t, apimetrics.OperationCheckCountry, "ResourceExhausted"); got != 1 {
		t.Errorf("request count{ResourceExhausted}=%d, want 1", got)
	}
}

func TestAccessLog_unknownMethodSharesOneOperationLabel(t *testing.T) {
	h := newHarness(&fakeChecker{})
	unimplemented := status.Error(codes.Unimplemented, "")

	h.reject("/nope.v1.Nope/A", "", unimplemented)
	h.reject("/nope.v1.Nope/B", "", unimplemented)

	if got := h.requestCount(t, apimetrics.OperationUnmatched, "Unimplemented"); got != 2 {
		t.Errorf("unmatched count=%d, want 2", got)
	}
}
