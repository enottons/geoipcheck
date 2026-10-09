package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoad_defaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"GEOIPCHECK_DB_PATH": "/data/GeoLite2-Country.mmdb"}))

	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DBPath:          "/data/GeoLite2-Country.mmdb",
		DBPollInterval:  60 * time.Second,
		StaleWarn:       168 * time.Hour,
		StaleError:      336 * time.Hour,
		HTTPAddr:        ":8080",
		GRPCAddr:        ":9090",
		AdminAddr:       ":8081",
		LogLevel:        slog.LevelInfo,
		LogFormat:       LogFormatJSON,
		DrainDelay:      5 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("config (-want +got):\n%s", diff)
	}
}

func TestLoad_everyVariableIsRead(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GEOIPCHECK_DB_PATH":          "db.mmdb",
		"GEOIPCHECK_DB_POLL_INTERVAL": "5s",
		"GEOIPCHECK_DB_STALE_WARN":    "1h",
		"GEOIPCHECK_DB_STALE_ERROR":   "2h",
		"GEOIPCHECK_HTTP_ADDR":        "127.0.0.1:1",
		"GEOIPCHECK_GRPC_ADDR":        "127.0.0.1:2",
		"GEOIPCHECK_ADMIN_ADDR":       "127.0.0.1:3",
		"GEOIPCHECK_LOG_LEVEL":        "DEBUG",
		"GEOIPCHECK_LOG_FORMAT":       "Text",
		"GEOIPCHECK_DRAIN_DELAY":      "0s",
		"GEOIPCHECK_SHUTDOWN_TIMEOUT": "3s",
	}))

	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DBPath:          "db.mmdb",
		DBPollInterval:  5 * time.Second,
		StaleWarn:       time.Hour,
		StaleError:      2 * time.Hour,
		HTTPAddr:        "127.0.0.1:1",
		GRPCAddr:        "127.0.0.1:2",
		AdminAddr:       "127.0.0.1:3",
		LogLevel:        slog.LevelDebug,
		LogFormat:       LogFormatText,
		DrainDelay:      0,
		ShutdownTimeout: 3 * time.Second,
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("config (-want +got):\n%s", diff)
	}
}

func TestLoad_zeroDisablesAStalenessThreshold(t *testing.T) {
	// One threshold off must not trip the ordering check.
	cfg, err := Load(env(map[string]string{
		"GEOIPCHECK_DB_PATH":        "db.mmdb",
		"GEOIPCHECK_DB_STALE_WARN":  "0s",
		"GEOIPCHECK_DB_STALE_ERROR": "1h",
	}))

	if err != nil {
		t.Fatal(err)
	}
	if cfg.StaleWarn != 0 || cfg.StaleError != time.Hour {
		t.Errorf("StaleWarn=%v StaleError=%v", cfg.StaleWarn, cfg.StaleError)
	}
}

func TestLoad_portZeroMayRepeat(t *testing.T) {
	_, err := Load(env(map[string]string{
		"GEOIPCHECK_DB_PATH":    "db.mmdb",
		"GEOIPCHECK_HTTP_ADDR":  "127.0.0.1:0",
		"GEOIPCHECK_GRPC_ADDR":  "127.0.0.1:0",
		"GEOIPCHECK_ADMIN_ADDR": "127.0.0.1:00",
	}))

	if err != nil {
		t.Fatal(err)
	}
}

func TestLoad_valuesAreTrimmed(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"GEOIPCHECK_DB_PATH":   "  db.mmdb\n",
		"GEOIPCHECK_HTTP_ADDR": " :7000 ",
	}))

	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != "db.mmdb" || cfg.HTTPAddr != ":7000" {
		t.Errorf("DBPath=%q HTTPAddr=%q", cfg.DBPath, cfg.HTTPAddr)
	}
}

// problems loads vars on top of a valid DB_PATH and returns the problem list.
func problems(t *testing.T, vars map[string]string) []string {
	t.Helper()
	vars["GEOIPCHECK_DB_PATH"] = "db.mmdb"
	_, err := Load(env(vars))
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err=%v, want *Error", err)
	}
	return cerr.Problems
}

func TestLoad_rejectsPortsThatCannotBind(t *testing.T) {
	for _, addr := range []string{":http", ":65536", ":-1", "localhost:"} {
		t.Run(addr, func(t *testing.T) {
			got := problems(t, map[string]string{"GEOIPCHECK_HTTP_ADDR": addr})

			if len(got) != 1 || !strings.HasPrefix(got[0], "GEOIPCHECK_HTTP_ADDR ") {
				t.Errorf("problems=%q, want one for HTTP_ADDR", got)
			}
		})
	}
}

func TestLoad_staleErrorEqualToStaleWarnIsRejected(t *testing.T) {
	got := problems(t, map[string]string{
		"GEOIPCHECK_DB_STALE_WARN":  "1h",
		"GEOIPCHECK_DB_STALE_ERROR": "1h",
	})

	if len(got) != 1 || !strings.HasPrefix(got[0], "GEOIPCHECK_DB_STALE_ERROR ") {
		t.Errorf("problems=%q, want one for DB_STALE_ERROR", got)
	}
}

func TestLoad_sharedPortIsRejected(t *testing.T) {
	// Different hosts and spellings, same port number.
	got := problems(t, map[string]string{
		"GEOIPCHECK_HTTP_ADDR": "127.0.0.1:7000",
		"GEOIPCHECK_GRPC_ADDR": "0.0.0.0:07000",
	})

	if len(got) != 1 || !strings.HasPrefix(got[0], "GEOIPCHECK_*_ADDR ") {
		t.Errorf("problems=%q, want one for *_ADDR", got)
	}
}

func TestLoad_reportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"GEOIPCHECK_DB_POLL_INTERVAL": "0s",
		"GEOIPCHECK_DB_STALE_WARN":    "2h",
		"GEOIPCHECK_DB_STALE_ERROR":   "1h",
		"GEOIPCHECK_HTTP_ADDR":        "8080",
		"GEOIPCHECK_ADMIN_ADDR":       ":9090",
		"GEOIPCHECK_LOG_LEVEL":        "loud",
		"GEOIPCHECK_LOG_FORMAT":       "xml",
		"GEOIPCHECK_DRAIN_DELAY":      "-1s",
		"GEOIPCHECK_SHUTDOWN_TIMEOUT": "soon",
	}))

	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err=%v, want *Error", err)
	}
	// One problem per variable, each naming it, in a stable order.
	wantPrefixes := []string{
		"GEOIPCHECK_DB_POLL_INTERVAL ",
		"GEOIPCHECK_HTTP_ADDR ",
		"GEOIPCHECK_LOG_LEVEL ",
		"GEOIPCHECK_LOG_FORMAT ",
		"GEOIPCHECK_DRAIN_DELAY ",
		"GEOIPCHECK_SHUTDOWN_TIMEOUT ",
		"GEOIPCHECK_DB_PATH ",
		"GEOIPCHECK_DB_STALE_ERROR ",
		"GEOIPCHECK_*_ADDR ",
	}
	if len(cerr.Problems) != len(wantPrefixes) {
		t.Fatalf("problems=%q, want %d", cerr.Problems, len(wantPrefixes))
	}
	for i, p := range wantPrefixes {
		if !strings.HasPrefix(cerr.Problems[i], p) {
			t.Errorf("problem[%d]=%q, want prefix %q", i, cerr.Problems[i], p)
		}
	}
}
