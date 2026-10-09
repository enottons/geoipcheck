//go:build integration

package countrycheck_test

import (
	"context"
	"testing"

	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/geoip"
)

// Proves the Service ↔ geoip.Reader seam with the real test database: one
// case per resolver-driven decision. Decision rules are owned by the unit
// tests; this only shows the real record shape flows through.
func TestServiceWithRealReader(t *testing.T) {
	db, err := geoip.LoadFile("../../testdata/GeoLite2-Country-Test.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	var r geoip.Reader
	r.Store(db)
	svc := countrycheck.NewService(&r)

	cases := []struct {
		ip       string
		allowed  []string
		decision countrycheck.Decision
		country  string
	}{
		{"81.2.69.142", []string{"GB"}, countrycheck.DecisionAllowed, "GB"},
		// registered_country for this IP is US; allowing US must not allow it.
		{"81.2.69.142", []string{"US"}, countrycheck.DecisionDenied, "GB"},
		{"2a02:d500::1", []string{"GB"}, countrycheck.DecisionUnknown, ""},
	}
	for _, c := range cases {
		got, err := svc.Check(context.Background(), countrycheck.Request{IP: c.ip, AllowedCountries: c.allowed})
		if err != nil {
			t.Fatalf("%s: %v", c.ip, err)
		}
		if got.Decision != c.decision || got.Country != c.country {
			t.Errorf("%s allowed=%v: got %s/%q, want %s/%q", c.ip, c.allowed, got.Decision, got.Country, c.decision, c.country)
		}
	}
}
