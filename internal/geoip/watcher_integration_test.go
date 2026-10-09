//go:build integration

package geoip

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/enottons/geoipcheck/internal/testutil/logtest"
)

// These tests own what only the real filesystem can show: that the default
// stat/load seams see a replaced file, and that lookups racing a reload are
// safe. Change-detection rules themselves are owned by watcher_test.go.

func newFSWatcher(t *testing.T, path string) (*Watcher, *Reader) {
	t.Helper()
	r := &Reader{}
	_, logger := logtest.New()
	w := NewWatcher(WatcherConfig{
		Path:    path,
		Reader:  r,
		Metrics: NewMetrics(prometheus.NewRegistry()),
		Logger:  logger,
	})
	return w, r
}

// replaceFile writes data to a temp file and renames it over path, the way
// geoipupdate does, and pushes the mtime forward so the change is visible
// even on filesystems with coarse timestamps.
func replaceFile(t *testing.T, path string, data []byte, mtime time.Time) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	// On Windows, renaming over a file that was just read can briefly fail
	// with "Access is denied" while antivirus or the indexer holds a
	// handle. Retry for a bounded time; Linux succeeds first try.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Rename(tmp, path)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherIntegration_replacedFileIsServed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	base := time.Now().Add(-time.Hour)
	replaceFile(t, path, readFixture(t, countryTestDB), base)

	w, r := newFSWatcher(t, path)
	w.Poll()
	if got := r.Current(); got == nil || got.Type != "GeoLite2-Country" {
		t.Fatalf("initial load: %+v", got)
	}

	replaceFile(t, path, readFixture(t, geoip2TestDB), base.Add(time.Minute))
	w.Poll()
	if got := r.Current().Type; got != "GeoIP2-Country" {
		t.Fatalf("after replace: Type=%q, want GeoIP2-Country", got)
	}
}

func TestWatcherIntegration_corruptReplacementKeepsServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	base := time.Now().Add(-time.Hour)
	good := readFixture(t, countryTestDB)
	replaceFile(t, path, good, base)

	w, r := newFSWatcher(t, path)
	w.Poll()
	before := r.Current()

	replaceFile(t, path, good[:len(good)/2], base.Add(time.Minute))
	w.Poll()

	if r.Current() != before {
		t.Fatal("corrupt replacement must not replace the loaded database")
	}
	got, err := r.Country(context.Background(), netip.MustParseAddr("81.2.69.142"))
	if err != nil || got != "GB" {
		t.Fatalf("lookup after failed reload: %q, %v", got, err)
	}
}

func TestWatcherIntegration_lookupsDuringReloadAreRaceFree(t *testing.T) {
	// Meaningful under -race: concurrent Country calls while Poll swaps
	// databases back and forth.
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	base := time.Now().Add(-time.Hour)
	fixtures := [][]byte{readFixture(t, countryTestDB), readFixture(t, geoip2TestDB)}
	replaceFile(t, path, fixtures[0], base)

	w, r := newFSWatcher(t, path)
	w.Poll()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	// Registered before the readers start so a t.Fatal in the swap loop
	// still stops them instead of leaking busy goroutines.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	const readers = 8
	var started sync.WaitGroup
	started.Add(readers)
	ip := netip.MustParseAddr("81.2.69.142")
	for range readers {
		wg.Go(func() {
			first := true
			for ctx.Err() == nil {
				got, err := r.Country(ctx, ip)
				if first {
					started.Done()
					first = false
				}
				if err != nil || got != "GB" {
					t.Errorf("lookup during reload: %q, %v", got, err)
					return
				}
			}
		})
	}
	// Every reader is mid-loop before the first swap, so the swaps really
	// race with lookups.
	started.Wait()
	for i := 1; i <= 20; i++ {
		replaceFile(t, path, fixtures[i%2], base.Add(time.Duration(i)*time.Minute))
		w.Poll()
	}
}
