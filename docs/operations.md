# Operations

How the data stays fresh, what to watch, what to alert on, and what to do
when an alert fires.

## Keeping the database fresh

MaxMind publishes GeoLite2-Country twice a week, on Tuesdays and Fridays.
The service never downloads anything. Something else puts a file at
`GEOIPCHECK_DB_PATH`, and the service picks it up within one poll without a
restart.

### Who downloads

`geoipupdate`, MaxMind's official updater, with a free account ID and
license key. One downloader per environment:

| Environment | How |
|---|---|
| Local | `deploy/compose.yaml`: a `geoipupdate` container with `GEOIPUPDATE_FREQUENCY=24` shares a volume with the service. |
| Kubernetes | `deploy/k8s/cronjob.yaml`: a daily CronJob writes to an RWX PVC that every pod mounts read-only ([design.md](design.md) D11). |
| No RWX storage | A `geoipupdate` sidecar per pod. It costs license quota per replica ([deploy/k8s/README.md](../deploy/k8s/README.md)). |

Daily is enough. A missed day costs nothing, since the data changes twice a
week and the staleness thresholds are in weeks. GeoLite allows 30 downloads
per day per account. One CronJob uses one, or up to four on a day its
retries run out.

The database is never committed or baked into the image. The GeoLite2 EULA
requires deleting outdated copies within 30 days, and it's easier to meet
that with one live file than with images in a registry.

### How the service picks it up

The watcher stats the file every `GEOIPCHECK_DB_POLL_INTERVAL` (60s). When
the size or mtime changes, it reads the whole file, parses it, and swaps it
in atomically. In-flight lookups finish on the old copy. Nothing locks
([design.md](design.md) D8, D9).

| Situation | What happens |
|---|---|
| No file at startup | Pod runs but stays not ready (`/readyz` 503, API 503 `database_unavailable`). It goes ready on the first poll after the file appears. |
| File replaced with a valid DB | Swapped in. Logs `geoip database loaded` with `build_time` and `previous_build_time`. |
| File replaced with a corrupt or truncated DB | Keeps serving the previous DB. Logs `geoip database load failed` and increments `geoipcheck_db_reloads_total{result="failure"}`. It retries when the file changes again. An unchanged bad file is not re-read every minute. |
| File deleted | Keeps serving the previous DB. Logs `geoip database file unavailable` once, not every poll. |

`geoipupdate` writes to a temp file and renames it, so the service never
sees a half-written file. If something else copies the file non-atomically,
the watcher may catch it mid-write. The load then fails, and the next poll
sees a new size and retries.

### Staleness

Age is measured from the database's own `build_time`, not the file mtime.
A copy of an old file doesn't look fresh.

| Age | Default | Effect |
|---|---|---|
| over `GEOIPCHECK_DB_STALE_WARN` | 7 days | `geoip database is stale` at warn level |
| over `GEOIPCHECK_DB_STALE_ERROR` | 14 days | same message at error level |

Each level is logged once, when the database crosses into it. A stale
database never makes the pod not ready ([design.md](design.md) D10). If
every pod went not ready during a MaxMind outage, the gateway would lose the
whole service to protect against two-week-old data. The alert belongs on the
metric: `time() - geoipcheck_db_build_timestamp_seconds` (see Alerts below).

Normal age is 0–5 days: releases come twice a week, and the daily CronJob
adds up to a day of lag. 7 days means at least one release was missed.

## Ports

| Port | Serves | Exposed to |
|---|---|---|
| 8080 | HTTP API | gateway |
| 9090 | gRPC API + `grpc.health.v1` | gateway |
| 8081 | `/healthz`, `/readyz`, `/metrics` | kubelet, Prometheus |

Admin is on its own port so the NetworkPolicy can keep `/metrics` away from
API clients, and so an API-port overload can't fail the liveness probe.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `geoipcheck_checks_total` | counter | `transport`, `decision`, `reason` | Successful checks. Every combination is pre-created at 0. |
| `geoipcheck_request_duration_seconds` | histogram | `transport`, `operation`, `code` | Every request, including rejected ones. `code` is the HTTP status (`"200"`, `"499"`, `"aborted"`) or the gRPC code name (`"InvalidArgument"`). `operation` is `check_country` or `unmatched`. |
| `geoipcheck_db_loaded` | gauge | | 1 once a database is serving. It stays 1 if a later reload fails, since the old one keeps serving. |
| `geoipcheck_db_build_timestamp_seconds` | gauge | | `build_time` of the serving database. 0 before the first load. |
| `geoipcheck_db_reloads_total` | counter | `result` = `success` \| `failure` | Load attempts. Both pre-created. |
| `geoipcheck_build_info` | gauge | `version`, `revision`, `goversion` | Always 1. |

Health probes are not timed or logged. They'd drown out real traffic.

## Alerts

Starting points. Tune the thresholds on real traffic.

```yaml
groups:
  - name: geoipcheck
    rules:
      # A pod that never loaded a DB is not ready, so this means capacity
      # loss, not wrong answers.
      - alert: GeoipcheckNoDatabase
        expr: geoipcheck_db_loaded == 0
        for: 10m
        labels: {severity: page}

      # Normal age is 0-5 days (twice-weekly releases plus up to a day of
      # CronJob lag). 10 days means at least two missed releases.
      - alert: GeoipcheckDatabaseStale
        expr: (time() - geoipcheck_db_build_timestamp_seconds) > 10 * 86400
          and geoipcheck_db_build_timestamp_seconds > 0
        for: 1h
        labels: {severity: ticket}

      # Pods keep serving the old DB, so this is a ticket, not a page. A bad
      # file is tried once, so this resolves after an hour even if the file
      # is still bad. GeoipcheckDatabaseStale catches it if nobody fixes it.
      - alert: GeoipcheckReloadFailing
        expr: increase(geoipcheck_db_reloads_total{result="failure"}[1h]) > 0
        labels: {severity: ticket}

      # 4xx, 499 and aborted are excluded: they are the caller's problem.
      # One selector joined with `or`, not two sums added: the latency
      # series aren't pre-created, and a sum over nothing is empty, which
      # would silence the HTTP half until the first gRPC error.
      - alert: GeoipcheckErrors
        expr: |
          sum(
            rate(geoipcheck_request_duration_seconds_count{transport="http",code=~"5.."}[5m])
            or rate(geoipcheck_request_duration_seconds_count{transport="grpc",code=~"Internal|Unavailable|DeadlineExceeded|Unknown|DataLoss"}[5m])
          ) > 0.01 * sum(rate(geoipcheck_request_duration_seconds_count[5m]))
        for: 5m
        labels: {severity: page}

      - alert: GeoipcheckSlow
        expr: |
          histogram_quantile(0.99, sum by (le) (
            rate(geoipcheck_request_duration_seconds_bucket{operation="check_country"}[5m]))) > 0.025
        for: 10m
        labels: {severity: ticket}
```

Not alerted on but worth a dashboard panel: the `unknown` share by reason.
A jump in `non_routable_address` almost always means a proxy in front of the
gateway started sending its own address. A jump in `country_unresolved`
after a reload means MaxMind changed coverage.

```promql
sum by (reason) (rate(geoipcheck_checks_total{decision="unknown"}[5m]))
  / ignoring(reason) group_left sum(rate(geoipcheck_checks_total[5m]))
```

## Runbook

**GeoipcheckNoDatabase.** Pods are not ready. Check the logs for
`geoip database file unavailable` (the file isn't there) or `geoip database
load failed` (it's there but bad). For a missing file, check the last
CronJob run (`kubectl -n geoipcheck get jobs`) and its logs. A bad license
key and an exhausted download quota both show up there. Seed manually with
`kubectl -n geoipcheck create job geoipupdate-manual --from=cronjob/geoipupdate`.

**GeoipcheckDatabaseStale.** Pods are serving, with old data. Same CronJob
checks. If MaxMind itself is down, there's nothing to do. The service keeps
answering from the old copy by design ([design.md](design.md) D10).

**GeoipcheckReloadFailing.** The file on the volume changed and doesn't
parse. The `geoip database load failed` line has the error. The alert
resolves on its own after an hour, so check the latest load line, not the
alert state, before closing the ticket. Usually a truncated download or a
non-atomic copy. Running the CronJob again replaces the file, and the pods
pick it up. A failed file isn't re-read until it changes, so there's no
retry storm.

**GeoipcheckErrors.** Group HTTP logs by `error_code` and gRPC logs by
`grpc_code`. gRPC sets `error_code` only for validation errors and
`database_unavailable`.

- `database_unavailable` / `Unavailable`: a not-ready pod is getting
  traffic. That's a load balancer problem (see gRPC load balancing below).
- `internal` / `Internal`: a bug. The log line has the `error`, and a panic
  has its own `panic serving request` line with the stack. A gRPC `Internal`
  with no `error` field and no `decision` is grpc-go rejecting a malformed
  message, which is a client bug.
- `DeadlineExceeded`: the lookup failed after the caller's deadline had
  passed. Lookups don't check the deadline, so this only happens with no
  database loaded. Treat it like `Unavailable`.

**GeoipcheckSlow.** Same as above. A full check takes about 1 µs
in-process, so p99 over 25 ms is CPU or GC, not the database.

## Logs

JSON on stdout, one line per request. The client IP is never logged in full,
only `client_ip_prefix` (/24 for IPv4, /48 for IPv6). The allowlist is
logged only as `allowlist_size`.

| Message | Level | Fields |
|---|---|---|
| `http request` | info; error for 5xx and aborted | `request_id`, `method`, `path`, `duration`, `status`, `allowlist_size`, `client_ip_prefix`, `country`, `decision`, `reason`, `error_code`, `error`, `aborted` |
| `grpc request` | info; error for `Internal`, `Unavailable`, `DeadlineExceeded`, `Unknown`, `DataLoss` | `request_id`, `grpc_method`, `grpc_code`, `duration`, then as HTTP |
| `geoip database loaded` | info | `path`, `type`, `build_time`, `size_bytes`, `previous_build_time` |
| `geoip database load failed` | error | `path`, `serving_previous`, `error` |
| `geoip database file unavailable` | warn, once per outage | `path`, `serving_previous`, `error` |
| `geoip database is stale` | warn, then error | `path`, `build_time`, `age` |
| `panic serving request` | error | `request_id`, `panic`, `stack` |
| `starting`, `listening` | info | `version`, `revision`, `db_path`; `listener` and `addr` per listener |
| `invalid configuration` | error | `problems`: every bad variable at once. Exit code 2. |
| `shutdown started; readiness is now false` | info | `cause`: `signal`, or the error that stopped the service; `drain_delay` |
| `drain delay over; stopping API servers`, `shutdown complete` | info | `timeout`; `duration` |
| `graceful stop timed out; closing connections` | warn | `listener`; `error` on HTTP. Requests were cut; raise the timeout or find the slow call. |
| `exiting after failure` | error | `error`, e.g. a port in use. Exit code 1. |

Fields after `duration` appear only when they apply. A malformed request has
no `decision`; a successful one has no `error_code`.

`duration` is nanoseconds in JSON. On Windows the clock is coarse enough
that fast requests log 0.

## Shutdown

On SIGTERM:

1. `/readyz` and gRPC health go to not ready. Both APIs keep serving.
2. Wait `GEOIPCHECK_DRAIN_DELAY` (5s) so endpoints and client-side load
   balancers drop the pod.
3. HTTP `Shutdown` and gRPC `GracefulStop` run in parallel, bounded by
   `GEOIPCHECK_SHUTDOWN_TIMEOUT` (15s). In-flight requests finish. Anything
   left after the timeout is cut.
4. The admin server stops last, within one second.

`terminationGracePeriodSeconds` must exceed drain + shutdown with margin.
The manifest uses 30s against 20s. A second SIGTERM exits immediately.

## gRPC load balancing

gRPC holds one HTTP/2 connection and sends every call over it. Behind a
plain ClusterIP Service, a gateway pod sticks to one geoipcheck pod, and new
replicas from the HPA get no traffic.

The server already sets `MaxConnectionAge` to 5 minutes, with 30s grace for
in-flight calls. Every client reconnects at least that often, so load
spreads to new replicas within about 5 minutes with no client changes. If
that's too slow:

- **Client-side:** add a headless Service (`clusterIP: None`; not in the
  manifests today) and dial
  `dns:///geoipcheck-headless.geoipcheck:9090` with the `round_robin`
  policy. The client then balances per call across every pod it resolved.
  It re-resolves when a connection closes, which `MaxConnectionAge`
  guarantees.
- **Mesh:** if the cluster runs Linkerd or Istio, the sidecar balances per
  call and nothing else changes.

HTTP/1.1 clients with keep-alive have a milder version of the same problem.
The drain delay covers the shutdown case.

## Capacity

One core does about 1M checks per second in-process (README benchmarks).
Network, TLS at the gateway, and JSON dominate. The manifest requests 100m CPU and 64Mi memory,
limits memory to 128Mi, and sets no CPU limit, so pods aren't throttled at
the p99. Memory is about two copies of the database (about 9 MB each) during
a reload plus the Go runtime.
