// Package app wires the service together and owns its lifecycle: config,
// logger, metrics, database watcher, the three listeners, and shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/enottons/geoipcheck/internal/admin"
	"github.com/enottons/geoipcheck/internal/apimetrics"
	"github.com/enottons/geoipcheck/internal/config"
	"github.com/enottons/geoipcheck/internal/countrycheck"
	"github.com/enottons/geoipcheck/internal/geoip"
	"github.com/enottons/geoipcheck/internal/grpcapi"
	"github.com/enottons/geoipcheck/internal/httpapi"
)

// Exit codes. Config errors are separate so a crash-looping pod says
// "fix the deploy" rather than "something broke".
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitConfig = 2
)

// HTTP server limits. The API answers in microseconds; 5s only allows for
// a slow client network. Idle connections from the gateway are reused.
const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 5 * time.Second
	httpWriteTimeout      = 5 * time.Second
	httpIdleTimeout       = 60 * time.Second
)

// adminShutdownTimeout covers an in-flight probe or scrape. Both answer in
// milliseconds, so this is kept out of SHUTDOWN_TIMEOUT's budget math.
const adminShutdownTimeout = time.Second

// Run starts the service and blocks until ctx is cancelled (SIGTERM) or a
// server fails, then shuts down. getenv is os.Getenv in production; logs
// go to stdout. The return value is the process exit code.
//
// Config errors are fatal. Data errors are not: a missing or corrupt
// database leaves the service running, not ready, and polling.
func Run(ctx context.Context, getenv func(string) string, stdout io.Writer) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(stdout, nil))
		var cerr *config.Error
		if errors.As(err, &cerr) {
			logger.Error("invalid configuration", slog.Any("problems", cerr.Problems))
		} else {
			logger.Error("invalid configuration", slog.String("error", err.Error()))
		}
		return ExitConfig
	}

	logger := newLogger(cfg, stdout)
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("exiting after failure", slog.String("error", err.Error()))
		return ExitFailed
	}
	return ExitOK
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == config.LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

type servers struct {
	api   *http.Server
	admin *http.Server
	grpc  *grpc.Server
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	build := readBuildInfo()
	logger.Info("starting",
		slog.String("version", build.version),
		slog.String("revision", build.revision),
		slog.String("db_path", cfg.DBPath))

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build.collector(),
	)

	reader := &geoip.Reader{}
	watcher := geoip.NewWatcher(geoip.WatcherConfig{
		Path:       cfg.DBPath,
		Reader:     reader,
		Metrics:    geoip.NewMetrics(reg),
		Logger:     logger,
		StaleWarn:  cfg.StaleWarn,
		StaleError: cfg.StaleError,
	})
	readiness := admin.NewReadiness(func() bool { return reader.Current() != nil })
	checker := countrycheck.NewService(reader)
	apiMetrics := apimetrics.New(reg)

	// Not ctx: binding is instant, and a SIGTERM that lands mid-bind should
	// go through the normal shutdown path, not fail startup.
	lis, err := listen(context.Background(), cfg)
	if err != nil {
		return err
	}
	srv := servers{
		api: newHTTPServer(httpapi.NewHandler(httpapi.Config{
			Checker: checker,
			Metrics: apiMetrics,
			Logger:  logger,
		}), logger),
		admin: newHTTPServer(admin.NewHandler(admin.Config{
			Readiness: readiness,
			Gatherer:  reg,
			Logger:    logger,
		}), logger),
		grpc: grpcapi.NewServer(grpcapi.Config{
			Checker:   checker,
			Readiness: readiness,
			Metrics:   apiMetrics,
			Logger:    logger,
		}),
	}
	// The bound address, not the configured one: with port 0 it is the
	// only place the real port shows up.
	for _, l := range []struct {
		name string
		l    net.Listener
	}{{"http", lis.api}, {"grpc", lis.grpc}, {"admin", lis.admin}} {
		logger.Info("listening", slog.String("listener", l.name), slog.String("addr", l.l.Addr().String()))
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serveHTTP("http", srv.api, lis.api) })
	g.Go(func() error { return serveHTTP("admin", srv.admin, lis.admin) })
	g.Go(func() error {
		// ErrServerStopped means shutdown won the race to the server
		// before Serve started. That is a clean stop, not a failure.
		if err := srv.grpc.Serve(lis.grpc); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc server: %w", err)
		}
		return nil
	})
	// Wait also waits for the watcher. It stops between polls, so a
	// reload stuck on a hung volume holds the exit until the orchestrator
	// kills the process. I'd rather see that than abandon the goroutine.
	g.Go(func() error {
		ticker := time.NewTicker(cfg.DBPollInterval)
		defer ticker.Stop()
		watcher.Run(gctx, ticker.C)
		return nil
	})
	// gctx ends on SIGTERM or when any server fails. Both paths drain: on
	// a failure the other transport is still serving traffic the load
	// balancer routed here.
	g.Go(func() error {
		<-gctx.Done()
		shutdown(cfg, logger, readiness, srv, shutdownCause(ctx, gctx))
		return nil
	})
	return g.Wait()
}

// shutdownCause names what started the shutdown, for the log. If a server
// fails and a signal lands before this runs, it says "signal"; the exit
// code still reflects the failure, since Wait returns it.
func shutdownCause(parent, gctx context.Context) string {
	if parent.Err() != nil {
		return "signal"
	}
	return context.Cause(gctx).Error()
}

type listeners struct {
	api, grpc, admin net.Listener
}

// listen binds all three ports before anything serves, so a port conflict
// fails startup instead of leaving a half-running process.
func listen(ctx context.Context, cfg config.Config) (listeners, error) {
	var lc net.ListenConfig
	var opened []net.Listener
	bind := func(name, addr string) (net.Listener, error) {
		l, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, o := range opened {
				_ = o.Close()
			}
			return nil, fmt.Errorf("listen %s on %s: %w", name, addr, err)
		}
		opened = append(opened, l)
		return l, nil
	}
	var ls listeners
	var err error
	if ls.api, err = bind("http", cfg.HTTPAddr); err != nil {
		return listeners{}, err
	}
	if ls.grpc, err = bind("grpc", cfg.GRPCAddr); err != nil {
		return listeners{}, err
	}
	if ls.admin, err = bind("admin", cfg.AdminAddr); err != nil {
		return listeners{}, err
	}
	return ls, nil
}

func newHTTPServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		// net/http logs connection-level errors (bad TLS, broken
		// clients) here; route them into the structured log.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

func serveHTTP(name string, srv *http.Server, l net.Listener) error {
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

// shutdown drains then stops the servers:
//  1. Readiness goes false, so /readyz and gRPC health report not ready
//     while requests are still served.
//  2. Wait DrainDelay for the load balancer to drop this endpoint.
//  3. Stop the API servers gracefully within ShutdownTimeout, then force.
//  4. Stop admin last, so /readyz and /metrics answer until the end. It
//     gets its own short budget: step 3 may have used all of its own.
func shutdown(cfg config.Config, logger *slog.Logger, readiness *admin.Readiness, srv servers, cause string) {
	start := time.Now()
	readiness.StartDraining()
	logger.Info("shutdown started; readiness is now false",
		slog.String("cause", cause), slog.Duration("drain_delay", cfg.DrainDelay))
	time.Sleep(cfg.DrainDelay)

	logger.Info("drain delay over; stopping API servers", slog.Duration("timeout", cfg.ShutdownTimeout))
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		stopHTTP(ctx, logger, "http", srv.api)
	}()
	stopGRPC(ctx, logger, srv.grpc)
	<-done

	adminCtx, adminCancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
	defer adminCancel()
	stopHTTP(adminCtx, logger, "admin", srv.admin)

	logger.Info("shutdown complete", slog.Duration("duration", time.Since(start)))
}

func stopHTTP(ctx context.Context, logger *slog.Logger, name string, srv *http.Server) {
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("graceful stop timed out; closing connections",
			slog.String("listener", name), slog.String("error", err.Error()))
		_ = srv.Close()
	}
}

func stopGRPC(ctx context.Context, logger *slog.Logger, srv *grpc.Server) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		logger.Warn("graceful stop timed out; closing connections", slog.String("listener", "grpc"))
		srv.Stop()
		<-done
	}
}
