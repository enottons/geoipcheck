package geoip

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/enottons/geoipcheck/internal/testutil/logtest"
)

// fakeFS drives the Watcher's stat and load seams.
type fakeFS struct {
	state   FileState
	statErr error
	db      *Database
	loadErr error
	loads   int
}

func (f *fakeFS) stat(string) (FileState, error) { return f.state, f.statErr }

func (f *fakeFS) load(string) (*Database, error) {
	f.loads++
	return f.db, f.loadErr
}

// touch simulates the file being replaced: new mtime, same size.
func (f *fakeFS) touch() { f.state.ModTime = f.state.ModTime.Add(time.Minute) }

type watcherHarness struct {
	w       *Watcher
	fs      *fakeFS
	reader  *Reader
	logs    *logtest.Handler
	reg     *prometheus.Registry
	metrics *Metrics
	now     time.Time
}

func newHarness(t *testing.T, db *Database) *watcherHarness {
	t.Helper()
	h := &watcherHarness{
		fs:     &fakeFS{state: FileState{ModTime: time.Unix(1_700_000_000, 0), Size: 1000}, db: db},
		reader: &Reader{},
		reg:    prometheus.NewRegistry(),
	}
	h.metrics = NewMetrics(h.reg)
	// Default clock: one day after the build, i.e. fresh.
	h.now = time.Unix(1_700_000_000, 0)
	if db != nil {
		h.now = db.BuildTime.Add(24 * time.Hour)
	}
	var logger *slog.Logger
	h.logs, logger = logtest.New()
	h.w = NewWatcher(WatcherConfig{
		Path:       "/data/GeoLite2-Country.mmdb",
		Reader:     h.reader,
		Metrics:    h.metrics,
		Logger:     logger,
		StaleWarn:  7 * 24 * time.Hour,
		StaleError: 14 * 24 * time.Hour,
		Stat:       h.fs.stat,
		Load:       h.fs.load,
		Now:        func() time.Time { return h.now },
	})
	return h
}

func (h *watcherHarness) reloads(result string) float64 {
	return testutil.ToFloat64(h.metrics.reloads.WithLabelValues(result))
}

func TestWatcher_firstPollLoadsAndPublishes(t *testing.T) {
	db := parseFixture(t, countryTestDB)
	h := newHarness(t, db)

	h.w.Poll()

	if h.reader.Current() != db {
		t.Fatal("database not stored in reader")
	}
	if got := h.reloads(reloadSuccess); got != 1 {
		t.Errorf("reloads{success}=%v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.loaded); got != 1 {
		t.Errorf("db_loaded=%v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.buildTimestamp); got != float64(db.BuildTime.Unix()) {
		t.Errorf("build_timestamp=%v, want %v", got, db.BuildTime.Unix())
	}
	logs := h.logs.Find("geoip database loaded")
	if len(logs) != 1 || logs[0].Level != slog.LevelInfo {
		t.Fatalf("want one info 'geoip database loaded', got %+v", h.logs.Records())
	}
	if got := logs[0].Attrs["type"].String(); got != "GeoLite2-Country" {
		t.Errorf("log type=%q", got)
	}
}

func TestWatcher_unchangedFileIsNotReloaded(t *testing.T) {
	h := newHarness(t, parseFixture(t, countryTestDB))
	h.w.Poll()
	h.w.Poll()
	h.w.Poll()
	if h.fs.loads != 1 {
		t.Fatalf("loads=%d, want 1", h.fs.loads)
	}
}

func TestWatcher_reloadsWhenModTimeOrSizeChanges(t *testing.T) {
	h := newHarness(t, parseFixture(t, countryTestDB))
	h.w.Poll()

	h.fs.touch()
	h.w.Poll()
	if h.fs.loads != 2 {
		t.Fatalf("after mtime change: loads=%d, want 2", h.fs.loads)
	}

	h.fs.state.Size++
	h.w.Poll()
	if h.fs.loads != 3 {
		t.Fatalf("after size change: loads=%d, want 3", h.fs.loads)
	}
}

func TestWatcher_swapsToNewDatabase(t *testing.T) {
	first := parseFixture(t, countryTestDB)
	second := parseFixture(t, geoip2TestDB)
	h := newHarness(t, first)
	h.w.Poll()

	h.fs.db = second
	h.fs.touch()
	h.w.Poll()

	if h.reader.Current() != second {
		t.Fatal("reader still serving the first database")
	}
	if got := h.reloads(reloadSuccess); got != 2 {
		t.Errorf("reloads{success}=%v, want 2", got)
	}
}

func TestWatcher_failedReloadKeepsServingPreviousDatabase(t *testing.T) {
	good := parseFixture(t, countryTestDB)
	h := newHarness(t, good)
	h.w.Poll()

	h.fs.db, h.fs.loadErr = nil, errors.New("verify mmdb: truncated")
	h.fs.touch()
	h.w.Poll()

	if h.reader.Current() != good {
		t.Fatal("failed reload must not replace the loaded database")
	}
	if got := h.reloads(reloadFailure); got != 1 {
		t.Errorf("reloads{failure}=%v, want 1", got)
	}
	logs := h.logs.Find("geoip database load failed")
	if len(logs) != 1 || logs[0].Level != slog.LevelError {
		t.Fatalf("want one error log, got %+v", h.logs.Records())
	}
	if got := logs[0].Attrs["serving_previous"].Bool(); !got {
		t.Error("serving_previous should be true when an older database is loaded")
	}
}

func TestWatcher_failedFileIsNotRetriedUntilItChanges(t *testing.T) {
	// A corrupt file that stays in place must not be re-read every poll.
	h := newHarness(t, nil)
	h.fs.loadErr = errors.New("corrupt")
	h.w.Poll()
	h.w.Poll()
	if h.fs.loads != 1 {
		t.Fatalf("loads=%d, want 1", h.fs.loads)
	}

	h.fs.loadErr, h.fs.db = nil, parseFixture(t, countryTestDB)
	h.fs.touch()
	h.w.Poll()
	if h.fs.loads != 2 || h.reader.Current() == nil {
		t.Fatalf("loads=%d current=%v: replaced file should load", h.fs.loads, h.reader.Current())
	}
}

func TestWatcher_missingFileLeavesReaderEmptyAndLogsOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.fs.statErr = fs.ErrNotExist
	h.w.Poll()
	h.w.Poll()

	if h.reader.Current() != nil {
		t.Fatal("reader should stay empty")
	}
	if h.fs.loads != 0 {
		t.Fatalf("loads=%d, want 0", h.fs.loads)
	}
	if got := testutil.ToFloat64(h.metrics.loaded); got != 0 {
		t.Errorf("db_loaded=%v, want 0", got)
	}
	logs := h.logs.Find("geoip database file unavailable")
	if len(logs) != 1 || logs[0].Level != slog.LevelWarn {
		t.Fatalf("want one warn log across two polls, got %+v", h.logs.Records())
	}
}

func TestWatcher_fileAppearingLaterIsLoaded(t *testing.T) {
	h := newHarness(t, parseFixture(t, countryTestDB))
	h.fs.statErr = fs.ErrNotExist
	h.w.Poll()

	h.fs.statErr = nil
	h.w.Poll()
	if h.reader.Current() == nil {
		t.Fatal("database should load once the file appears")
	}

	// A second outage after recovery is logged again.
	h.fs.statErr = fs.ErrNotExist
	h.w.Poll()
	if n := len(h.logs.Find("geoip database file unavailable")); n != 2 {
		t.Fatalf("unavailable logs=%d, want 2", n)
	}
}

func TestWatcher_stalenessLoggedOncePerThreshold(t *testing.T) {
	db := parseFixture(t, countryTestDB)
	h := newHarness(t, db)

	h.now = db.BuildTime.Add(6 * 24 * time.Hour)
	h.w.Poll()
	if n := len(h.logs.Find("geoip database is stale")); n != 0 {
		t.Fatalf("fresh database logged as stale %d times", n)
	}

	h.now = db.BuildTime.Add(8 * 24 * time.Hour)
	h.w.Poll()
	h.w.Poll()
	stale := h.logs.Find("geoip database is stale")
	if len(stale) != 1 || stale[0].Level != slog.LevelWarn {
		t.Fatalf("want one warn after crossing StaleWarn, got %+v", stale)
	}

	h.now = db.BuildTime.Add(15 * 24 * time.Hour)
	h.w.Poll()
	h.w.Poll()
	stale = h.logs.Find("geoip database is stale")
	if len(stale) != 2 || stale[1].Level != slog.LevelError {
		t.Fatalf("want a second log at error after crossing StaleError, got %+v", stale)
	}
}

func TestWatcher_reloadReevaluatesStaleness(t *testing.T) {
	// Reloading a database that is still stale logs again, so an operator
	// sees that the update job delivered an old file.
	db := parseFixture(t, countryTestDB)
	h := newHarness(t, db)
	h.now = db.BuildTime.Add(8 * 24 * time.Hour)
	h.w.Poll()

	h.fs.touch()
	h.w.Poll()
	if n := len(h.logs.Find("geoip database is stale")); n != 2 {
		t.Fatalf("stale logs=%d, want 2", n)
	}
}

func TestWatcher_runPollsImmediatelyThenOnEachTickAndStopsOnCancel(t *testing.T) {
	fake := &fakeFS{state: FileState{Size: 1}, db: parseFixture(t, countryTestDB)}
	polled := make(chan struct{}, 1)
	_, logger := logtest.New()
	w := NewWatcher(WatcherConfig{
		Path:    "db.mmdb",
		Reader:  &Reader{},
		Metrics: NewMetrics(prometheus.NewRegistry()),
		Logger:  logger,
		Stat: func(p string) (FileState, error) {
			polled <- struct{}{}
			return fake.stat(p)
		},
		Load: fake.load,
		Now:  time.Now,
	})

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, ticks)
		close(done)
	}()

	<-polled // initial poll, before any tick
	ticks <- time.Time{}
	<-polled
	ticks <- time.Time{}
	<-polled

	// Run must return on cancel. If it didn't, this receive would block and
	// the test would time out.
	cancel()
	<-done
}
