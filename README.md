# geoipcheck

A Go service that tells the API gateway whether a login IP is in one of a
customer's allowed countries. It serves HTTP and gRPC from a local MaxMind
GeoLite2-Country database that hot-reloads.

- [Problem](#problem)
- [Initial requirements](#initial-requirements)
- [Optional requirements](#optional-requirements)
- [API contracts](#api-contracts)
- [Alternate designs](#alternate-designs)
- [Running it](#running-it)

## Problem

Customers want to stop their users from logging in to the UI from countries
they haven't approved. The usual case is an employee quietly outsourcing
their work to someone abroad.

The flow is already designed. The customer database holds each customer's
allowed countries. The API gateway captures the requesting IP, looks up the
customer's restrictions, and calls a new service with both. This repo is that
service.

Phase 1 uses only the presented IP. VPNs are out of scope.

## Initial requirements

| Requirement | How it's met |
|---|---|
| HTTP API that takes an IP and a list of allowed countries | `POST /v1/country-checks` with `{ip, allowed_countries}` |
| Returns an indicator of whether the IP is in an allowed country | `decision`: `allowed`, `denied`, or `unknown`, plus a `reason` ([why three values](#why-three-answers-not-two)) |
| Uses MaxMind GeoLite2 for IP-to-country mapping | GeoLite2-Country `.mmdb`, read in-process |
| Go | Go 1.27, stdlib `net/http` and `log/slog` |
| Coding style and code organization | One package per responsibility under `internal/` ([design.md](docs/design.md) D6) |
| API design | Versioned path, strict validation, stable error codes, one contract across HTTP and gRPC |
| Operations: logging and error handling | One structured log line per request, RFC 9457 errors, metrics, health and readiness, graceful shutdown ([operations.md](docs/operations.md)) |

## Optional requirements

Each item below is what I'd say in the design meeting to justify the work.

### Keeping the mapping data up to date (planned and implemented)

GeoLite2 is republished twice a week. A database that never updates goes
wrong quietly: logins get allowed or denied on stale data, and nobody notices.
The service watches its database file and swaps in a new copy without a
restart. It also exposes the database's build time as a metric, so staleness
becomes an alert, not a surprise. Downloading is `geoipupdate`'s job: a
CronJob in Kubernetes, a companion container in compose. The service never
downloads anything. Full plan:
[operations.md, "Keeping the database fresh"](docs/operations.md#keeping-the-database-fresh).

### Dockerfile

This is how it ships. The multi-stage build produces a static binary on
distroless, running as non-root with no shell. That gives a small attack
surface for a service that sits in the login path.
[deploy/Dockerfile](deploy/Dockerfile).

### Kubernetes YAML

This is how it runs in the existing cluster. It deploys with
`kubectl apply -k`. HPA, PDB, NetworkPolicy, restricted Pod Security,
probes, and a drain-aware shutdown are included, so a node rotation doesn't
fail logins. [deploy/k8s/](deploy/k8s/README.md).

### gRPC alongside HTTP

The gateway calls this on every login. gRPC gives it a typed client
generated from the proto and long-lived HTTP/2 connections. Both transports
share one core, so the second one costs a thin adapter, not a second
implementation. [api/proto/geoipcheck/v1/checks.proto](api/proto/geoipcheck/v1/checks.proto).

### Other extensions

#### Why three answers, not two

The brief asks for an allowed/not-allowed indicator. Some IPs have no
answer: private addresses behind a misconfigured proxy, or public ranges
MaxMind can't place. If the service says "denied", it has decided for the
customer to fail closed. If it says "allowed", every unknown IP is a
loophole. `unknown` hands that policy back to the gateway, which knows the
customer. There's no extra `allowed` boolean: two fields that can disagree
are a bug waiting to happen (D2, D15).

#### Country codes validated against ISO 3166-1

`UK` is the most common bad input. It isn't an ISO code, and MaxMind returns
`GB`. A format-only check accepts `UK` and then silently denies every UK
user. The service rejects unknown codes and answers "did you mean GB" (D12).
A typo in a customer's allowlist surfaces as a 400 on the first login, not
as a support ticket a month later.

#### Request IDs and privacy-safe access logs

When a customer says "my user was blocked", support needs to find that one
request. Every request gets an ID, which is echoed back and logged on one
structured line with the decision. The client IP is logged only as its /24
or /48 prefix, so the logs don't become a store of user IPs (D14).

#### Metrics, health, and readiness

Without metrics, "is it working?" means reading logs. The service exports
decision counts, latency, and database age and reload results, and the
alerts are in [operations.md](docs/operations.md). A pod reports ready only
once it has a database, so it never serves 503s from inside the Service.

#### Graceful shutdown

On SIGTERM the pod goes not ready, keeps serving while the load balancer
drops it, then finishes in-flight requests. Deploys and node drains don't
fail logins.

#### CI

`-race`, the Docker build, and the manifest checks can't run on every dev
machine. CI runs all of them, plus lint, fuzzing, govulncheck, and a check
for breaking changes to the proto. [.github/workflows/ci.yml](.github/workflows/ci.yml).

Features I'd build next, and what would make each worth doing, are in
[docs/extensions.md](docs/extensions.md).

## API contracts

### HTTP: `POST /v1/country-checks`

Request (`Content-Type: application/json`, at most 8 KiB):

```json
{"ip":"81.2.69.142","allowed_countries":["US","GB"]}
```

| Field | Type | Rules |
|---|---|---|
| `ip` | string | IPv4 or IPv6. IPv4-mapped IPv6 is treated as IPv4. |
| `allowed_countries` | string[] | 1–250 ISO 3166-1 alpha-2 codes, plus `XK`. Case-insensitive. Duplicates are allowed but count toward the 250. |

Unknown fields are rejected, so a typo like `allowed_country` fails loudly
instead of being ignored.

Response (200). Every field is always present:

```json
{"ip":"81.2.69.142","country":"GB","decision":"allowed","reason":"country_allowed"}
```

| Field | Type | Meaning |
|---|---|---|
| `ip` | string | The IP as parsed. |
| `country` | string or `null` | Resolved country code. `null` when none was resolved. |
| `decision` | string | `allowed`, `denied`, or `unknown`. |
| `reason` | string | Why. See below. |

| `decision` | `reason` | When |
|---|---|---|
| `allowed` | `country_allowed` | Country resolved and in the list. |
| `denied` | `country_not_allowed` | Country resolved and not in the list. |
| `unknown` | `non_routable_address` | Private, loopback, link-local, CGNAT, documentation, or another non-public range. Teredo and NAT64 are included on purpose: the database would place them by an embedded IPv4 address that isn't the user's. No lookup is done. |
| `unknown` | `country_unresolved` | Public IP, but the database has no country for it. |

Branch on `decision`. `reason` is for logs, metrics, and support. New reasons
may be added under an existing decision. Treat an unrecognized `decision` as
`unknown`.

### Errors

HTTP errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)
`application/problem+json`:

```json
{"title":"Bad Request","status":400,
 "detail":"\"UK\" is not an ISO 3166-1 alpha-2 country code; did you mean \"GB\"?",
 "code":"invalid_country_code","field":"allowed_countries[0]","suggestion":"GB",
 "request_id":"QRIKQ2JFXL3XNHBNIKSR2X4JUS"}
```

`code` is stable; `detail` is for humans and may change. `field` and
`suggestion` appear when they apply.

| HTTP | gRPC | `code` | Meaning |
|---|---|---|---|
| 400 | `INVALID_ARGUMENT` | `invalid_ip` | `ip` doesn't parse. |
| 400 | `INVALID_ARGUMENT` | `invalid_country_code` | Not an assigned code. `UK` and common alpha-3 codes (`GBR`, `USA`, ...) get a `suggestion`. |
| 400 | `INVALID_ARGUMENT` | `allowed_countries_required` | Empty or missing list. Never treated as "allow all". |
| 400 | `INVALID_ARGUMENT` | `too_many_countries` | More than 250 entries. |
| 400 | — | `malformed_json`, `unknown_field` | Body isn't one valid JSON object with known fields. |
| 413 | `RESOURCE_EXHAUSTED` | `request_too_large` | Over 8 KiB. |
| 415 | — | `unsupported_media_type` | Not `application/json`. |
| 404 / 405 | `UNIMPLEMENTED` | `not_found` / `method_not_allowed` | Wrong path or method. |
| 503 | `UNAVAILABLE` | `database_unavailable` | No database loaded yet. `Retry-After: 5` / `RetryInfo`. |
| 504 | `DEADLINE_EXCEEDED` | `deadline_exceeded` | The lookup failed and the caller's deadline had passed. A lookup takes about a microsecond and doesn't check the deadline, so this only happens with no database loaded. HTTP carries no deadline, so in practice this is gRPC only. |
| 500 | `INTERNAL` | `internal` | Bug. Logged with the request ID. On gRPC, grpc-go also returns `INTERNAL` for a protobuf message it can't decode. |

On gRPC, the `code` travels in `google.rpc.ErrorInfo.reason` (domain
`geoipcheck`). Only the `INVALID_ARGUMENT` and `UNAVAILABLE` rows carry
`ErrorInfo`. Validation errors add `BadRequest` when one field is at fault;
`UNAVAILABLE` adds `RetryInfo`. For the other gRPC statuses, the status code
is the whole answer.

If the lookup fails after the HTTP client has disconnected, the request is
logged as 499, `client_closed_request`, not 503. The client never sees it,
and it stays out of 5xx alerts. On gRPC the same case logs as `Canceled`.
A client that disconnects during a normal check is logged with the status
the service wrote: the check finishes before the disconnect matters.

### Request IDs

Send `X-Request-ID` (HTTP) or `x-request-id` metadata (gRPC). It's on every
log line for the request. HTTP echoes it in the response header and in
problem bodies. gRPC echoes it in response header metadata, except for
calls grpc-go rejects before decoding (oversized or malformed messages).
An ID must be 1–128 visible ASCII characters. Otherwise, or if none is
sent, the service generates one.

### gRPC: `geoipcheck.v1.CountryCheckService/CheckCountry`

Defined in
[api/proto/geoipcheck/v1/checks.proto](api/proto/geoipcheck/v1/checks.proto).
It has the same fields and rules as HTTP. Decisions and reasons are enums
(`DECISION_ALLOWED`, `REASON_COUNTRY_ALLOWED`, ...).
`grpc.health.v1.Health` is served on the same port and follows `/readyz`.

```sh
grpcurl -plaintext -import-path api/proto -proto geoipcheck/v1/checks.proto \
  -H 'x-request-id: demo-2' -d '{"ip":"81.2.69.142","allowed_countries":["GB"]}' \
  localhost:9090 geoipcheck.v1.CountryCheckService/CheckCountry
```

There is no server reflection, so grpcurl needs the proto. It also needs the
googleapis protos to decode error details; without them it prints the
details as raw bytes.

### Gateway contract

What the gateway should do with each answer:

- **`allowed`**: let the login through.
- **`denied`**: block it.
- **`unknown`**: this is customer policy, not this service's. The gateway
  decides per customer whether to fail open or closed. `non_routable_address`
  usually means a misconfigured proxy chain in front of the gateway, so it
  is worth alerting on.
- **4xx**: a gateway bug or bad customer data. Don't retry.
  `invalid_country_code` means the stored allowlist is wrong.
- **503 / `UNAVAILABLE`**: retry on another replica after `Retry-After`. A
  replica only returns this before its first database load.
- **Timeouts**: a check takes about a microsecond in-process. A 100 ms
  client deadline leaves room for the network.

### Admin endpoints (`:8081`)

| Path | Meaning |
|---|---|
| `GET /healthz` | 200 while the process is up. For liveness. |
| `GET /readyz` | 200 when a database is loaded and the replica isn't shutting down; otherwise 503. |
| `GET /metrics` | Prometheus. Metrics and alerts are in [operations.md](docs/operations.md). |

## Alternate designs

The main decisions and what I rejected. The full reasoning, and the signal
that would make me revisit each one, is in [docs/design.md](docs/design.md).

| # | Decision | Chosen | Rejected, and why |
|---|---|---|---|
| D1 | Data source | GeoLite2-Country `.mmdb`, local file | MaxMind web service: 1,000 free lookups a day and a network hop per login. Postgres import: a stateful DB for 9 MB of static data. Baked into the image: the EULA forbids redistribution. |
| D2 | IPs with no country | `allowed` / `denied` / `unknown` + `reason` | Fail closed or fail open: either one hard-codes a policy that belongs to the customer. |
| D3 | HTTP shape | `POST /v1/country-checks`, JSON body | GET with query params: puts the IP in URL logs and is awkward to extend. Both: two contracts for one caller. |
| D4 | Service boundary | `{ip, allowed_countries}` in, decision out | Geolocation only: the gateway would redo the join. Read the customer DB: couples the service to customer schema and auth. |
| D5 | Transports | One core, thin HTTP and gRPC adapters, separate ports | grpc-gateway: forces proto-shaped JSON. cmux on one port: fragile protocol sniffing. |
| D6 | Code layout | One package per responsibility under `internal/` | Single `main` package, `handlers/services/models` layers, or formal ports/adapters naming: they hide where the logic is. |
| D7 | Go and router | Go 1.27, stdlib `ServeMux`, `log/slog` | chi: stdlib does method routing now, so the dependency buys nothing. |
| D8 | DB change detection | Poll mtime and size every 60s | fsnotify: NFS-backed RWX volumes don't deliver events for writes from another node. SIGHUP: the writer would have to signal every pod. Restart per update: daily rollouts for a data file. |
| D9 | DB in memory | Read into memory, swap with `atomic.Pointer` | mmap with ref counting: use-after-unmap risk and more code. RWMutex: a lock on every lookup for no gain. |
| D10 | Readiness | Ready once a DB is loaded; staleness is an alert | Ready only while fresh: a MaxMind outage would take every pod down at once. |
| D11 | DB in Kubernetes | One CronJob writes an RWX volume; pods mount it read-only | Sidecar per pod: multiplies license quota (kept as the fallback without RWX storage). Baked into the image or an init container: no hot reload. |
| D12 | Country codes | Assigned ISO 3166-1 list + `XK`, with "did you mean" | Format-only: accepts `UK` and silently denies every UK user. Warnings: they get ignored. |
| D13 | GeoLite field | `country` only; missing → `country_unresolved` | Fall back to `registered_country`: that's where the block is registered, not where the user is. |
| D14 | Correlation | Request ID on both transports | Also log `customer_id`: customer data in logs, and the gateway already has the join. |
| D15 | Response shape | `decision` only, no `allowed` boolean | An `allowed` boolean too: two fields that can disagree. |
| D16 | Test layering | Unit, integration, e2e; each behavior tested once | The same assertions at every layer: slow and noisy, and one bug fails six tests. |

## Running it

### Quickstart

With MaxMind's test database, no account needed:

```sh
GEOIPCHECK_DB_PATH=testdata/GeoLite2-Country-Test.mmdb go run ./cmd/geoipcheck
```

```sh
curl -s localhost:8080/v1/country-checks \
  -H 'Content-Type: application/json' -H 'X-Request-ID: demo-1' \
  -d '{"ip":"81.2.69.142","allowed_countries":["US","GB"]}'
```

The test database is old, so the service logs `geoip database is stale` at
error level on startup. That's the staleness alert working.

With a real database: copy `deploy/geoipupdate.env.example` to
`deploy/geoipupdate.env`, add your MaxMind account ID and license key, then
run `docker compose -f deploy/compose.yaml up --build`. For Kubernetes, see
[deploy/k8s/README.md](deploy/k8s/README.md).

### Configuration

All configuration is environment variables. Invalid values are all reported
at once and the process exits with code 2. The three listeners must use
different ports.

| Variable | Default | Meaning |
|---|---|---|
| `GEOIPCHECK_DB_PATH` | (required) | Path to the `.mmdb` file. It doesn't have to exist at startup. |
| `GEOIPCHECK_DB_POLL_INTERVAL` | `60s` | How often to check the file for changes. |
| `GEOIPCHECK_DB_STALE_WARN` | `168h` | Database age that logs a warning. `0` disables it. |
| `GEOIPCHECK_DB_STALE_ERROR` | `336h` | Database age that logs an error. `0` disables it. Must exceed the warn age. |
| `GEOIPCHECK_HTTP_ADDR` | `:8080` | API listener. |
| `GEOIPCHECK_GRPC_ADDR` | `:9090` | gRPC listener. |
| `GEOIPCHECK_ADMIN_ADDR` | `:8081` | Health and metrics listener. |
| `GEOIPCHECK_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `GEOIPCHECK_LOG_FORMAT` | `json` | `json` or `text`. |
| `GEOIPCHECK_DRAIN_DELAY` | `5s` | On SIGTERM, how long to report not ready while still serving, so the load balancer can drop the endpoint. |
| `GEOIPCHECK_SHUTDOWN_TIMEOUT` | `15s` | Graceful stop budget after the drain. In-flight requests past it are cut. |

The orchestrator's kill deadline must exceed `DRAIN_DELAY + SHUTDOWN_TIMEOUT`
plus a few seconds. A second SIGTERM or Ctrl-C exits immediately.

Exit codes: `0` clean shutdown, `1` runtime failure (for example a port in
use), `2` invalid configuration.

### Development

```sh
make test          # unit, integration, e2e; all with -race (needs cgo)
make test RACE=    # same, without a C compiler
make lint          # go vet, golangci-lint, buf lint
make proto         # regenerate api/proto after editing the .proto
make tools         # install the pinned tool versions
```

Each behavior is tested once, at the layer that owns it (D16):

- **unit**: `go test ./...`.
- **integration**: `-tags=integration ./internal/...`. Real database files,
  the HTTP stack over `httptest`, and gRPC over `bufconn`.
- **e2e**: `-tags=e2e ./test/e2e`. The whole service through `app.Run`, with
  env config and real ports.

Real GeoLite2 databases are never committed (EULA). `testdata/` holds
MaxMind's MIT/Apache-licensed test databases.

### Benchmarks

AMD EPYC 74F3, 8 threads, Windows, Go 1.27.2 (`make bench`):

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `countrycheck.Check` (validate + decide, fake lookup) | 689 | 280 | 8 |
| `geoip.Reader.Country` (real lookup only) | 237 | 16 | 1 |
| non-routable range check | 124 | 0 | 0 |

A full check with a real lookup is the first two rows added, about 930 ns,
or roughly 1M checks per second per core. Network and JSON dominate the
real latency.

### Other docs

- [SUBMISSION_NOTES.md](SUBMISSION_NOTES.md): overview for reviewers.
- [docs/design.md](docs/design.md): decision records D1–D16.
- [docs/operations.md](docs/operations.md): data updates, metrics, alerts,
  runbook, shutdown.
- [docs/extensions.md](docs/extensions.md): what I'd build next.
- [AI_USAGE.md](AI_USAGE.md): every AI prompt and the verdict on it.
