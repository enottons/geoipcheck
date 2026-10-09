package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/enottons/geoipcheck/internal/countrycheck"
)

// ErrorDomain is the ErrorInfo domain on every error detail this service
// sends.
const ErrorDomain = "geoipcheck"

// Metadata keys on ErrorInfo.
const (
	metadataField      = "field"
	metadataSuggestion = "suggestion"
)

// callError is a failed call: the status for the client, plus the code and
// cause for the access log. cause never reaches the client.
type callError struct {
	status *status.Status
	code   string
	cause  error
}

// callErrorFor maps an error from Checker.Check. ErrorInfo.reason is the same
// lowercase code the HTTP API returns. AIP-193 asks for UPPER_SNAKE, but a
// client switching on one code across both transports matters more.
func callErrorFor(err error) *callError {
	var verr *countrycheck.ValidationError
	switch {
	case errors.As(err, &verr):
		info := &errdetails.ErrorInfo{Reason: verr.Code, Domain: ErrorDomain}
		if verr.Field != "" || verr.Suggestion != "" {
			info.Metadata = map[string]string{}
			if verr.Field != "" {
				info.Metadata[metadataField] = verr.Field
			}
			if verr.Suggestion != "" {
				info.Metadata[metadataSuggestion] = verr.Suggestion
			}
		}
		details := []protoadapt.MessageV1{info}
		if verr.Field != "" {
			details = append(details, &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
				Field: verr.Field, Description: verr.Message, Reason: verr.Code,
			}}})
		}
		return &callError{status: withDetails(codes.InvalidArgument, verr.Message, details...), code: verr.Code}
	case errors.Is(err, countrycheck.ErrDatabaseUnavailable):
		st := withDetails(codes.Unavailable, "geoip database is not available; retry later",
			&errdetails.ErrorInfo{Reason: countrycheck.CodeDatabaseUnavailable, Domain: ErrorDomain},
			&errdetails.RetryInfo{RetryDelay: durationpb.New(countrycheck.RetryAfterUnavailable)})
		return &callError{status: st, code: countrycheck.CodeDatabaseUnavailable, cause: err}
	case errors.Is(err, context.Canceled):
		return &callError{status: status.New(codes.Canceled, "request cancelled"), cause: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &callError{status: status.New(codes.DeadlineExceeded, "deadline exceeded"), cause: err}
	default:
		return internalStatus(err)
	}
}

func internalStatus(cause error) *callError {
	return &callError{status: status.New(codes.Internal, "internal error"), cause: cause}
}

// withDetails attaches details. It only fails when a detail can't be
// marshaled, which can't happen for these well-known types; the status
// without details is still correct.
func withDetails(c codes.Code, msg string, details ...protoadapt.MessageV1) *status.Status {
	st := status.New(c, msg)
	if withD, err := st.WithDetails(details...); err == nil {
		return withD
	}
	return st
}
