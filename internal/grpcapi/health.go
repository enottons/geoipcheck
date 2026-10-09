package grpcapi

import (
	"context"
	"slices"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
)

// health answers grpc.health.v1 from the same readiness check as /readyz,
// so the two can never disagree. grpc-go's health.Server is push-based and
// would need every readiness change mirrored into it.
//
// Watch stays unimplemented. The health spec says clients must then not
// retry it, and Kubernetes gRPC probes only call Check. A grpc-go client
// with client-side health checking treats Unimplemented as healthy. That is
// acceptable: readiness only changes at startup, before the port is in the
// Service, and at shutdown, where GracefulStop sends GOAWAY anyway.
type health struct {
	healthpb.UnimplementedHealthServer
	readiness Readiness
}

// services are the names Check answers for. "" is the whole server.
var services = []string{"", pb.CountryCheckService_ServiceDesc.ServiceName}

func (h *health) Check(_ context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if !slices.Contains(services, req.GetService()) {
		return nil, status.Errorf(codes.NotFound, "unknown service %q", req.GetService())
	}
	return &healthpb.HealthCheckResponse{Status: h.status()}, nil
}

func (h *health) List(context.Context, *healthpb.HealthListRequest) (*healthpb.HealthListResponse, error) {
	st := &healthpb.HealthCheckResponse{Status: h.status()}
	out := &healthpb.HealthListResponse{Statuses: map[string]*healthpb.HealthCheckResponse{}}
	for _, s := range services {
		out.Statuses[s] = st
	}
	return out, nil
}

func (h *health) status() healthpb.HealthCheckResponse_ServingStatus {
	if h.readiness.Check() != nil {
		return healthpb.HealthCheckResponse_NOT_SERVING
	}
	return healthpb.HealthCheckResponse_SERVING
}
