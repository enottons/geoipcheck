package geoip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// FileState is what the Watcher compares between polls to decide whether
// the database file changed.
type FileState struct {
	ModTime time.Time
	Size    int64
}

func (s FileState) equal(o FileState) bool {
	return s.Size == o.Size && s.ModTime.Equal(o.ModTime)
}

// WatcherConfig configures a Watcher. Stat, Load and Now are test seams;
// nil means the real implementation.
type WatcherConfig struct {
	Path    string
	Reader  *Reader
	Metrics *Metrics
	Logger  *slog.Logger

	// StaleWarn and StaleError are database ages, measured from the build
	// time in the file's metadata, that trigger a warn or error log. Zero
	// disables the threshold. StaleError should exceed StaleWarn, or the
	// warn level is never logged; config validation enforces this.
	StaleWarn  time.Duration
	StaleError time.Duration

	Stat func(path string) (FileState, error)
	Load func(path string) (*Database, error)
	Now  func() time.Time
}

// Watcher polls the database file and swaps a new Database into the
// Reader when the file's mtime or size changes (D8). A file that fails to
// load is not retried until it changes again, and the previous database
// keeps serving.
//
// Poll and Run must not be called concurrently; Run owns the Watcher.
type Watcher struct {
	cfg WatcherConfig

	// lastAttempt is recorded before Load, not after a successful load, so a
	// file that fails to parse is not re-read every poll (D8).
	attempted   bool
	lastAttempt FileState
	statFailing bool
	staleLevel  slog.Level
}

// levelFresh is below every real level so "fresh" never compares equal to
// a warn or error staleness level.
const levelFresh = slog.LevelDebug - 1

// NewWatcher returns a Watcher. Path, Reader, Metrics and Logger are
// required.
func NewWatcher(cfg WatcherConfig) *Watcher {
	if cfg.Stat == nil {
		cfg.Stat = statFile
	}
	if cfg.Load == nil {
		cfg.Load = LoadFile
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Logger = cfg.Logger.With(slog.String("path", cfg.Path))
	return &Watcher{cfg: cfg, staleLevel: levelFresh}
}

// Run polls once immediately, then once per value received from ticks,
// until ctx is done. Pass time.Ticker.C in production.
func (w *Watcher) Run(ctx context.Context, ticks <-chan time.Time) {
	w.Poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			w.Poll()
		}
	}
}

// Poll checks the file once and reloads it if it changed.
func (w *Watcher) Poll() {
	defer w.checkStaleness()

	st, err := w.cfg.Stat(w.cfg.Path)
	if err != nil {
		// Logged on the transition only. While the file is missing the pod
		// is not ready, and the readiness probe already says so every few
		// seconds; repeating the log every poll adds nothing.
		if !w.statFailing {
			w.cfg.Logger.Warn("geoip database file unavailable",
				slog.Bool("serving_previous", w.cfg.Reader.Current() != nil),
				slog.Any("error", err))
		}
		w.statFailing = true
		return
	}
	w.statFailing = false

	if w.attempted && st.equal(w.lastAttempt) {
		return
	}
	w.attempted, w.lastAttempt = true, st

	db, err := w.cfg.Load(w.cfg.Path)
	if err != nil {
		w.cfg.Metrics.reloads.WithLabelValues(reloadFailure).Inc()
		w.cfg.Logger.Error("geoip database load failed",
			slog.Bool("serving_previous", w.cfg.Reader.Current() != nil),
			slog.Any("error", err))
		return
	}

	prev := w.cfg.Reader.Current()
	w.cfg.Reader.Store(db)
	w.cfg.Metrics.reloads.WithLabelValues(reloadSuccess).Inc()
	w.cfg.Metrics.loaded.Set(1)
	w.cfg.Metrics.buildTimestamp.Set(float64(db.BuildTime.Unix()))

	attrs := []any{
		slog.String("type", db.Type),
		slog.Time("build_time", db.BuildTime),
		slog.Int64("size_bytes", st.Size),
	}
	if prev != nil {
		attrs = append(attrs, slog.Time("previous_build_time", prev.BuildTime))
	}
	w.cfg.Logger.Info("geoip database loaded", attrs...)

	// A new database gets its staleness evaluated from scratch, so loading
	// a file that is already old is logged even if the old one was too.
	w.staleLevel = levelFresh
}

func (w *Watcher) checkStaleness() {
	db := w.cfg.Reader.Current()
	if db == nil {
		return
	}
	age := w.cfg.Now().Sub(db.BuildTime)
	level := levelFresh
	switch {
	case w.cfg.StaleError > 0 && age > w.cfg.StaleError:
		level = slog.LevelError
	case w.cfg.StaleWarn > 0 && age > w.cfg.StaleWarn:
		level = slog.LevelWarn
	}
	if level == w.staleLevel {
		return
	}
	w.staleLevel = level
	if level == levelFresh {
		return
	}
	w.cfg.Logger.Log(context.Background(), level, "geoip database is stale",
		slog.Time("build_time", db.BuildTime),
		slog.String("age", age.Round(time.Minute).String()))
}

// LoadFile reads and parses the database at path.
func LoadFile(path string) (*Database, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// statFile follows symlinks, which is what a Kubernetes volume swap
// changes: the link target moves, and the target's mtime is new.
func statFile(path string) (FileState, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileState{}, err
	}
	return FileState{ModTime: fi.ModTime(), Size: fi.Size()}, nil
}
