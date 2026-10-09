# geoipcheck: Design

## Context

Customers want to restrict UI logins to selected countries. The customer DB
holds each customer's country allowlist. The API gateway captures the
requesting IP, loads the customer's allowlist, and calls this service with
`{ip, allowed_countries}`. This service says whether the IP is in an allowed
country.

Phase 1 ignores VPNs and uses only the presented IP.

Data source: MaxMind GeoLite2-Country.

## Architecture summary

```
cmd/geoipcheck         thin main
internal/app           lifecycle (config → logger → metrics → DB → servers)
internal/config        env → validated Config, all errors aggregated
internal/countrycheck  domain: Service.Check(ctx, Request) (Result, error)
internal/geoip         Reader + Watcher over the .mmdb file
internal/httpapi       HTTP adapter on :8080
internal/admin         /healthz, /readyz, /metrics on :8081
internal/grpcapi       gRPC adapter on :9090
internal/apimetrics    request metrics shared by both adapters
internal/requestid     X-Request-ID validation and generation
internal/logredact     IP → /24 or /48 prefix
internal/testutil      test-only helpers (logtest, metricstest)
api/proto              .proto + generated code
deploy                 Dockerfile, compose, k8s/
test/e2e               system tests through app.Run
```

Everything exported from `internal/countrycheck` is transport-neutral. Both
adapters depend on consumer-side interfaces defined in the adapter packages,
not on `*countrycheck.Service` concretely.

## Decision records

Each decision below gives the chosen option, the alternatives I rejected, and
why. **ACCEPTED** entries end with a **Revisit if** line: the signal that
would cause me to open the decision again.

---

### D1 — Data source: local GeoLite2-Country `.mmdb`

**Chosen:** download the GeoLite2-Country database to a volume and read it
from disk.

**Alternatives rejected:**

- *MaxMind web service.* 1,000 free lookups per day on the free tier, and
  every request pays an extra network round trip. The gateway is going to
  call this on every login. The quota is the wrong shape for this traffic.
- *CSV import into Postgres.* Stateful database for 9 MB of static data. The
  mmdb format is already a packed trie built for prefix lookups. Postgres
  adds operations without buying anything.
- *Commit or embed the DB in the image.* MaxMind EULA forbids redistribution.
  Not an option.

**Why:** the mmdb format is designed for this exact lookup. Reads are fast
and in-memory. Updates are a file swap.

*By: user.*

---

### D2 — Unmappable IPs: tri-state `decision` plus `reason`

**Chosen:** `{allowed, denied, unknown}` with a machine-readable `reason`.

**Alternatives rejected:**

- *Fail closed.* The service would be hard-coding a policy choice. Private
  IPs and un-geolocated public IPs would silently become `denied`. If the
  gateway wants to allow intranet traffic, the service has already denied it.
- *Fail open.* Opens a loophole: any IP that doesn't resolve is allowed.

**Why:** the gateway owns the policy. The service reports what it knows.
Possible reasons:

| decision | reason |
|---|---|
| allowed | `country_allowed` |
| denied  | `country_not_allowed` |
| unknown | `non_routable_address` |
| unknown | `country_unresolved` |

The gateway maps `unknown` to allow or deny per customer.

*By: user.*

---

### D3 — HTTP shape: `POST /v1/country-checks` with JSON

**Chosen:** POST a JSON body.

**Alternatives rejected:**

- *GET with query params.* The IP shows up in HTTP access logs. The input is
  loosely validated. The URL schema is awkward to extend.
- *Both GET and POST.* Two contracts for a single caller.

**Why:** POST JSON for scalability going forward. GET locks us in to a more
concise and concrete impl not set up for individual customer needs.

*By: user.*

---

### D4 — Service boundary: `{ip, allowed_countries}` in, decision out

**Chosen:** the service takes both the IP and the allowlist and returns a
decision.

**Alternatives rejected:**

- *Pure geolocation (`ip → country`).* The gateway would then join the
  allowlist itself. That contradicts the design we already agreed on.
- *The service reads the customer DB directly.* The service now has to know
  about customers, auth, and schema changes in the customer DB. Hard
  coupling for no benefit.

**Why:** the gateway already has the customer context. Passing the allowlist
in each call keeps this service stateless and single-purpose.

*Revisit if:* a second caller needs only geolocation. At that point split the
decision step from the lookup step and expose both.

*By: AI, user accepted.*

---

### D5 — Transports: shared core, thin HTTP and gRPC adapters, separate ports

**Chosen:** one core package, two transport adapters, one listener per
transport.

**Alternatives rejected:**

- *grpc-gateway.* The JSON contract becomes protobuf-shaped. The HTTP body is
  the camelCase JSON of a proto message, not an idiomatic JSON schema. I
  don't want to constrain the HTTP API to the proto spelling.
- *cmux on a single port.* Fragile. If a client gets the ALPN or first bytes
  wrong, it hits the wrong handler.

**Why:** the adapters are small. HTTP users get real JSON. gRPC users get
real protobuf. The core is tested once.

*By: user.*

---

### D6 — Code organization: package-by-responsibility under `internal/`

**Chosen:** one package per responsibility, named for what it does
(`countrycheck`, `geoip`, `httpapi`, `grpcapi`, `admin`, `logredact`, `config`,
`app`). `internal/app` wires them; `cmd/geoipcheck/main.go` only calls
`app.Run`.

**Alternatives rejected:**

- *Single `main` package.* Fine for a prototype. Not fine for a service with
  two transports, hot reload, and operations.
- *`handlers/services/models` split.* Horizontal layers across the codebase.
  A single change touches three packages for no reason.
- *Formal ports-and-adapters naming (`port/adapter/domain/...`).* Reads like
  a template. The names don't say what the package does.

**Why:** the names tell you where the logic is. One package per concept.

*Revisit if:* packages stay single-file and trivial for long. At that point
consolidate rather than keep the shells.

*By: AI, user accepted.*

---

### D7 — Go version and router: current stable, stdlib `net/http`, `log/slog`

**Chosen:** latest stable Go (1.27). Stdlib `net/http` with the method-aware
pattern (`"POST /v1/country-checks"`). `log/slog` for structured logs.

**Alternatives rejected:**

- *Go 1.21 + chi.* chi existed because stdlib couldn't do method routing.
  Stdlib can now. The dep buys nothing.
- *Manual method checks.* Easy to drift between routes.

**Why:** fewer deps, stdlib primitives.

*By: user.*

---

### D8 — DB change detection: poll `mtime` and size every 60s

**Chosen:** a goroutine that stats the file every 60 seconds and reloads
when either `mtime` or size changes.

**Alternatives rejected:**

- *fsnotify.* The file lives on an RWX volume, usually NFS or EFS, written
  from another node. inotify doesn't see writes made by another NFS
  client. Polling is the thing that always works.
- *SIGHUP only.* The CronJob that writes the DB would need to also signal
  every pod. More moving parts.
- *Restart on each update.* Rolling restarts for a data file every day. No.

**Why:** stat is cheap. One syscall per minute per pod is nothing.

*Revisit if:* reload latency matters to someone. Then combine fsnotify as a
fast path with polling as a floor.

*By: AI, user accepted.*

---

### D9 — DB in memory: `maxminddb.OpenBytes` plus `atomic.Pointer` swap

**Chosen:** read the file into a `[]byte`, build the reader with
`OpenBytes`, publish via `atomic.Pointer[Database]`. Lookups take the current
pointer with one atomic load.

**Alternatives rejected:**

- *mmap with ref counting.* After the file is replaced on disk and remapped,
  any lookup still holding the old mapping is at risk of use-after-unmap.
  Ref counting works, but it adds code and an atomic increment and
  decrement per lookup, where the pointer swap needs one atomic load.
- *RWMutex around the reader.* Every lookup takes a read lock. Fine, but the
  pointer swap costs nothing.

**Why:** the DB is 9 MB. Keeping two copies for a few seconds during reload
costs nothing. The lookup path has no lock.

*Revisit if:* heap grows above about 25 MB per pod, or we switch to the
GeoLite2-City DB (about 70 MB). At that point the cost of two copies
starts to matter.

*By: AI, user accepted.*

---

### D10 — Readiness: ready = valid DB loaded. Staleness = metric + logs.

**Chosen:** `/readyz` returns 200 as soon as a valid DB is in memory. If the
DB is stale, we log and emit a metric but we stay ready.

**Alternatives rejected:**

- *Readiness requires a fresh DB.* If MaxMind has an outage, every pod goes
  not-ready at the same time. The whole service drops.

**Why:** staleness is a monitoring problem, not a liveness problem. A
two-week-old country DB still answers 99%+ of lookups correctly.

*Revisit if:* staleness alerts go unwatched. Then readiness is the louder
signal.

*By: AI, user accepted.*

---

### D11 — k8s DB distribution: one CronJob writes an RWX PVC; pods mount it RO

**Chosen:** a daily CronJob runs `geoipupdate` and writes the DB to a
ReadWriteMany PVC. Every pod mounts the same PVC read-only. Pods pick up the
new file via the polling watcher (D8).

**Alternatives rejected:**

- *Sidecar in every pod.* Documented as the fallback for clusters without
  RWX. Downsides: license-key quota multiplies by replica count; the sidecar
  consumes resources on every pod.
- *Bake the DB into the image.* Needs a rebuild and a rollout per update.
  Also, EULA.
- *Init container, no hot reload.* Pods are stuck with an old DB until the
  next rollout.

**Why:** one downloader, one copy, pods just read.

*By: user.*

---

### D12 — Country-code validation: assigned ISO 3166-1 alpha-2 list plus `XK`

**Chosen:** validate against the full assigned ISO 3166-1 alpha-2 set, plus
`XK` (Kosovo, the exceptional MaxMind code). Reject unknown codes. For
near-misses, hint: `UK → GB`, `GBR → GB`, `UKR → UA`. `EN` gets no hint: it is a
language code, so any mapping is a guess.

**Alternatives rejected:**

- *Format only (two uppercase ASCII letters).* The AI's first proposal. It
  found the flaw itself: `UK` is a very common input, it is not an ISO code, and
  MaxMind returns `GB`. Format validation accepts `UK` and then silently
  denies every UK user.
- *Format plus warnings.* Warnings get ignored. The call still fails
  silently in production.

**Why:** the validator is a seat belt. Catch the bad input at the edge and
tell the caller.

*By: user.*

---

### D13 — GeoLite field: `country` only; absent → `country_unresolved`

**Chosen:** read `country.iso_code`. If `country` is absent in the record,
return `unknown` with reason `country_unresolved`.

**Alternatives rejected:**

- *Fall back to `registered_country`.* `registered_country` is where the
  address block is registered, not where the end user is. A foreign VPS
  rented through a US-registered company would be reported as US. That is
  exactly the kind of mislocation the service exists to catch.
- *Return both and let the caller pick.* Hedge. If a caller needs it later
  we can add it.

**Why:** one field. Clear semantics.

*Revisit if:* the `country_unresolved` share in real traffic is high. Then
look at what's missing and decide between a reason-level signal
(`country_unresolved_registered_only`) and a split field.

*By: AI, user accepted.*

---

### D14 — Correlation: `X-Request-ID` on HTTP, `x-request-id` on gRPC

**Chosen:** read the header if present, otherwise generate one. Return it on
responses. Log it on every line.

**Alternatives rejected:**

- *Also log `customer_id`.* Customer data in service logs for no gain. The
  gateway already joins its own correlation ID with the customer ID.

**Why:** one ID, one scope. Enough to trace a call across services.

*By: user.*

---

### D15 — No `allowed` boolean; `decision` is the single source of truth

**Chosen:** the response has `decision` and `reason`. There is no boolean
`allowed`. The policy for `unknown` is per-customer and lives in the gateway.

**Alternatives rejected:**

- *Add a boolean `allowed` to match the colloquial "indicator" wording.*
  Then we have two fields that can disagree, and documentation that says
  "when they disagree, trust `decision`". That is a trap.

**Why:** one field, one meaning. The `Decision` type's doc comment calls
this out so that it survives a refactor.

*By: user.*

---

### D16 — Test layering: unit → component integration → system E2E

**Chosen:** three layers. Each behavior is asserted at exactly one layer: the
one that owns it.

| Layer | How it runs | Owns |
|---|---|---|
| Unit | `go test ./...` | every validation rule, decision logic, resolver stub, HTTP decode/encode, access-log fields, gRPC status mapping |
| Component integration | `go test -tags=integration ./internal/...` | real `.mmdb` file reload under `-race`, HTTP stack via `httptest.Server`, gRPC via `bufconn`, metrics wiring |
| System E2E | `go test -tags=e2e ./test/e2e` | `app.Run` with env config, both transports, DB appears/reloads, graceful shutdown |

**Alternatives rejected:**

- *Assert everything at every layer.* Hundreds of tests retelling the same
  story. Slow runs. Noisy failures (one bug fails six tests).

**Why:** when a test fails you know what layer failed and the reason reads
as a sentence.

*By: user.*

---

## See also

- [README.md](../README.md#api-contracts): the API contracts.
- [operations.md](operations.md): data updates, metrics, alerts, and the
  runbook.
