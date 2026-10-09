// Package countrycheck decides whether an IP address is in one of a
// caller-supplied set of allowed countries. It is transport-neutral: the
// HTTP and gRPC adapters both call Service.Check.
package countrycheck

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// MaxAllowedCountries caps the allowlist size. There are 250 accepted codes
// (249 assigned + XK), so a larger list is a caller bug, not a policy.
const MaxAllowedCountries = 250

// Service runs country checks against a CountryResolver.
type Service struct {
	resolver CountryResolver
}

// NewService returns a Service that resolves countries with r.
func NewService(r CountryResolver) *Service {
	return &Service{resolver: r}
}

// Check validates req and returns the decision for req.IP.
//
// Errors:
//   - *ValidationError for bad input. Nothing is looked up.
//   - an error wrapping ErrDatabaseUnavailable when the resolver fails.
//   - the context error when ctx is done while the resolver fails.
func (s *Service) Check(ctx context.Context, req Request) (Result, error) {
	addr, err := parseIP(req.IP)
	if err != nil {
		return Result{}, err
	}
	allowed, err := normalizeCountries(req.AllowedCountries)
	if err != nil {
		return Result{}, err
	}

	if isNonRoutable(addr) {
		return Result{IP: addr, Decision: DecisionUnknown, Reason: ReasonNonRoutableAddress}, nil
	}

	country, err := s.resolver.Country(ctx, addr)
	if err != nil {
		// A canceled or timed-out caller is not a database outage. Keep the
		// two apart so 503s and their alerts only fire for real outages.
		// Only one sentinel is wrapped (%w); the resolver's error is kept as
		// text (%v) so errors.Is never matches both categories.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, fmt.Errorf("country lookup: %w (resolver: %v)", ctxErr, err) //nolint:errorlint // see above
		}
		return Result{}, fmt.Errorf("%w: %v", ErrDatabaseUnavailable, err) //nolint:errorlint // see above
	}

	res := Result{IP: addr, Country: country}
	switch {
	case country == "":
		res.Decision, res.Reason = DecisionUnknown, ReasonCountryUnresolved
	case allowed[country]:
		res.Decision, res.Reason = DecisionAllowed, ReasonCountryAllowed
	default:
		res.Decision, res.Reason = DecisionDenied, ReasonCountryNotAllowed
	}
	return res, nil
}

// parseIP accepts a bare IPv4 or IPv6 address. It rejects the common
// mistakes a gateway makes when forwarding an address (host:port, zones,
// padding) with a message that names the mistake. IPv4-mapped IPv6 is
// unmapped so "::ffff:1.2.3.4" is checked as "1.2.3.4".
func parseIP(raw string) (netip.Addr, error) {
	invalid := func(msg string) error {
		return &ValidationError{Field: "ip", Code: CodeInvalidIP, Message: msg}
	}
	if raw == "" {
		return netip.Addr{}, invalid("ip is required")
	}
	if strings.TrimSpace(raw) != raw {
		return netip.Addr{}, invalid("ip must not contain leading or trailing whitespace")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		if _, perr := netip.ParseAddrPort(raw); perr == nil {
			return netip.Addr{}, invalid("ip must not include a port; send the address only")
		}
		return netip.Addr{}, invalid("ip is not a valid IPv4 or IPv6 address")
	}
	if addr.Zone() != "" {
		return netip.Addr{}, invalid("ip must not include an IPv6 zone")
	}
	return addr.Unmap(), nil
}

// normalizeCountries upper-cases, validates, and de-duplicates the
// allowlist. An empty list is an error, never "allow all": a missing
// allowlist is a caller bug, and treating it as open is a loophole.
func normalizeCountries(raw []string) (map[string]bool, error) {
	if len(raw) == 0 {
		return nil, &ValidationError{
			Field:   "allowed_countries",
			Code:    CodeAllowedCountriesMissing,
			Message: "allowed_countries must contain at least one country code",
		}
	}
	// Checked before de-duplication so the request size is bounded no
	// matter what the entries are.
	if len(raw) > MaxAllowedCountries {
		return nil, &ValidationError{
			Field:   "allowed_countries",
			Code:    CodeTooManyCountries,
			Message: fmt.Sprintf("allowed_countries must contain at most %d entries", MaxAllowedCountries),
		}
	}
	set := make(map[string]bool, len(raw))
	for i, c := range raw {
		code := asciiUpper(c)
		if !isAssignedCountry(code) {
			return nil, invalidCountry(i, c, code)
		}
		set[code] = true
	}
	return set, nil
}

// asciiUpper upper-cases ASCII letters only. strings.ToUpper is
// Unicode-aware and would fold "ſe" (long s) into "SE", so a non-ASCII
// input would pass validation.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

func invalidCountry(index int, raw, upper string) *ValidationError {
	ve := &ValidationError{
		Field:   fmt.Sprintf("allowed_countries[%d]", index),
		Code:    CodeInvalidCountryCode,
		Message: fmt.Sprintf("%q is not an ISO 3166-1 alpha-2 country code", raw),
	}
	if hint := hintFor(upper); hint != "" {
		ve.Suggestion = hint
		ve.Message += fmt.Sprintf("; did you mean %q?", hint)
	}
	return ve
}
