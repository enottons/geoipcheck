//go:build integration

package grpcapi_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
	"github.com/enottons/geoipcheck/internal/admin"
	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/geoip"
	"github.com/enottons/geoipcheck/internal/grpcapi"
	"github.com/enottons/geoipcheck/internal/testutil/logtest"
	"github.com/enottons/geoipcheck/internal/testutil/metricstest"
)

// These tests own what the unit tests can't show: the real gRPC transport,
// that status details and metadata survive the wire, the message size cap,
// and the real domain, reader and readiness behind it. Mapping rules are
// owned by server_test.go.

type stack struct {
	client    pb.CountryCheckServiceClient
	health    healthpb.HealthClient
	reader    *geoip.Reader
	readiness *admin.Readiness
	reg       *prometheus.Registry
	logs      *logtest.Handler
	conn      *grpc.ClientConn
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{reader: &geoip.Reader{}, reg: prometheus.NewRegistry()}
	s.readiness = admin.NewReadiness(func() bool { return s.reader.Current() != nil })
	logs, logger := logtest.New()
	s.logs = logs
	srv := grpcapi.NewServer(grpcapi.Config{
		Checker:   countrycheck.NewService(s.reader),
		Readiness: s.readiness,
		Metrics:   apimetrics.New(s.reg),
		Logger:    logger,
	})

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s.conn = conn
	s.client = pb.NewCountryCheckServiceClient(conn)
	s.health = healthpb.NewHealthClient(conn)
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

// accessLog waits for the call's one access log line. The server writes it
// on the stats End event, which can land after the client has its status.
func (s *stack) accessLog(t *testing.T) logtest.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logs := s.logs.Find("grpc request")
		if len(logs) == 1 {
			return logs[0]
		}
		if len(logs) > 1 || time.Now().After(deadline) {
			t.Fatalf("want one access log line, got %+v", s.logs.Records())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "x-request-id", "it-1")
}

func TestGRPCStack_checkAgainstRealDatabase(t *testing.T) {
	s := newStack(t)
	s.loadTestDB(t)

	resp, err := s.client.CheckCountry(callCtx(t),
		&pb.CheckCountryRequest{Ip: "81.2.69.142", AllowedCountries: []string{"us", "gb"}})

	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCountry() != "GB" || resp.GetDecision() != pb.Decision_DECISION_ALLOWED {
		t.Errorf("resp=%v", resp)
	}
	s.accessLog(t) // the call is finished server-side; metrics are final
	allowed := metricstest.Find(t, s.reg, "geoipcheck_checks_total",
		map[string]string{"transport": "grpc", "decision": "allowed", "reason": "country_allowed"}).GetCounter().GetValue()
	if allowed != 1 {
		t.Errorf("checks_total{grpc,allowed}=%v, want 1", allowed)
	}
}

func TestGRPCStack_errorDetailsAndRequestIDSurviveTheWire(t *testing.T) {
	s := newStack(t)
	s.loadTestDB(t)
	var header metadata.MD

	_, err := s.client.CheckCountry(callCtx(t),
		&pb.CheckCountryRequest{Ip: "81.2.69.142", AllowedCountries: []string{"UK"}}, grpc.Header(&header))

	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("status=%v", st)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if v, ok := d.(*errdetails.ErrorInfo); ok {
			info = v
		}
	}
	if info.GetReason() != countrycheck.CodeInvalidCountryCode || info.GetMetadata()["suggestion"] != "GB" {
		t.Errorf("ErrorInfo=%v", info)
	}
	if got := header.Get("x-request-id"); len(got) != 1 || got[0] != "it-1" {
		t.Errorf("x-request-id on error=%v, want [it-1]", got)
	}
}

func TestGRPCStack_emptyReaderIsUnavailable(t *testing.T) {
	// The chain geoip.ErrNotLoaded → countrycheck.ErrDatabaseUnavailable →
	// Unavailable only exists with the real reader and service.
	s := newStack(t)

	_, err := s.client.CheckCountry(callCtx(t), &pb.CheckCountryRequest{Ip: "81.2.69.142", AllowedCountries: []string{"GB"}})

	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code=%v, want Unavailable", got)
	}
}

func TestGRPCStack_oversizedRequestIsRejectedAndLogged(t *testing.T) {
	// grpc-go rejects this before any interceptor, so only the stats
	// handler sees it.
	s := newStack(t)
	s.loadTestDB(t)

	_, err := s.client.CheckCountry(callCtx(t), &pb.CheckCountryRequest{Ip: strings.Repeat("1", 16<<10)})

	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("code=%v, want ResourceExhausted", got)
	}
	rec := s.accessLog(t)
	if rec.Attrs["grpc_code"].String() != "ResourceExhausted" || rec.Attrs["request_id"].String() != "it-1" {
		t.Errorf("access log=%+v", rec.Attrs)
	}
}

func TestGRPCStack_unknownMethodIsLoggedAndEchoesRequestID(t *testing.T) {
	// Only the UnknownServiceHandler gives these calls a stats End event.
	s := newStack(t)
	var header metadata.MD

	err := s.conn.Invoke(callCtx(t), "/nope.v1.Nope/Do", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{}, grpc.Header(&header))

	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("code=%v, want Unimplemented", got)
	}
	if got := s.accessLog(t).Attrs["grpc_method"].String(); got != "/nope.v1.Nope/Do" {
		t.Errorf("grpc_method=%q", got)
	}
	if got := header.Get("x-request-id"); len(got) != 1 || got[0] != "it-1" {
		t.Errorf("x-request-id=%v, want [it-1]", got)
	}
}

func TestGRPCStack_healthFollowsReadiness(t *testing.T) {
	s := newStack(t)
	check := func() healthpb.HealthCheckResponse_ServingStatus {
		t.Helper()
		resp, err := s.health.Check(callCtx(t), &healthpb.HealthCheckRequest{Service: "geoipcheck.v1.CountryCheckService"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatus()
	}

	if got := check(); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("before load: %v, want NOT_SERVING", got)
	}
	s.loadTestDB(t)
	if got := check(); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("after load: %v, want SERVING", got)
	}
	s.readiness.StartDraining()
	if got := check(); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("draining: %v, want NOT_SERVING", got)
	}
}
