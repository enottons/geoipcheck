package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/requestid"
)

const requestIDHeader = "X-Request-ID"

// requestInfo is filled in by handlers and read by the access log. That is
// how one log line per request carries the decision without handlers
// logging themselves.
type requestInfo struct {
	operation     string
	decoded       bool // body parsed; allowlistSize and clientPrefix are set
	clientPrefix  string
	allowlistSize int
	result        *countrycheck.Result
	errCode       string
	errCause      error
}

type infoKey struct{}

func infoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(infoKey{}).(*requestInfo); ok {
		return info
	}
	// Only reachable if a handler runs without withAccessLog. A throwaway
	// keeps handlers free of nil checks.
	return &requestInfo{}
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestid.Resolve(r.Header.Get(requestIDHeader))
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(requestid.NewContext(r.Context(), id)))
	})
}

// withAccessLog writes one log line and one latency observation per
// request, including requests whose response was aborted. The client IP is
// logged only as its redacted prefix, and the allowlist only as its size.
func withAccessLog(logger *slog.Logger, metrics *apimetrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			// Only http.ErrAbortHandler reaches here; withRecover turns every
			// other panic into a response. Log it, then let net/http abort.
			aborted := recover()
			logRequest(logger, metrics, r, rec, info, time.Since(start), aborted != nil)
			if aborted != nil {
				panic(aborted)
			}
		}()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))
	})
}

// codeAborted is the metric code for a response that was cut off before a
// status was sent.
const codeAborted = "aborted"

func logRequest(logger *slog.Logger, metrics *apimetrics.Metrics, r *http.Request, rec *statusRecorder, info *requestInfo, elapsed time.Duration, aborted bool) {
	status := rec.statusOrOK()
	code := strconv.Itoa(status)
	if aborted && !rec.wroteHeader {
		code = codeAborted
	}
	operation := info.operation
	if operation == "" {
		operation = apimetrics.OperationUnmatched
	}
	metrics.Request(apimetrics.TransportHTTP, operation, code, elapsed)

	attrs := []slog.Attr{
		slog.String("request_id", requestid.FromContext(r.Context())),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Duration("duration", elapsed),
	}
	if code != codeAborted {
		attrs = append(attrs, slog.Int("status", status))
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
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	level := levelFor(status)
	if aborted {
		level = slog.LevelError
	}
	logger.LogAttrs(r.Context(), level, "http request", attrs...)
}

// levelFor keeps client mistakes at info so a misbehaving client cannot
// page anyone. Server faults are errors.
func levelFor(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelInfo
}

// withRecover turns a handler panic into a 500 problem. If the status was
// already sent, the response can't be fixed, so it is aborted instead:
// a client must never take a truncated body for a complete one.
func withRecover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// net/http uses this sentinel to abort a response on purpose.
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			cause := fmt.Errorf("panic: %v", v)
			logger.LogAttrs(r.Context(), slog.LevelError, "panic serving request",
				slog.String("request_id", requestid.FromContext(r.Context())),
				slog.String("panic", fmt.Sprint(v)),
				slog.String("stack", string(debug.Stack())))
			if rec, ok := w.(*statusRecorder); ok && rec.wroteHeader {
				infoFrom(r.Context()).errCause = cause
				panic(http.ErrAbortHandler)
			}
			writeProblem(w, r, internalProblem(cause))
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) statusOrOK() int {
	if !s.wroteHeader {
		return http.StatusOK
	}
	return s.status
}
