// Package config reads the service configuration from GEOIPCHECK_*
// environment variables. Load reports every problem at once, so a bad
// deploy shows the whole list in one crash log instead of one per restart.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// Prefix is prepended to every variable name.
const Prefix = "GEOIPCHECK_"

// Log formats.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the validated service configuration.
type Config struct {
	DBPath         string
	DBPollInterval time.Duration
	// StaleWarn and StaleError are database ages that trigger a warn or
	// error log. Zero disables a threshold.
	StaleWarn  time.Duration
	StaleError time.Duration

	HTTPAddr  string
	GRPCAddr  string
	AdminAddr string

	LogLevel  slog.Level
	LogFormat string

	// DrainDelay is how long the replica reports not ready, while still
	// serving, before it stops accepting requests. It covers the time the
	// load balancer takes to drop the endpoint.
	DrainDelay time.Duration
	// ShutdownTimeout bounds the graceful stop that follows DrainDelay.
	// The orchestrator's kill deadline (terminationGracePeriodSeconds in
	// Kubernetes) must exceed DrainDelay + ShutdownTimeout plus a few
	// seconds, or the process is killed mid-drain.
	ShutdownTimeout time.Duration
}

// Defaults. The staleness thresholds match MaxMind's twice-weekly
// GeoLite2 releases: one missed week warns, two missed weeks is an error.
const (
	defaultHTTPAddr        = ":8080"
	defaultGRPCAddr        = ":9090"
	defaultAdminAddr       = ":8081"
	defaultDBPollInterval  = 60 * time.Second
	defaultStaleWarn       = 7 * 24 * time.Hour
	defaultStaleError      = 14 * 24 * time.Hour
	defaultDrainDelay      = 5 * time.Second
	defaultShutdownTimeout = 15 * time.Second
)

// Error lists every invalid setting.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// Load reads the configuration through getenv, which is os.Getenv in
// production. An empty value means unset and takes the default.
func Load(getenv func(string) string) (Config, error) {
	p := &parser{getenv: getenv}
	cfg := Config{
		DBPath:          p.str("DB_PATH", ""),
		DBPollInterval:  p.positiveDuration("DB_POLL_INTERVAL", defaultDBPollInterval),
		StaleWarn:       p.nonNegativeDuration("DB_STALE_WARN", defaultStaleWarn),
		StaleError:      p.nonNegativeDuration("DB_STALE_ERROR", defaultStaleError),
		HTTPAddr:        p.addr("HTTP_ADDR", defaultHTTPAddr),
		GRPCAddr:        p.addr("GRPC_ADDR", defaultGRPCAddr),
		AdminAddr:       p.addr("ADMIN_ADDR", defaultAdminAddr),
		LogLevel:        p.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:       p.oneOf("LOG_FORMAT", LogFormatJSON, LogFormatJSON, LogFormatText),
		DrainDelay:      p.nonNegativeDuration("DRAIN_DELAY", defaultDrainDelay),
		ShutdownTimeout: p.positiveDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
	}

	if cfg.DBPath == "" {
		p.fail("DB_PATH", "is required")
	}
	// With StaleError <= StaleWarn the warn level would never be logged.
	if cfg.StaleWarn > 0 && cfg.StaleError > 0 && cfg.StaleError <= cfg.StaleWarn {
		p.fail("DB_STALE_ERROR", fmt.Sprintf("(%v) must be greater than %sDB_STALE_WARN (%v)",
			cfg.StaleError, Prefix, cfg.StaleWarn))
	}
	if addrs := []string{cfg.HTTPAddr, cfg.GRPCAddr, cfg.AdminAddr}; hasDuplicatePort(addrs) {
		p.fail("*_ADDR", fmt.Sprintf("HTTP, gRPC and admin addresses must differ, got %s", strings.Join(addrs, ", ")))
	}

	if len(p.problems) > 0 {
		return Config{}, &Error{Problems: p.problems}
	}
	return cfg, nil
}

type parser struct {
	getenv   func(string) string
	problems []string
}

func (p *parser) fail(name, msg string) {
	p.problems = append(p.problems, Prefix+name+" "+msg)
}

func (p *parser) str(name, def string) string {
	if v := strings.TrimSpace(p.getenv(Prefix + name)); v != "" {
		return v
	}
	return def
}

func (p *parser) duration(name string, def time.Duration) (time.Duration, bool) {
	raw := p.str(name, "")
	if raw == "" {
		return def, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.fail(name, fmt.Sprintf("%q is not a duration (e.g. 60s, 168h)", raw))
		return 0, false
	}
	return d, true
}

func (p *parser) positiveDuration(name string, def time.Duration) time.Duration {
	d, ok := p.duration(name, def)
	if ok && d <= 0 {
		p.fail(name, fmt.Sprintf("must be positive, got %v", d))
	}
	return d
}

func (p *parser) nonNegativeDuration(name string, def time.Duration) time.Duration {
	d, ok := p.duration(name, def)
	if ok && d < 0 {
		p.fail(name, fmt.Sprintf("must not be negative, got %v", d))
	}
	return d
}

// addr requires a numeric port so a typo fails here, as a config error,
// rather than at bind time as a runtime failure.
func (p *parser) addr(name, def string) string {
	a := p.str(name, def)
	_, port, err := net.SplitHostPort(a)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		p.fail(name, fmt.Sprintf("%q is not host:port with a port from 0 to 65535 (e.g. :8080)", a))
	}
	return a
}

func (p *parser) level(name string, def slog.Level) slog.Level {
	raw := p.str(name, "")
	if raw == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(raw)); err != nil {
		p.fail(name, fmt.Sprintf("%q is not one of debug, info, warn, error", raw))
	}
	return l
}

func (p *parser) oneOf(name, def string, allowed ...string) string {
	v := strings.ToLower(p.str(name, def))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.fail(name, fmt.Sprintf("%q is not one of %s", v, strings.Join(allowed, ", ")))
	return def
}

// hasDuplicatePort reports two addresses on the same port. Port 0 (pick
// any) never collides. Different hosts on one port are legal but not a
// setup this service needs, so they are rejected too.
// Ports compare as numbers, so ":80" and ":080" collide.
func hasDuplicatePort(addrs []string) bool {
	seen := map[uint64]bool{}
	for _, a := range addrs {
		_, raw, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			continue
		}
		if seen[port] {
			return true
		}
		seen[port] = true
	}
	return false
}
