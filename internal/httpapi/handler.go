// Package httpapi is the HTTP adapter for countrycheck: POST
// /v1/country-checks with a JSON body (D3). Errors are RFC 9457
// problem+json with a stable code and the request ID.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/logredact"
)

// Checker is the domain call this adapter needs. *countrycheck.Service
// satisfies it.
type Checker interface {
	Check(ctx context.Context, req countrycheck.Request) (countrycheck.Result, error)
}

// Config wires a Handler. All fields are required.
type Config struct {
	Checker Checker
	Metrics *apimetrics.Metrics
	Logger  *slog.Logger
}

const checkCountryPath = "/v1/country-checks"

// NewHandler returns the public API handler with request-ID, access-log
// and panic-recovery middleware applied.
func NewHandler(cfg Config) http.Handler {
	h := &handler{checker: cfg.Checker, metrics: cfg.Metrics}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+checkCountryPath, h.checkCountry)
	// The method-less pattern only matches what the POST pattern doesn't,
	// so it turns the mux's plain-text 405 into problem+json.
	mux.HandleFunc(checkCountryPath, methodNotAllowed(http.MethodPost))
	// Unclean paths ("//v1/...") get ServeMux's plain 301 before any pattern
	// runs. Accepted: the gateway sends canonical paths.
	mux.HandleFunc("/", notFound)

	return withRequestID(withAccessLog(cfg.Logger, cfg.Metrics, withRecover(cfg.Logger, mux)))
}

type handler struct {
	checker Checker
	metrics *apimetrics.Metrics
}

type checkRequest struct {
	IP               string   `json:"ip"`
	AllowedCountries []string `json:"allowed_countries"`
}

// checkResponse always has every field. Country is null when no country
// was resolved, so clients see one stable shape.
type checkResponse struct {
	IP       string                `json:"ip"`
	Country  *string               `json:"country"`
	Decision countrycheck.Decision `json:"decision"`
	Reason   countrycheck.Reason   `json:"reason"`
}

func (h *handler) checkCountry(w http.ResponseWriter, r *http.Request) {
	info := infoFrom(r.Context())
	info.operation = apimetrics.OperationCheckCountry

	var req checkRequest
	if p := decodeJSON(w, r, &req); p != nil {
		writeProblem(w, r, p)
		return
	}
	info.decoded = true
	info.clientPrefix = logredact.PrefixString(req.IP)
	info.allowlistSize = len(req.AllowedCountries)

	res, err := h.checker.Check(r.Context(), countrycheck.Request{IP: req.IP, AllowedCountries: req.AllowedCountries})
	if err != nil {
		writeProblem(w, r, problemFor(err))
		return
	}
	h.metrics.Check(apimetrics.TransportHTTP, res)
	info.result = &res

	writeJSON(w, http.StatusOK, toResponse(res))
}

func toResponse(res countrycheck.Result) checkResponse {
	out := checkResponse{IP: addrString(res.IP), Decision: res.Decision, Reason: res.Reason}
	if res.Country != "" {
		out.Country = &res.Country
	}
	return out
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, &problem{
		Status: http.StatusNotFound,
		Code:   CodeNotFound,
		Detail: "no route for " + r.URL.Path,
	})
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeProblem(w, r, &problem{
			Status: http.StatusMethodNotAllowed,
			Code:   CodeMethodNotAllowed,
			Detail: r.Method + " is not supported; use " + allow,
		})
	}
}
