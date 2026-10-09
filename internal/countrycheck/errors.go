package countrycheck

import (
	"errors"
	"fmt"
	"time"
)

// ErrDatabaseUnavailable is returned when the GeoIP database is not loaded
// or cannot be read. Transport adapters map this to HTTP 503 and gRPC
// Unavailable.
var ErrDatabaseUnavailable = errors.New("geoip database unavailable")

// RetryAfterUnavailable is the retry hint both transports send with
// ErrDatabaseUnavailable. A replica without a DB is not ready, so the
// gateway's retry normally lands on another replica; this only bounds how
// hard it hammers when every replica is down.
const RetryAfterUnavailable = 5 * time.Second

// ValidationError describes a client input that failed validation. Each
// error has a stable machine-readable Code that is identical on HTTP and
// gRPC. Field names the offending field. Message is a short human-readable
// description. Suggestion, when set, carries a "did you mean" hint for a
// near-miss country code (for example "UK" → "GB"); callers may surface it.
type ValidationError struct {
	Field      string
	Code       string
	Message    string
	Suggestion string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
}

// Error codes shared across transports. The HTTP and gRPC adapters both
// return these verbatim as `code` fields so clients can switch on a stable
// value regardless of transport.
const (
	CodeInvalidIP               = "invalid_ip"
	CodeInvalidCountryCode      = "invalid_country_code"
	CodeAllowedCountriesMissing = "allowed_countries_required"
	CodeTooManyCountries        = "too_many_countries"

	// CodeDatabaseUnavailable accompanies ErrDatabaseUnavailable.
	CodeDatabaseUnavailable = "database_unavailable"
)
