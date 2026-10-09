package countrycheck

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeResolver returns a fixed country per IP, or err for every lookup.
type fakeResolver struct {
	countries map[string]string
	err       error
	calls     int
}

func (f *fakeResolver) Country(_ context.Context, ip netip.Addr) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.countries[ip.String()], nil
}

func newService(r CountryResolver) *Service { return NewService(r) }

func TestCheck_allowedWhenCountryInList(t *testing.T) {
	svc := newService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	got, err := svc.Check(context.Background(), Request{IP: "81.2.69.142", AllowedCountries: []string{"US", "GB"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Result{IP: netip.MustParseAddr("81.2.69.142"), Country: "GB", Decision: DecisionAllowed, Reason: ReasonCountryAllowed}
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
		t.Fatalf("result mismatch (-want +got):\n%s", diff)
	}
}

func TestCheck_deniedWhenCountryNotInList(t *testing.T) {
	svc := newService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	got, err := svc.Check(context.Background(), Request{IP: "81.2.69.142", AllowedCountries: []string{"US"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionDenied || got.Reason != ReasonCountryNotAllowed || got.Country != "GB" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_unknownWhenRecordHasNoCountry(t *testing.T) {
	// Resolver returns "" for an IP it has a record for but no country (D13).
	svc := newService(&fakeResolver{countries: map[string]string{}})
	got, err := svc.Check(context.Background(), Request{IP: "8.8.8.8", AllowedCountries: []string{"US"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionUnknown || got.Reason != ReasonCountryUnresolved || got.Country != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_unknownForNonRoutableSkipsResolver(t *testing.T) {
	// Which prefixes are non-routable is owned by specialaddr_test.go.
	// This test only proves Check short-circuits on them.
	r := &fakeResolver{}
	svc := newService(r)
	got, err := svc.Check(context.Background(), Request{IP: "10.0.0.1", AllowedCountries: []string{"US"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionUnknown || got.Reason != ReasonNonRoutableAddress || got.Country != "" {
		t.Fatalf("got %+v", got)
	}
	if r.calls != 0 {
		t.Fatalf("resolver called %d times for non-routable addresses; want 0", r.calls)
	}
}

func TestCheck_resolverErrorMapsToDatabaseUnavailable(t *testing.T) {
	svc := newService(&fakeResolver{err: errors.New("disk on fire")})
	_, err := svc.Check(context.Background(), Request{IP: "8.8.8.8", AllowedCountries: []string{"US"}})
	if !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("err=%v, want ErrDatabaseUnavailable", err)
	}
	// The cause must stay in the chain so logs can show it.
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err=%v lost its cause", err)
	}
}

func TestCheck_unmapsIPv4MappedIPv6(t *testing.T) {
	r := &fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}}
	svc := newService(r)
	got, err := svc.Check(context.Background(), Request{IP: "::ffff:81.2.69.142", AllowedCountries: []string{"GB"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.IP.String() != "81.2.69.142" || got.Decision != DecisionAllowed {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_mappedPrivateIsNonRoutable(t *testing.T) {
	svc := newService(&fakeResolver{})
	got, err := svc.Check(context.Background(), Request{IP: "::ffff:10.0.0.1", AllowedCountries: []string{"US"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != ReasonNonRoutableAddress {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_normalizesCountryCase(t *testing.T) {
	svc := newService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	got, err := svc.Check(context.Background(), Request{IP: "81.2.69.142", AllowedCountries: []string{"gb"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != DecisionAllowed {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_duplicateCountriesAccepted(t *testing.T) {
	svc := newService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	_, err := svc.Check(context.Background(), Request{IP: "81.2.69.142", AllowedCountries: []string{"GB", "gb", "GB"}})
	if err != nil {
		t.Fatalf("duplicates should be de-duplicated, not rejected: %v", err)
	}
}

func TestCheck_validationErrors(t *testing.T) {
	tooMany := make([]string, MaxAllowedCountries+1)
	for i := range tooMany {
		tooMany[i] = "US"
	}
	cases := []struct {
		name       string
		req        Request
		code       string
		field      string
		suggestion string
		msgHas     string
	}{
		{"empty ip", Request{IP: "", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", ""},
		{"garbage ip", Request{IP: "not-an-ip", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", ""},
		{"ipv4 with port", Request{IP: "1.2.3.4:443", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", "port"},
		{"ipv6 with port", Request{IP: "[2001:db8::1]:443", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", "port"},
		{"ipv6 zone", Request{IP: "fe80::1%eth0", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", "zone"},
		{"leading space", Request{IP: " 1.2.3.4", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", "whitespace"},
		{"trailing space", Request{IP: "1.2.3.4 ", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", "whitespace"},
		{"cidr", Request{IP: "1.2.3.0/24", AllowedCountries: []string{"US"}}, CodeInvalidIP, "ip", "", ""},
		{"nil countries", Request{IP: "1.2.3.4"}, CodeAllowedCountriesMissing, "allowed_countries", "", ""},
		{"empty countries", Request{IP: "1.2.3.4", AllowedCountries: []string{}}, CodeAllowedCountriesMissing, "allowed_countries", "", ""},
		{"too many", Request{IP: "1.2.3.4", AllowedCountries: tooMany}, CodeTooManyCountries, "allowed_countries", "", ""},
		// Hint contents are owned by iso3166_test.go; these prove a hint is
		// looked up on the upper-cased value and surfaced in the error.
		{"hint surfaced", Request{IP: "1.2.3.4", AllowedCountries: []string{"USA"}}, CodeInvalidCountryCode, "allowed_countries[0]", "US", `did you mean "US"`},
		{"hint after upper-casing", Request{IP: "1.2.3.4", AllowedCountries: []string{"GB", "usa"}}, CodeInvalidCountryCode, "allowed_countries[1]", "US", ""},
		{"unassigned", Request{IP: "1.2.3.4", AllowedCountries: []string{"ZZ"}}, CodeInvalidCountryCode, "allowed_countries[0]", "", ""},
		{"empty code", Request{IP: "1.2.3.4", AllowedCountries: []string{""}}, CodeInvalidCountryCode, "allowed_countries[0]", "", ""},
		{"padded code", Request{IP: "1.2.3.4", AllowedCountries: []string{" US"}}, CodeInvalidCountryCode, "allowed_countries[0]", "", ""},
		{"non-ascii code", Request{IP: "1.2.3.4", AllowedCountries: []string{"ÜS"}}, CodeInvalidCountryCode, "allowed_countries[0]", "", ""},
		// strings.ToUpper would fold U+017F (long s) to "S" and accept "SE".
		{"unicode case-fold", Request{IP: "1.2.3.4", AllowedCountries: []string{"ſe"}}, CodeInvalidCountryCode, "allowed_countries[0]", "", ""},
	}
	r := &fakeResolver{}
	svc := newService(r)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Check(context.Background(), c.req)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err=%v, want *ValidationError", err)
			}
			if ve.Code != c.code || ve.Field != c.field || ve.Suggestion != c.suggestion {
				t.Fatalf("got {code:%q field:%q suggestion:%q}, want {code:%q field:%q suggestion:%q}",
					ve.Code, ve.Field, ve.Suggestion, c.code, c.field, c.suggestion)
			}
			if c.msgHas != "" && !strings.Contains(ve.Message, c.msgHas) {
				t.Fatalf("message %q does not mention %q", ve.Message, c.msgHas)
			}
		})
	}
	if r.calls != 0 {
		t.Fatalf("resolver called %d times for invalid input; want 0", r.calls)
	}
}

func TestCheck_ipValidatedBeforeCountries(t *testing.T) {
	// Deterministic error ordering: one request, one error, always the same one.
	svc := newService(&fakeResolver{})
	_, err := svc.Check(context.Background(), Request{IP: "bad", AllowedCountries: []string{"ZZ"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != CodeInvalidIP {
		t.Fatalf("err=%v, want invalid_ip first", err)
	}
}

func TestCheck_maxAllowedCountriesAccepted(t *testing.T) {
	codes := make([]string, 0, MaxAllowedCountries)
	for c := range assignedISO {
		if len(codes) == MaxAllowedCountries {
			break
		}
		codes = append(codes, c)
	}
	svc := newService(&fakeResolver{})
	if _, err := svc.Check(context.Background(), Request{IP: "8.8.8.8", AllowedCountries: codes}); err != nil {
		t.Fatalf("exactly MaxAllowedCountries should be accepted: %v", err)
	}
}

func TestCheck_resolverErrorWhileCallerIsDone(t *testing.T) {
	// When the caller's context is done, the error is the context error,
	// never a database outage, regardless of what the resolver returned.
	// The resolver's message stays in the text for the logs.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel2()

	cases := []struct {
		name        string
		ctx         context.Context
		resolverErr error
		want        error
	}{
		{"canceled, resolver canceled", canceled, context.Canceled, context.Canceled},
		{"canceled, unrelated resolver error", canceled, errors.New("disk on fire"), context.Canceled},
		{"deadline, resolver deadline", expired, context.DeadlineExceeded, context.DeadlineExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := newService(&fakeResolver{err: c.resolverErr})
			_, err := svc.Check(c.ctx, Request{IP: "8.8.8.8", AllowedCountries: []string{"US"}})
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			if errors.Is(err, ErrDatabaseUnavailable) {
				t.Fatalf("err=%v: a done caller must not be reported as a database outage", err)
			}
			if !strings.Contains(err.Error(), c.resolverErr.Error()) {
				t.Fatalf("err=%v lost the resolver's message", err)
			}
		})
	}
}

func TestCheck_resolverCanceledWhileCallerIsLiveIsDatabaseUnavailable(t *testing.T) {
	// A resolver returning context.Canceled on its own (caller still live)
	// is the resolver's failure. It must match exactly one category.
	svc := newService(&fakeResolver{err: context.Canceled})
	_, err := svc.Check(context.Background(), Request{IP: "8.8.8.8", AllowedCountries: []string{"US"}})
	if !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatalf("err=%v, want ErrDatabaseUnavailable", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v matches both ErrDatabaseUnavailable and context.Canceled", err)
	}
}
