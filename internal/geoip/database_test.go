package geoip

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"
)

// Test fixtures are MaxMind's published test databases (see
// testdata/LICENSE-*). Known records in GeoLite2-Country-Test:
//   - 81.2.69.142: country GB, registered_country US
//   - 89.160.20.112: country SE
//   - 2a02:d500::/29: record present, no country
const (
	countryTestDB  = "../../testdata/GeoLite2-Country-Test.mmdb"
	geoip2TestDB   = "../../testdata/GeoIP2-Country-Test.mmdb"
	asnTestDB      = "../../testdata/GeoLite2-ASN-Test.mmdb"
	testDBBuildDay = "2026-02-04"
)

func readFixture(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseFixture(t testing.TB, path string) *Database {
	t.Helper()
	db, err := Parse(readFixture(t, path))
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	return db
}

func TestParse_exposesMetadata(t *testing.T) {
	db := parseFixture(t, countryTestDB)
	if db.Type != "GeoLite2-Country" {
		t.Errorf("Type=%q", db.Type)
	}
	if got := db.BuildTime.UTC().Format(time.DateOnly); got != testDBBuildDay {
		t.Errorf("BuildTime=%s, want %s", got, testDBBuildDay)
	}
}

func TestParse_acceptsCommercialCountryDB(t *testing.T) {
	// GeoIP2-Country has the same record shape; operators may upgrade.
	if db := parseFixture(t, geoip2TestDB); db.Type != "GeoIP2-Country" {
		t.Fatalf("Type=%q", db.Type)
	}
}

func TestParse_rejectsWrongDatabaseType(t *testing.T) {
	_, err := Parse(readFixture(t, asnTestDB))
	if !errors.Is(err, ErrUnsupportedDatabase) {
		t.Fatalf("err=%v, want ErrUnsupportedDatabase", err)
	}
}

func TestParse_rejectsCorruptBytes(t *testing.T) {
	full := readFixture(t, countryTestDB)
	cases := map[string][]byte{
		"empty":     {},
		"garbage":   []byte("this is not a maxmind database"),
		"truncated": full[:len(full)/2],
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(b); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestDatabaseCountry(t *testing.T) {
	db := parseFixture(t, countryTestDB)
	cases := []struct {
		ip, want string
	}{
		// country is GB, registered_country is US: we must report GB (D13).
		{"81.2.69.142", "GB"},
		{"89.160.20.112", "SE"},
		// Record exists but has no country field.
		{"2a02:d500::1", ""},
		// No record at all.
		{"8.8.8.8", ""},
	}
	for _, c := range cases {
		got, err := db.Country(netip.MustParseAddr(c.ip))
		if err != nil {
			t.Fatalf("%s: %v", c.ip, err)
		}
		if got != c.want {
			t.Errorf("Country(%s)=%q, want %q", c.ip, got, c.want)
		}
	}
}

func TestReader_notLoadedUntilStored(t *testing.T) {
	var r Reader
	if r.Current() != nil {
		t.Fatal("zero Reader should have no database")
	}
	if _, err := r.Country(context.Background(), netip.MustParseAddr("81.2.69.142")); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("err=%v, want ErrNotLoaded", err)
	}

	db := parseFixture(t, countryTestDB)
	r.Store(db)
	if r.Current() != db {
		t.Fatal("Current should return the stored database")
	}
	got, err := r.Country(context.Background(), netip.MustParseAddr("81.2.69.142"))
	if err != nil || got != "GB" {
		t.Fatalf("Country=%q err=%v, want GB", got, err)
	}
}

func BenchmarkReaderCountry(b *testing.B) {
	var r Reader
	r.Store(parseFixture(b, countryTestDB))
	ip := netip.MustParseAddr("81.2.69.142")
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Country(ctx, ip); err != nil {
			b.Fatal(err)
		}
	}
}
