package countrycheck

import "testing"

func TestAssignedISO_countIsPinnedSnapshot(t *testing.T) {
	// Pins the ISO 3166-1 alpha-2 snapshot (249 assigned codes) plus XK.
	// If this test fails because a new code was assigned, update the
	// list in iso3166.go, bump the snapshot date in the file header,
	// and update this count.
	const want = 250
	if got := len(assignedISO); got != want {
		t.Fatalf("len(assignedISO)=%d, want %d", got, want)
	}
}

func TestAssignedISO_includesXK(t *testing.T) {
	if !isAssignedCountry("XK") {
		t.Fatal("XK must be accepted: MaxMind emits it for Kosovo")
	}
}

func TestAssignedISO_rejectsUnassigned(t *testing.T) {
	for _, code := range []string{"ZZ", "AA", "QM", "UK"} {
		if isAssignedCountry(code) {
			t.Errorf("%q should be unassigned", code)
		}
	}
}

func TestAssignedISO_sampleCommonCountries(t *testing.T) {
	for _, code := range []string{"US", "GB", "DE", "FR", "JP", "CN", "IN", "BR"} {
		if !isAssignedCountry(code) {
			t.Errorf("%q should be assigned", code)
		}
	}
}

func TestHintFor_UKMapsToGB(t *testing.T) {
	// Protects against the exact bug called out in D12. If this hint is
	// ever dropped, "UK" starts round-tripping through validation and
	// silently never matches any user because MaxMind returns "GB".
	if got := hintFor("UK"); got != "GB" {
		t.Fatalf("hintFor(\"UK\")=%q, want \"GB\"", got)
	}
}

func TestHintFor_absentCode(t *testing.T) {
	if got := hintFor("ZZ"); got != "" {
		t.Fatalf("hintFor(\"ZZ\")=%q, want \"\"", got)
	}
}

func TestNearMissHints_pointToAssignedCodes(t *testing.T) {
	// Every suggested code must itself be a valid ISO code; otherwise the
	// hint recommends a value that would be rejected by the same validator.
	for bad, suggestion := range nearMissHints {
		if !isAssignedCountry(suggestion) {
			t.Errorf("hint %q → %q: suggestion is not an assigned ISO code", bad, suggestion)
		}
	}
}
