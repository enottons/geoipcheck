package countrycheck

import (
	"context"
	"net/netip"
)

// Decision is the only authoritative outcome of a country check. The
// response has no boolean "allowed" field (see design decision D15): a
// second field could disagree with Decision, and documentation telling
// callers "when they disagree, trust Decision" is a trap. The gateway
// decides how to treat Decision=unknown per customer.
//
// Clients MUST treat an unrecognized Decision value as DecisionUnknown.
type Decision string

// Decision values.
const (
	DecisionAllowed Decision = "allowed"
	DecisionDenied  Decision = "denied"
	DecisionUnknown Decision = "unknown"
)

// Reason explains the Decision. It is machine-readable and stable; callers
// are expected to switch on it.
type Reason string

// Reason values.
const (
	ReasonCountryAllowed     Reason = "country_allowed"
	ReasonCountryNotAllowed  Reason = "country_not_allowed"
	ReasonNonRoutableAddress Reason = "non_routable_address"
	ReasonCountryUnresolved  Reason = "country_unresolved"
)

// Outcome is a Decision with the Reason that explains it.
type Outcome struct {
	Decision Decision
	Reason   Reason
}

// Outcomes returns every Decision/Reason pair Check can produce. It is the
// canonical list: metrics pre-create a series per pair, and the fuzz test
// fails if Check returns a pair that is missing here.
func Outcomes() []Outcome {
	return []Outcome{
		{DecisionAllowed, ReasonCountryAllowed},
		{DecisionDenied, ReasonCountryNotAllowed},
		{DecisionUnknown, ReasonNonRoutableAddress},
		{DecisionUnknown, ReasonCountryUnresolved},
	}
}

// Request is the input to a country check. IP is the IP string as the
// caller presented it (parsed inside the domain so validation errors can
// cite the raw value); AllowedCountries is the raw list, normalized and
// validated inside Service.Check.
type Request struct {
	IP               string
	AllowedCountries []string
}

// Result is the output of a country check. Country is the detected ISO
// 3166-1 alpha-2 code, or empty when the record had no country field or
// the IP was non-routable.
type Result struct {
	IP       netip.Addr
	Country  string
	Decision Decision
	Reason   Reason
}

// CountryResolver looks up the country ISO code for a routable IP. The
// returned string is empty when the record exists but has no country
// field (D13). A non-nil error means the resolver itself could not
// answer; the Service maps that to ErrDatabaseUnavailable.
type CountryResolver interface {
	Country(ctx context.Context, ip netip.Addr) (string, error)
}
