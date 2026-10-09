// Package grpcapi is the gRPC adapter for countrycheck:
// geoipcheck.v1.CountryCheckService/CheckCountry (D5). Validation failures
// carry the same codes as the HTTP API, in an ErrorInfo detail.
package grpcapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/logredact"
)

// Checker is the domain call this adapter needs. *countrycheck.Service
// satisfies it.
type Checker interface {
	Check(ctx context.Context, req countrycheck.Request) (countrycheck.Result, error)
}

// Readiness reports whether this replica should take traffic. A nil error
// means ready. *admin.Readiness satisfies it.
type Readiness interface {
	Check() error
}

// Config wires a Server. All fields are required.
type Config struct {
	Checker   Checker
	Readiness Readiness
	Metrics   *apimetrics.Metrics
	Logger    *slog.Logger
}

// maxRecvMsgBytes matches the HTTP body limit. A valid request is well
// under 2 KiB; the 4 MiB gRPC default would only let clients waste memory.
const maxRecvMsgBytes = 8 << 10

// Connection lifetime. The gateway holds long-lived HTTP/2 connections, so
// without a max age, replicas added by the autoscaler would get no gRPC
// traffic until something broke the old connections. The grace period
// lets in-flight calls finish.
const (
	maxConnectionAge      = 5 * time.Minute
	maxConnectionAgeGrace = 30 * time.Second
)

// minClientPingInterval is the fastest keepalive ping accepted. The 5
// minute default would make the server close connections from any client
// that pings more often than that.
const minClientPingInterval = 10 * time.Second

// NewServer returns a gRPC server with CountryCheckService and the
// standard health service registered. The caller owns Serve and Stop.
func NewServer(cfg Config) *grpc.Server {
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxRecvMsgBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      maxConnectionAge,
			MaxConnectionAgeGrace: maxConnectionAgeGrace,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             minClientPingInterval,
			PermitWithoutStream: true,
		}),
		grpc.StatsHandler(&accessLog{logger: cfg.Logger, metrics: cfg.Metrics}),
		// Without this, grpc-go answers unknown methods without a stats End
		// event, so they would be neither logged nor counted.
		grpc.UnknownServiceHandler(unknownMethod),
		// Every registered RPC is unary. A streaming RPC would need a
		// matching ChainStreamInterceptor for the request ID and panic
		// recovery. unknownMethod is a stream handler but can't panic and
		// sets its own header.
		grpc.ChainUnaryInterceptor(unaryInterceptor(cfg.Logger)),
	)
	pb.RegisterCountryCheckServiceServer(srv, &service{checker: cfg.Checker, metrics: cfg.Metrics})
	healthpb.RegisterHealthServer(srv, &health{readiness: cfg.Readiness})
	return srv
}

func unknownMethod(_ any, stream grpc.ServerStream) error {
	// No interceptor runs here, so echo the request ID the stats handler
	// already resolved. A failed SetHeader only loses the echo.
	_ = stream.SetHeader(metadata.Pairs(requestIDKey, infoFrom(stream.Context()).requestID))
	method, _ := grpc.MethodFromServerStream(stream)
	return status.Errorf(codes.Unimplemented, "unknown method %s", method)
}

type service struct {
	pb.UnimplementedCountryCheckServiceServer
	checker Checker
	metrics *apimetrics.Metrics
}

func (s *service) CheckCountry(ctx context.Context, req *pb.CheckCountryRequest) (*pb.CheckCountryResponse, error) {
	info := infoFrom(ctx)
	info.decoded = true
	info.clientPrefix = logredact.PrefixString(req.GetIp())
	info.allowlistSize = len(req.GetAllowedCountries())

	res, err := s.checker.Check(ctx, countrycheck.Request{IP: req.GetIp(), AllowedCountries: req.GetAllowedCountries()})
	if err != nil {
		return nil, info.fail(callErrorFor(err))
	}
	resp, err := toProto(res)
	if err != nil {
		return nil, info.fail(internalStatus(err))
	}
	s.metrics.Check(apimetrics.TransportGRPC, res)
	info.result = &res
	return resp, nil
}

var (
	decisions = map[countrycheck.Decision]pb.Decision{
		countrycheck.DecisionAllowed: pb.Decision_DECISION_ALLOWED,
		countrycheck.DecisionDenied:  pb.Decision_DECISION_DENIED,
		countrycheck.DecisionUnknown: pb.Decision_DECISION_UNKNOWN,
	}
	reasons = map[countrycheck.Reason]pb.Reason{
		countrycheck.ReasonCountryAllowed:     pb.Reason_REASON_COUNTRY_ALLOWED,
		countrycheck.ReasonCountryNotAllowed:  pb.Reason_REASON_COUNTRY_NOT_ALLOWED,
		countrycheck.ReasonNonRoutableAddress: pb.Reason_REASON_NON_ROUTABLE_ADDRESS,
		countrycheck.ReasonCountryUnresolved:  pb.Reason_REASON_COUNTRY_UNRESOLVED,
	}
)

// toProto fails rather than sending DECISION_UNSPECIFIED: a domain value
// without a proto mapping is a bug here, not something to hide from the
// caller as a valid-looking answer.
func toProto(res countrycheck.Result) (*pb.CheckCountryResponse, error) {
	d, ok := decisions[res.Decision]
	if !ok {
		return nil, fmt.Errorf("no proto mapping for decision %q", res.Decision)
	}
	r, ok := reasons[res.Reason]
	if !ok {
		return nil, fmt.Errorf("no proto mapping for reason %q", res.Reason)
	}
	return &pb.CheckCountryResponse{Ip: addrString(res.IP), Country: res.Country, Decision: d, Reason: r}, nil
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}
