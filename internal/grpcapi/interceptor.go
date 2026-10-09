package grpcapi

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/enottons/geoipcheck/internal/requestid"
)

// unaryInterceptor puts the request ID on the context and the response
// header, and turns panics into Internal. Logging is accessLog's job.
func unaryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		info := infoFrom(ctx)
		id := info.requestID
		if id == "" {
			// Health calls aren't tracked by accessLog.
			id = requestid.Resolve(firstMetadata(ctx, requestIDKey))
		}
		ctx = requestid.NewContext(ctx, id)
		// SetHeader only fails if headers were already sent, which can't
		// happen before the handler runs.
		_ = grpc.SetHeader(ctx, metadata.Pairs(requestIDKey, id))

		defer func() {
			v := recover()
			if v == nil {
				return
			}
			logger.LogAttrs(ctx, slog.LevelError, "panic serving request",
				slog.String("request_id", id),
				slog.String("panic", fmt.Sprint(v)),
				slog.String("stack", string(debug.Stack())))
			resp, err = nil, info.fail(internalStatus(fmt.Errorf("panic: %v", v)))
		}()
		return handler(ctx, req)
	}
}

func firstMetadata(ctx context.Context, key string) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}
