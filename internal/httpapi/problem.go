package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/requestid"
)

// Error codes owned by the HTTP adapter. Validation and database codes come
// from countrycheck and are identical on gRPC.
const (
	CodeMalformedJSON        = "malformed_json"
	CodeUnknownField         = "unknown_field"
	CodeRequestTooLarge      = "request_too_large"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeNotFound             = "not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeClientClosedRequest  = "client_closed_request"
	CodeDeadlineExceeded     = "deadline_exceeded"
	CodeInternal             = "internal"
)

// statusClientClosedRequest is the de facto (nginx) status for a client
// that went away. The client never sees it; it keeps these requests out of
// 5xx alerts.
const statusClientClosedRequest = 499

// problem is an RFC 9457 problem detail. Type is omitted, which means
// "about:blank", so Title is the HTTP status text.
type problem struct {
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Code       string `json:"code"`
	Field      string `json:"field,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
	RequestID  string `json:"request_id"`

	// cause goes to the access log only; it is never sent to the client.
	cause error
}

// problemFor maps an error from Checker.Check.
func problemFor(err error) *problem {
	var verr *countrycheck.ValidationError
	switch {
	case errors.As(err, &verr):
		return &problem{
			Status:     http.StatusBadRequest,
			Code:       verr.Code,
			Field:      verr.Field,
			Detail:     verr.Message,
			Suggestion: verr.Suggestion,
		}
	case errors.Is(err, countrycheck.ErrDatabaseUnavailable):
		return &problem{
			Status: http.StatusServiceUnavailable,
			Code:   countrycheck.CodeDatabaseUnavailable,
			Detail: "geoip database is not available; retry later",
			cause:  err,
		}
	case errors.Is(err, context.Canceled):
		return &problem{Status: statusClientClosedRequest, Code: CodeClientClosedRequest, cause: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &problem{Status: http.StatusGatewayTimeout, Code: CodeDeadlineExceeded, cause: err}
	default:
		return internalProblem(err)
	}
}

func internalProblem(cause error) *problem {
	return &problem{Status: http.StatusInternalServerError, Code: CodeInternal, Detail: "internal error", cause: cause}
}

func writeProblem(w http.ResponseWriter, r *http.Request, p *problem) {
	p.Title = http.StatusText(p.Status)
	if p.Status == statusClientClosedRequest {
		p.Title = "Client Closed Request"
	}
	p.RequestID = requestid.FromContext(r.Context())
	info := infoFrom(r.Context())
	info.errCode, info.errCause = p.Code, p.cause

	if p.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(int(countrycheck.RetryAfterUnavailable.Seconds())))
	}
	writeBody(w, "application/problem+json", p.Status, p)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeBody(w, jsonMediaType, status, v)
}

// writeBody encodes before writing headers so a marshal failure can still
// become a clean 500. The values written here are plain structs of strings
// and ints, so that branch is defensive.
func writeBody(w http.ResponseWriter, contentType string, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"Internal Server Error","status":500,"code":"internal"}` + "\n"))
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	// A write error means the client is gone; there is no one to tell.
	_, _ = w.Write(body)
}
