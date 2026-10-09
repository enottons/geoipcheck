// Package geoip loads MaxMind country databases and serves lookups from
// the currently loaded copy. A Watcher reloads the file when it changes;
// lookups never block on a reload (design decisions D8, D9).
package geoip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

var (
	// ErrNotLoaded is returned by Reader.Country before any database has
	// been stored.
	ErrNotLoaded = errors.New("geoip database not loaded")

	// ErrUnsupportedDatabase is returned by Parse for a valid MaxMind file
	// that is not a country database (for example GeoLite2-ASN).
	ErrUnsupportedDatabase = errors.New("unsupported geoip database type")
)

// supportedTypes are database types whose records carry country.iso_code.
// GeoIP2-Country is the commercial build of the same schema.
var supportedTypes = []string{"GeoLite2-Country", "GeoIP2-Country"}

// Database is one immutable, fully loaded MaxMind country database.
type Database struct {
	reader    *maxminddb.Reader
	Type      string
	BuildTime time.Time
}

// Parse builds a Database from the raw bytes of an .mmdb file. The bytes
// are retained; the caller must not modify them afterwards.
//
// Parse verifies the whole search tree and data section. That walk runs
// once per reload on the watcher goroutine, off the lookup path, and it
// means a truncated or corrupt file is rejected here instead of failing
// lookups later.
func Parse(data []byte) (*Database, error) {
	r, err := maxminddb.OpenBytes(data)
	if err != nil {
		return nil, fmt.Errorf("open mmdb: %w", err)
	}
	if !slices.Contains(supportedTypes, r.Metadata.DatabaseType) {
		return nil, fmt.Errorf("%w: %q (want one of %v)", ErrUnsupportedDatabase, r.Metadata.DatabaseType, supportedTypes)
	}
	if err := r.Verify(); err != nil {
		return nil, fmt.Errorf("verify mmdb: %w", err)
	}
	return &Database{reader: r, Type: r.Metadata.DatabaseType, BuildTime: r.Metadata.BuildTime()}, nil
}

// Country returns the ISO 3166-1 alpha-2 code from the record's `country`
// field. It returns "" with no error when there is no record or the
// record has no country. `registered_country` is deliberately ignored: it
// is where the address block is registered, not where the user is (D13).
func (d *Database) Country(ip netip.Addr) (string, error) {
	var iso string
	if err := d.reader.Lookup(ip).DecodePath(&iso, "country", "iso_code"); err != nil {
		// The IP is deliberately left out: this error ends up in logs, and
		// logs only ever carry the redacted prefix.
		return "", fmt.Errorf("decode country: %w", err)
	}
	return iso, nil
}

// Reader serves lookups from the most recently stored Database. The zero
// value is ready to use and reports ErrNotLoaded until Store is called.
// It is safe for concurrent use; lookups take no lock.
type Reader struct {
	db atomic.Pointer[Database]
}

// Store makes db the database used by subsequent lookups. Lookups already
// in flight finish on the previous database, which the garbage collector
// reclaims once they are done: Parse reads from a byte slice, not an mmap,
// so there is nothing to unmap (D9).
func (r *Reader) Store(db *Database) {
	r.db.Store(db)
}

// Current returns the database in use, or nil if none has been stored.
func (r *Reader) Current() *Database {
	return r.db.Load()
}

// Country implements countrycheck.CountryResolver.
func (r *Reader) Country(_ context.Context, ip netip.Addr) (string, error) {
	db := r.db.Load()
	if db == nil {
		return "", ErrNotLoaded
	}
	return db.Country(ip)
}
