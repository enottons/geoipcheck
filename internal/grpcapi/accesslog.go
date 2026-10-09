package grpcapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/requestid"
)

// requestIDKey is the metadata key for the request ID (D14). gRPC
// metadata keys are lowercase.
const requestIDKey = "x-request-id"

var (
	checkCountryMethod = "/" + pb.CountryCheckService_ServiceDesc.ServiceName + "/CheckCountry"

	// operations maps full method names to the operation label. Anything
	// else, including methods that don't exist, is OperationUnmatched.
	operations = map[string]string{checkCountryMethod: apimetrics.OperationCheckCountry}

	// healthMethodPrefix covers the health service. Probes call it every
	// few seconds; logging or timing them would drown out real traffic.
	healthMethodPrefix = "/" + healthpb.Health_ServiceDesc.ServiceName + "/"
)

// callInfo is filled in over the life of one call and read by the access
// log. It is the gRPC twin of httpapi's requestInfo.
type callInfo struct {
	method        string
	requestID     string
	decoded       bool // handler ran; allowlistSize and clientPrefix are set
	clientPrefix  string
	allowlistSize int
	result        *countrycheck.Result
	errCode       string
	errCause      error
}

// fail records e for the access log and returns the client-facing error.
func (c *callInfo) fail(e *callError) error {
	c.errCode, c.errCause = e.code, e.cause
	return e.status.Err()
}

type infoKey struct{}

// infoFrom returns the call's info, or a throwaway for health calls so
// handlers need no nil checks.
func infoFrom(ctx context.Context) *callInfo {
	if info, ok := ctx.Value(infoKey{}).(*callInfo); ok {
		return info
	}
	return &callInfo{}
}

// accessLog writes one log line and one latency observation per call. It
// is a stats.Handler, not an interceptor, because grpc-go rejects oversized
// and undecodable messages, and unknown methods, before any interceptor
// runs. Those calls are still logged and counted, as they are on HTTP.
type accessLog struct {
	logger  *slog.Logger
	metrics *apimetrics.Metrics
}

func (a *accessLog) TagRPC(ctx context.Context, tag *stats.RPCTagInfo) context.Context {
	if strings.HasPrefix(tag.FullMethodName, healthMethodPrefix) {
		return ctx
	}
	return context.WithValue(ctx, infoKey{}, &callInfo{method: tag.FullMethodName})
}

func (a *accessLog) HandleRPC(ctx context.Context, s stats.RPCStats) {
	info, ok := ctx.Value(infoKey{}).(*callInfo)
	if !ok {
		return
	}
	switch s := s.(type) {
	case *stats.InHeader:
		// The first server-side event, so every logged call has an ID, even
		// one rejected before the handler.
		var incoming string
		if v := s.Header.Get(requestIDKey); len(v) > 0 {
			incoming = v[0]
		}
		info.requestID = requestid.Resolve(incoming)
	case *stats.End:
		a.log(ctx, info, status.Code(s.Error), s.EndTime.Sub(s.BeginTime))
	}
}

func (*accessLog) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }

func (*accessLog) HandleConn(context.Context, stats.ConnStats) {}

func (a *accessLog) log(ctx context.Context, info *callInfo, code codes.Code, elapsed time.Duration) {
	operation, ok := operations[info.method]
	if !ok {
		operation = apimetrics.OperationUnmatched
	}
	// Metric before log: End can fire after the client has its status, so
	// tests wait for the log line and can then read the metric.
	a.metrics.Request(apimetrics.TransportGRPC, operation, code.String(), elapsed)

	// grpc_method and grpc_code, not method and code: the HTTP log uses
	// method for the verb, and both land in one index.
	attrs := []slog.Attr{
		slog.String("request_id", info.requestID),
		slog.String("grpc_method", info.method),
		slog.String("grpc_code", code.String()),
		slog.Duration("duration", elapsed),
	}
	if info.decoded {
		attrs = append(attrs, slog.Int("allowlist_size", info.allowlistSize))
		if info.clientPrefix != "" {
			attrs = append(attrs, slog.String("client_ip_prefix", info.clientPrefix))
		}
	}
	if res := info.result; res != nil {
		attrs = append(attrs,
			slog.String("country", res.Country),
			slog.String("decision", string(res.Decision)),
			slog.String("reason", string(res.Reason)))
	}
	if info.errCode != "" {
		attrs = append(attrs, slog.String("error_code", info.errCode))
	}
	if info.errCause != nil {
		attrs = append(attrs, slog.String("error", info.errCause.Error()))
	}
	a.logger.LogAttrs(ctx, levelFor(code), "grpc request", attrs...)
}

// levelFor mirrors httpapi: client mistakes stay at info so a misbehaving
// client cannot page anyone; server faults are errors.
func levelFor(c codes.Code) slog.Level {
	switch c {
	case codes.Internal, codes.Unavailable, codes.DeadlineExceeded, codes.Unknown, codes.DataLoss:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
