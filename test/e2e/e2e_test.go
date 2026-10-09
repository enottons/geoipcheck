//go:build e2e

// Package e2e runs the whole service in-process through app.Run, with env
// config and real listeners on random ports. These tests own what only
// the assembled binary shows: config errors and exit codes, startup
// without a database, hot reload seen through the API, both transports
// answering, and the shutdown sequence. Request-level behavior is owned by
// the unit and integration tests.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/enottons/geoipcheck/api/proto/geoipcheck/v1"
	"github.com/enottons/geoipcheck/internal/app"
)

const (
	liteDB   = "../../testdata/GeoLite2-Country-Test.mmdb"
	geoip2DB = "../../testdata/GeoIP2-Country-Test.mmdb"

	// gbIP is GB in both test databases. usIP is US in GeoLite2 and has no
	// country in GeoIP2, so swapping the files changes its answer.
	gbIP = "81.2.69.142"
	usIP = "214.78.0.1"

	waitTimeout = 5 * time.Second
)

// logBuffer is the service's stdout. Run writes from many goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	data := bytes.Clone(b.buf.Bytes())
	b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

type service struct {
	t      *testing.T
	stdout *logBuffer
	cancel context.CancelFunc
	exit   chan int
}

// start runs the service with test-friendly defaults: random ports, fast
// polling, a short drain. vars override them.
func start(t *testing.T, vars map[string]string) *service {
	t.Helper()
	env := map[string]string{
		"GEOIPCHECK_HTTP_ADDR":        "127.0.0.1:0",
		"GEOIPCHECK_GRPC_ADDR":        "127.0.0.1:0",
		"GEOIPCHECK_ADMIN_ADDR":       "127.0.0.1:0",
		"GEOIPCHECK_DB_POLL_INTERVAL": "50ms",
		"GEOIPCHECK_DRAIN_DELAY":      "0s",
		"GEOIPCHECK_SHUTDOWN_TIMEOUT": "2s",
	}
	for k, v := range vars {
		env[k] = v
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &service{t: t, stdout: &logBuffer{}, cancel: cancel, exit: make(chan int, 1)}
	go func() { s.exit <- app.Run(ctx, func(k string) string { return env[k] }, s.stdout) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.exit:
		case <-time.After(waitTimeout):
			t.Error("service did not exit after cancel")
		}
	})
	return s
}

// waitExit returns the exit code, failing if Run doesn't return in time.
func (s *service) waitExit() int {
	s.t.Helper()
	select {
	case code := <-s.exit:
		s.exit <- code // let Cleanup see it too
		return code
	case <-time.After(waitTimeout):
		s.t.Fatal("service did not exit")
		return -1
	}
}

// waitLog returns the first log line with msg for which match is true.
func (s *service) waitLog(msg string, match func(map[string]any) bool) map[string]any {
	s.t.Helper()
	var line map[string]any
	eventually(s.t, "log "+msg, func() bool {
		for _, l := range s.stdout.lines(s.t) {
			if l["msg"] == msg && (match == nil || match(l)) {
				line = l
				return true
			}
		}
		return false
	})
	return line
}

func (s *service) addr(listener string) string {
	s.t.Helper()
	l := s.waitLog("listening", func(l map[string]any) bool { return l["listener"] == listener })
	addr, ok := l["addr"].(string)
	if !ok {
		s.t.Fatalf("listening line without addr: %v", l)
	}
	return addr
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var client = &http.Client{Timeout: 2 * time.Second}

func (s *service) readyStatus() int {
	s.t.Helper()
	resp, err := client.Get("http://" + s.addr("admin") + "/readyz")
	if err != nil {
		s.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

type checkResponse struct {
	Country  *string `json:"country"`
	Decision string  `json:"decision"`
	Reason   string  `json:"reason"`
}

func (s *service) checkHTTP(ip string, allowed ...string) (int, checkResponse) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"ip": ip, "allowed_countries": allowed})
	resp, err := client.Post("http://"+s.addr("http")+"/v1/country-checks", "application/json", bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out checkResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			s.t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func (s *service) grpcConn() *grpc.ClientConn {
	s.t.Helper()
	conn, err := grpc.NewClient(s.addr("grpc"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func rpcCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// installDB copies src over dst the way geoipupdate does: write a temp
// file, then rename. The mtime is pushed forward so a replacement is seen
// even on filesystems with coarse timestamps.
func installDB(t *testing.T, src, dst string, mtime time.Time) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	// Windows can briefly refuse a rename over a file another handle just
	// read. Linux succeeds first try.
	eventually(t, "rename "+dst, func() bool { return os.Rename(tmp, dst) == nil })
}

func TestE2E_configErrorsExitNonZeroAndListEveryProblem(t *testing.T) {
	s := start(t, map[string]string{
		"GEOIPCHECK_LOG_LEVEL":      "loud",
		"GEOIPCHECK_DB_STALE_WARN":  "2h",
		"GEOIPCHECK_DB_STALE_ERROR": "1h",
	})

	if code := s.waitExit(); code != app.ExitConfig {
		t.Fatalf("exit=%d, want %d", code, app.ExitConfig)
	}
	// The exact wording is the config unit tests' job. This checks that
	// every problem reaches the log, not just the first.
	line := s.waitLog("invalid configuration", nil)
	problems := fmt.Sprint(line["problems"])
	for _, name := range []string{"GEOIPCHECK_DB_PATH", "GEOIPCHECK_LOG_LEVEL", "GEOIPCHECK_DB_STALE_ERROR"} {
		if !strings.Contains(problems, name) {
			t.Errorf("problems=%s, want one naming %s", problems, name)
		}
	}
}

func TestE2E_portInUseExitsNonZero(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	s := start(t, map[string]string{
		"GEOIPCHECK_DB_PATH":   liteDB,
		"GEOIPCHECK_HTTP_ADDR": busy.Addr().String(),
	})

	if code := s.waitExit(); code != app.ExitFailed {
		t.Fatalf("exit=%d, want %d", code, app.ExitFailed)
	}
	s.waitLog("exiting after failure", func(l map[string]any) bool {
		msg, _ := l["error"].(string)
		return strings.Contains(msg, "listen http")
	})
}

func TestE2E_startsNotReadyThenServesBothTransportsWhenTheDatabaseAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	s := start(t, map[string]string{"GEOIPCHECK_DB_PATH": path})
	health := healthpb.NewHealthClient(s.grpcConn())

	// A missing file is not fatal: the process stays up and not ready.
	if got := s.readyStatus(); got != http.StatusServiceUnavailable {
		t.Errorf("readyz before the file exists=%d, want 503", got)
	}

	installDB(t, liteDB, path, time.Now())

	eventually(t, "ready", func() bool { return s.readyStatus() == http.StatusOK })
	if resp, err := health.Check(rpcCtx(t), &healthpb.HealthCheckRequest{}); err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("grpc health=%v, %v; want SERVING", resp, err)
	}
	if code, got := s.checkHTTP(gbIP, "GB"); code != http.StatusOK || got.Decision != "allowed" {
		t.Errorf("http check=%d %+v, want 200 allowed", code, got)
	}
	resp, err := pb.NewCountryCheckServiceClient(s.grpcConn()).CheckCountry(rpcCtx(t),
		&pb.CheckCountryRequest{Ip: gbIP, AllowedCountries: []string{"GB"}})
	if err != nil || resp.GetDecision() != pb.Decision_DECISION_ALLOWED {
		t.Errorf("grpc check=%v, %v; want ALLOWED", resp, err)
	}
}

func TestE2E_hotReloadIsVisibleThroughTheAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	base := time.Now().Add(-time.Hour)
	installDB(t, liteDB, path, base)
	s := start(t, map[string]string{"GEOIPCHECK_DB_PATH": path})
	eventually(t, "ready", func() bool { return s.readyStatus() == http.StatusOK })

	if _, got := s.checkHTTP(usIP, "US"); got.Decision != "allowed" {
		t.Fatalf("before reload: %+v, want allowed", got)
	}

	installDB(t, geoip2DB, path, base.Add(time.Minute))

	eventually(t, "reloaded answer", func() bool {
		_, got := s.checkHTTP(usIP, "US")
		return got.Decision == "unknown" && got.Reason == "country_unresolved" && got.Country == nil
	})
}

func TestE2E_shutdownDrainsWhileServingThenExitsCleanly(t *testing.T) {
	s := start(t, map[string]string{
		"GEOIPCHECK_DB_PATH": liteDB,
		// Long enough that the checks below land inside it on a slow CI box.
		"GEOIPCHECK_DRAIN_DELAY": "3s",
	})
	eventually(t, "ready", func() bool { return s.readyStatus() == http.StatusOK })
	apiAddr := s.addr("http")
	conn := s.grpcConn()

	s.cancel()

	// During the drain the replica says not ready on both health
	// endpoints but still answers, so requests routed before the load
	// balancer notices still succeed.
	eventually(t, "not ready", func() bool { return s.readyStatus() == http.StatusServiceUnavailable })
	health, err := healthpb.NewHealthClient(conn).Check(rpcCtx(t), &healthpb.HealthCheckRequest{})
	if err != nil || health.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Errorf("grpc health during drain=%v, %v; want NOT_SERVING", health, err)
	}
	if code, _ := s.checkHTTP(gbIP, "GB"); code != http.StatusOK {
		t.Errorf("http check during drain=%d, want 200", code)
	}
	if _, err := pb.NewCountryCheckServiceClient(conn).CheckCountry(rpcCtx(t),
		&pb.CheckCountryRequest{Ip: gbIP, AllowedCountries: []string{"GB"}}); err != nil {
		t.Errorf("grpc check during drain: %v", err)
	}

	if code := s.waitExit(); code != app.ExitOK {
		t.Fatalf("exit=%d, want 0", code)
	}
	s.waitLog("shutdown complete", nil)
	if conn, err := net.DialTimeout("tcp", apiAddr, time.Second); err == nil {
		_ = conn.Close()
		t.Error("API port still accepts connections after exit")
	}
}
