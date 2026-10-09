package countrycheck

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// FuzzCheck asserts the core contract on arbitrary input: Check never
// panics, and it returns either a well-formed Result or a *ValidationError.
// The fake resolver never fails, so ErrDatabaseUnavailable is never valid.
func FuzzCheck(f *testing.F) {
	seeds := []struct{ ip, countries string }{
		{"81.2.69.142", "GB,US"},
		{"::ffff:81.2.69.142", "gb"},
		{"10.0.0.1", "US"},
		{"fe80::1%eth0", "US"},
		{"1.2.3.4:443", "US"},
		{"2001:db8::1", "UK"},
		{"", ""},
		{" 1.2.3.4", "ZZ"},
	}
	for _, s := range seeds {
		f.Add(s.ip, s.countries)
	}

	svc := NewService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	f.Fuzz(func(t *testing.T, ip, countries string) {
		var list []string
		if countries != "" {
			list = strings.Split(countries, ",")
		}
		res, err := svc.Check(context.Background(), Request{IP: ip, AllowedCountries: list})
		if err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("non-validation error %v", err)
			}
			if ve.Code == "" || ve.Field == "" || ve.Message == "" {
				t.Fatalf("incomplete ValidationError %+v", ve)
			}
			return
		}
		assertWellFormed(t, res)
	})
}

func assertWellFormed(t *testing.T, res Result) {
	t.Helper()
	if !res.IP.IsValid() || res.IP.Is4In6() || res.IP.Zone() != "" {
		t.Fatalf("bad IP in result: %v", res.IP)
	}
	if !slices.Contains(Outcomes(), Outcome{res.Decision, res.Reason}) {
		t.Fatalf("decision %q with reason %q is not in Outcomes()", res.Decision, res.Reason)
	}
	if (res.Decision == DecisionUnknown) != (res.Country == "") {
		t.Fatalf("country %q inconsistent with decision %q", res.Country, res.Decision)
	}
}

func BenchmarkCheck(b *testing.B) {
	svc := NewService(&fakeResolver{countries: map[string]string{"81.2.69.142": "GB"}})
	req := Request{IP: "81.2.69.142", AllowedCountries: []string{"US", "GB", "DE", "FR", "CA"}}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := svc.Check(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIsNonRoutable(b *testing.B) {
	addr := netip.MustParseAddr("2606:4700:4700::1111")
	for b.Loop() {
		_ = isNonRoutable(addr)
	}
}
