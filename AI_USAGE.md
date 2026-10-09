# AI_USAGE

Running log of prompts to Claude, what I accepted, what I rejected, and why.

Format:

> **Prompt summary.**
> **Verdict:** accepted / rejected / partial. Why.

---

## Planning session

**Prompt: proposed data source.**
Options: MaxMind web service, GeoLite2 CSV in Postgres, GeoLite2 `.mmdb`
local file, bake DB into image.
**Verdict:** accepted `.mmdb` local. The web service free tier is 1,000/day.
Postgres is a stateful database for 9 MB of static data. The DB EULA
forbids redistributing the file in the image.

**Prompt: unmappable IPs.**
Options: fail closed, fail open, tri-state `decision` + `reason`.
**Verdict:** accepted tri-state. Fail-closed hard-codes a policy the
customer wants to control. Fail-open opens a loophole.

**Prompt: HTTP shape.**
Options: GET `/v1/countries/{code}/check/{ip}`, POST `/v1/country-checks`,
both.
**Verdict:** accepted POST JSON. My reason: "POST JSON for scalability
going forward. GET locks us in to a more concise and concrete impl not set
up for individual customer needs." PII in URLs is a bonus argument.

**Prompt: service boundary.**
Options: pure geolocation (`ip → country`), decide here (`ip + allowlist →
decision`), service reads customer DB.
**Verdict:** accepted the AI's recommendation. The gateway has the customer
context already. Keeps this service stateless.

**Prompt: transports.**
Options: HTTP only, HTTP + gRPC separate ports, grpc-gateway, cmux.
**Verdict:** accepted separate ports. grpc-gateway forces proto-shaped JSON.
cmux is fragile.

**Prompt: package layout.**
Options: single `main` package; `handlers/services/models`; package by
responsibility; formal ports/adapters naming.
**Verdict:** accepted "package by responsibility." The others either hide
intent (horizontal split) or read as a template (formal naming).

**Prompt: Go version and router.**
**Verdict:** accepted current stable Go and stdlib ServeMux. chi was a
workaround for a thing stdlib now does.

**Prompt: DB change detection.**
Options: fsnotify, SIGHUP, poll mtime+size, restart on each update.
**Verdict:** accepted polling. fsnotify is unreliable with Kubernetes
symlink swaps. SIGHUP needs a signal pathway from the writer to every pod.

**Prompt: DB in memory.**
Options: mmap + refcount, RWMutex, `FromBytes` + `atomic.Pointer` swap.
**Verdict:** accepted the atomic pointer. 9 MB file; two copies for a few
seconds costs nothing; lookup path has no lock. Revisit at the City DB.

**Prompt: readiness.**
Options: ready requires fresh DB, ready = DB loaded (staleness → metric).
**Verdict:** accepted the DB-loaded version. "Fresh" readiness turns a
MaxMind outage into a fleet-wide outage.

**Prompt: Kubernetes DB distribution.**
Options: sidecar per pod, CronJob + RWX PVC, bake into image, init
container with restarts.
**Verdict:** accepted CronJob + RWX PVC. Sidecar is the documented fallback
if a cluster has no RWX class.

**Prompt: country code validation.**
Options: format only, format + warnings, ISO list + "did you mean".
**Verdict:** accepted ISO list. The AI first proposed format-only, then
found the flaw itself: `UK` is a common input, not an ISO code, and would
be accepted and then silently never match any user because MaxMind returns
`GB`.

**Prompt: GeoLite field.**
Options: `country` only, fall back to `registered_country`, return both.
**Verdict:** accepted `country` only. `registered_country` points to where
the address block is registered, not the user.

**Prompt: correlation.**
Options: `X-Request-ID` only, plus `customer_id`.
**Verdict:** accepted request ID only. Customer data in service logs with
no payoff.

**Prompt: response shape — add an `allowed` boolean?**
**Verdict:** rejected. Two fields that can disagree. The `decision` type
is the single source of truth.

**Prompt: test strategy.**
Options: duplicate assertions at every layer; each behavior owned by one
layer.
**Verdict:** accepted one-owning-layer. Fewer tests. Clearer failure
messages. The failure tells you where to look.

---

## Implementation

(Appended per phase.)

### Phase 0 — bootstrap

**Prompt: write `docs/design.md` in the user's voice.**
**Verdict:** accepted. Short declarative sentences. Facts and opinion kept
separate.

**Prompt: write `.golangci.yml` and `Makefile`.**
**Verdict:** accepted. Lint config is strict but focused on real problems.
Makefile has one target per layer in D16 plus `bench`, `fuzz`, `lint`,
`proto`, `docker`.

**AI action: committed Phase 0 without being asked.**
**Verdict:** rejected. I said "Do not commit, this is a local build, I'll
commit later." The AI undid the commit (files kept staged) and stopped
committing. The plan's "commit after each phase" step is dropped; I commit.

**Note:** `-race` needs cgo and a C compiler. This Windows machine has
neither, so local runs use `make test RACE=`. CI runs with `-race`.

### Phase 1 — countrycheck

**Prompt: implement the domain package test-first.**
**Verdict:** accepted. Check takes ~555 ns with 3 allocs. The fuzz test
ran 650k execs clean.

**AI self-catch: `2001:db8::/32` documentation prefix.**
The first version claimed it sat inside `2001::/23`. It doesn't (the /23
ends at `2001:1ff`). The unit test failed. Fixed by listing it explicitly.
**Verdict:** accepted. This is why the table tests exist.

**Code-review subagent findings (all fixed):**
- **Teredo carve-out was wrong.** MaxMind maps `2001::/32` onto the IPv4
  tree using the embedded Teredo *server* address. A lookup would return
  the relay's country. Now non-routable. **Verdict:** accepted. Real bug.
  This one I would not have caught.
- **Missing prefixes:** `5f00::/16` (SRv6), `100:0:0:1::/64` (dummy),
  `::/96` (IPv4-compatible; MaxMind would look `::a00:1` up as
  `10.0.0.1`). Missing carve-outs `2001:1::3`, `192.0.0.9`, `192.0.0.10`.
  **Verdict:** accepted.
- **`strings.ToUpper` is Unicode-aware.** `"ſe"` folded to `"SE"` and
  passed validation. Replaced with an ASCII-only upper. **Verdict:**
  accepted.
- **Error wrapping matched two categories.** A resolver returning its own
  `context.Canceled` matched both `ErrDatabaseUnavailable` and
  `context.Canceled`. Now only one sentinel is wrapped; the cause is kept
  as text. **Verdict:** accepted.
- **Duplicated assertions across tests.** The service tests re-checked
  five non-routable IPs that `specialaddr_test.go` owns. Cut to one IP
  proving the short-circuit. **Verdict:** accepted. Matches D16.
- **`EN`/`ENG` → `GB` hints.** `EN` is a language code, so the hint is a
  guess. Removed. **Verdict:** accepted.
- **NAT64 `64:ff9b::/96` is globally reachable per IANA.** Kept it
  non-routable, but the deviation is now documented in the file header.
  **Verdict:** accepted.

### Phase 2 — geoip reader and watcher

**Prompt: implement the reader, hot reload, and freshness metrics test-first.**
**Verdict:** accepted. A lookup takes ~262 ns with 1 alloc. Swaps go
through `atomic.Pointer`, so lookups never take a lock.

**AI self-catch: the plan named the wrong API.** The plan said
`maxminddb.FromBytes`. maxminddb-golang v2.7.0 calls it `OpenBytes`. Code
uses `OpenBytes`; D9's wording gets fixed in Phase 7.
**Verdict:** accepted.

**AI self-catch: `go mod tidy` dropped the grpc deps.** Nothing imports
grpc until Phase 4, so tidy removed it. They come back in Phase 4.
**Verdict:** accepted. No point pinning deps we don't use yet.

**AI self-catch: data race in the Run test.** The first version called
`touch()` on the fake FS while `Run` was polling. Rewritten so `Stat`
signals a channel and the test drives ticks one at a time.
**Verdict:** accepted.

**Code-review subagent findings:**
- **Windows rename flake.** `os.Rename` over a file that was just read
  failed with "Access is denied" about 1 in 300 runs. `replaceFile` now
  retries for up to 2s. **Verdict:** accepted. Test-only; production
  never renames.
- **Goroutine leak on `t.Fatal`.** The race test only cancelled the
  readers on the happy path. A cleanup now does `cancel(); wg.Wait()`.
  **Verdict:** accepted.
- **Swaps could finish before the readers started.** The race test now
  waits until every reader has done one lookup. Without that it could
  pass without testing anything. **Verdict:** accepted.
- **The "polled after cancel" check never tested anything.** No tick was
  ever sent after cancel, so it couldn't fail. Removed; `<-done` is the
  real assertion. **Verdict:** accepted.
- **logtest broke the slog.Handler rules.** `WithGroup("")` added a
  group, empty-key groups weren't inlined, and empty attrs weren't
  dropped. Fixed, with a test, since every later phase asserts logs
  through it. **Verdict:** accepted.
- **`StaleError < StaleWarn` hides the warn level.** Documented on the
  field. Config validation enforces it in Phase 5. **Verdict:** accepted.
- **`lastAttempt` is set before `Load`.** That's on purpose (D8): a corrupt
  file isn't re-read every poll. Added a comment. **Verdict:** accepted.
- **"Verify costs a few milliseconds" was a guess.** Reworded to say what
  matters: it runs off the lookup path. **Verdict:** accepted.

### Phase 3 — logredact, httpapi, admin

**Prompt:** "Phase 3: HTTP adapter (POST /v1/country-checks, problem+json,
one access-log line per request), admin listener (/healthz, /readyz,
/metrics), IP redaction for logs. Unit tests own decode and error mapping;
one integration test owns the real stack."

**Three small packages the plan didn't name.** `logredact` (an IP becomes
its /24 or /48 prefix), `requestid` (validate or generate), and `apimetrics`
(the checks and latency series). HTTP and gRPC both need all three. Without
them, gRPC would either copy the code or import `httpapi`.
**Verdict:** accepted. Each one is a single file with its own tests.

**AI self-catch: the design.md request example contradicted itself.** The
request sent `["US","gb"]` for a GB address, but the response showed
`denied`. Lowercase is normalized, so that request is allowed. Changed the
request to `["US","ca"]`, which keeps the lowercase example and makes
`denied` correct.
**Verdict:** accepted.

**AI self-catch: two doc fixes from earlier phases.** D9 now says
`OpenBytes` and `atomic.Pointer[Database]`. D12 now says why `EN` gets no
hint (it's a language code, so there's no single country to suggest).
**Verdict:** accepted.

**AI self-catch: JSON type errors named the field `allowed_countries.0`.**
encoding/json reports dotted paths. The domain's validation errors use
`allowed_countries[0]`. `fieldPath` converts the JSON form so a client sees
one format.
**Verdict:** accepted.

**Code-review subagent findings:**
- **The raw IP leaked into logs through the geoip error.** `Country` wrapped
  its error with the address, and the access log logs the cause. Removed
  the IP from the error text. **Verdict:** accepted. The reviewer suggested
  a test with a fake error containing the IP. Rejected as written: the
  access log logs the cause verbatim, so that test can only fail. The
  guard belongs on the geoip error text, and that's where the fix went.
- **`allowlist_size=0` was logged when the body didn't decode.** That reads
  as a real empty allowlist. Request fields are now logged only after a
  successful decode. **Verdict:** accepted.
- **A panic after the headers were sent wrote a second response.** It now
  aborts with `http.ErrAbortHandler`, so the client can't take a truncated
  body for a complete one. **Verdict:** accepted.
- **Aborted requests weren't logged or counted.** The access log now
  recovers, logs with `aborted=true` and code `aborted`, then re-panics.
  **Verdict:** accepted.
- **apimetrics had its own copy of the outcome list.** Replaced with
  `countrycheck.Outcomes()`. The fuzz test uses the same list.
  **Verdict:** accepted.
- **The integration tests repeated the unit-test assertions.** Trimmed to
  what only the real stack shows (D16). **Verdict:** accepted.
- **Metrics assertions only checked that a series existed.** They now check
  the sample count and sum. **Verdict:** accepted.
- **The 499 problem had an empty title.** `StatusText(499)` is "". It is set
  to "Client Closed Request" now. **Verdict:** accepted.
- **ServeMux's 301 for unclean paths was undocumented.** Added a comment.
  The gateway sends canonical paths. **Verdict:** accepted.
- **Transport was a bare string.** It's `apimetrics.Transport` now.
  **Verdict:** accepted.
- **`StartDraining` doesn't log.** Deferred to Phase 5. The app owns the
  shutdown sequence, so it logs there. **Verdict:** deferred.

### Phase 4 — proto, buf, grpcapi

**Prompt:** "Phase 4: geoipcheck.v1 proto with buf (STANDARD lint, FILE
breaking), generated code committed, gRPC adapter with the same codes as
HTTP in ErrorInfo, grpc.health.v1 backed by the same readiness as /readyz.
Unit tests own status mapping; one integration test over bufconn owns the
wire."

**buf installed with `go install`.** No Docker here and no package manager
entry. `go install github.com/bufbuild/buf/cmd/buf` plus protoc-gen-go and
protoc-gen-go-grpc put everything in `~/go/bin`. CI does the same, so the
local and CI generators can't drift.
**Verdict:** accepted.

**Shared constants moved down to the domain.** `CodeDatabaseUnavailable`
and the retry delay lived in `httpapi`. gRPC needs both, and importing
`httpapi` from `grpcapi` is backwards. They're in `countrycheck` now, next
to the other codes. The operation labels moved to `apimetrics` for the
same reason.
**Verdict:** accepted.

**ErrorInfo.reason is lowercase (`invalid_country_code`).** AIP-193 asks
for UPPER_SNAKE. I care more about a client switching on one code across
HTTP and gRPC than about matching the AIP. Documented in status.go and in
the proto.
**Verdict:** accepted. Revisit if we publish to a Google-style API
registry that lints reasons.

**Custom health server instead of grpc-go's `health.Server`.** The stock
server is push-based: every readiness change has to be mirrored into it.
Ours reads the same `Readiness` as /readyz on every call, so the two can't
disagree. Watch stays Unimplemented.
**Verdict:** accepted.

**`metricstest` helper.** httpapi and grpcapi tests each had their own
gather-and-match loop. `metricstest.Find` and `Count` replace both.
**Verdict:** accepted.

**AI self-catch: the proto hard-codes "1 to 250 entries".** The number is
owned by `countrycheck.MaxAllowedCountries`, and a proto comment can't
reference it. I checked it against the constant before leaving it in. If
the limit changes, the proto comment has to change with it.
**Verdict:** accepted.

**Code-review subagent findings, round 1:**
- **Calls rejected before the interceptor weren't logged or counted.**
  grpc-go fails oversized and undecodable messages, and unknown methods,
  before any interceptor runs. HTTP logs every request, so gRPC should too.
  The access log moved to a `stats.Handler` (TagRPC → InHeader → End).
  **Verdict:** accepted.
- **`method` and `code` attrs collided with the HTTP log.** HTTP uses
  `method` for the verb, and both land in one index. Renamed to
  `grpc_method` and `grpc_code`. **Verdict:** accepted.
- **Unknown methods would create one metric series each.** They share the
  `unmatched` operation label now, same as HTTP. **Verdict:** accepted.
- **No stream interceptor.** Every RPC is unary. Added a comment saying a
  streaming RPC needs one. **Verdict:** accepted as a comment; I don't want
  to build it without a consumer.
- **No MaxConnectionAge.** The gateway holds HTTP/2 connections open, so
  replicas added by the HPA would get no gRPC traffic. Added a 5m max age
  with 30s grace, and a keepalive enforcement policy so frequent client
  pings don't get the connection killed. **Verdict:** accepted.
- **`statusFor` didn't say what it returned.** Renamed to `callErrorFor`.
  **Verdict:** accepted.
- **ErrorDomain and the Reason↔Decision pairs weren't in the proto.** The
  proto is what clients read. Both are documented there now.
  **Verdict:** accepted.
- **Watch unimplemented breaks grpc-go client-side health checking.** It
  doesn't break it: grpc-go treats Unimplemented as healthy. Readiness
  only changes at startup, before the pod is in the Service, and at
  shutdown, where GracefulStop sends GOAWAY anyway. **Verdict:** rejected,
  reason written into health.go.
- **The readiness error is dropped by health Check.** Phase 5 logs
  readiness transitions once, which is where that belongs. Logging it per
  probe would be noise. **Verdict:** rejected.
- **The health "not logged" test only checked one label set.** Now asserts
  the latency family has no series at all (`metricstest.Count`).
  **Verdict:** accepted.
- **Nil logger panics.** httpapi has the same contract: all Config fields
  are required. **Verdict:** rejected for consistency.

**AI self-catch: unknown methods still weren't logged after the stats
handler change.** The new integration test failed. Reading grpc-go's
`handleStream`: it fires TagRPC and InHeader for unknown methods, then
writes Unimplemented without an End event. Registering a
`grpc.UnknownServiceHandler` routes them through the normal path, where
End fires. The handler also echoes the request ID, since no interceptor
runs for it.
**Verdict:** accepted.

**AI self-catch: the integration metric assertion was racy.** End can fire
after the client already has its status. The integration tests now wait
for the access log line, and the metric is recorded before the log, so a
read after the line is safe.
**Verdict:** accepted.

**Code-review subagent findings, round 2:**
- **Integration tests repeated unit-test metric assertions** (OK,
  ResourceExhausted, unmatched) and the success-path request ID echo.
  Removed; each layer keeps only what it alone proves (D16).
  **Verdict:** accepted.
- **The unit harness ran the interceptor for calls grpc-go rejects before
  it.** That's a path production never takes. Added `reject`, which drives
  only the stats handler. **Verdict:** accepted.
- **Metric name repeated as a string.** A typo would make the "no series"
  assertion pass vacuously. One constant now. **Verdict:** accepted.
- **Proto overstated the error contract.** BadRequest is only attached
  when one field is at fault, and pre-decode rejections get no request ID
  echo. Both corrected. **Verdict:** accepted.
- **Health follows readiness is asserted at both layers.** The unit test
  covers Check/List/NotFound against a fake; the integration test covers
  the real `admin.Readiness` over the wire. Different claims.
  **Verdict:** rejected.
- **Health-prefix skip also hides `/grpc.health.v1.Health/Bogus`.**
  Negligible. **Verdict:** rejected.

### Phase 5 — config, app, cmd, e2e

**Prompt:** "Phase 5: env config that reports every problem at once,
app.Run owning the lifecycle (three listeners, watcher, drain-then-stop
shutdown), a thin main, and e2e tests through app.Run for what only the
assembled binary shows."

**Added `GEOIPCHECK_DRAIN_DELAY`.** The plan had a fixed drain. How long the
load balancer takes to drop an endpoint depends on the environment, and
e2e needs it at zero to run fast. One variable, defaulting to 5s.
**Verdict:** accepted.

**Exit code 2 for config errors, 1 for runtime failures.** A crash-looping
pod with exit 2 says "fix the deploy" without reading logs.
**Verdict:** accepted.

**The "listening" log carries the bound address.** With port 0 it is the
only place the real port shows up. e2e reads ports from it, so there's no
test-only hook in `app`.
**Verdict:** accepted.

**`golang.org/x/sync/errgroup` for the run group.** The first server
failure cancels the rest and is the error Run returns. Hand-rolling that
is more code for the same behavior.
**Verdict:** accepted.

**AI self-catch: the test databases disagree on one IP.** Hot reload needs
an answer that changes when the file changes. `214.78.0.1` is US in
GeoLite2-Country-Test and has no country in GeoIP2-Country-Test, so the
swap turns allowed into unknown/country_unresolved. Found by dumping both
files, not by guessing.
**Verdict:** accepted.

**A second signal kills the process.** main restores default signal
handling after the first, so an operator can skip the drain with a second
Ctrl-C.
**Verdict:** accepted.

**In-flight drain is not faked in e2e.** `http.Server.Shutdown` and
`grpc.Server.GracefulStop` already guarantee in-flight requests finish.
e2e checks what we own: readiness flips while requests still succeed, then
a clean exit.
**Verdict:** accepted.

**Code-review subagent findings, round 1:**
- **[bug] `GracefulStop` before `Serve` made a clean stop exit 1.** With a
  zero drain, a cancel that lands before the gRPC goroutine reaches
  `Serve` makes it return `ErrServerStopped`. Treated as nil now.
  **Verdict:** accepted.
- **Admin stop got the API servers' already-spent context.** Admin now has
  its own 1s budget. **Verdict:** accepted.
- **Shutdown didn't say why it started.** The log now has `cause`: `signal`
  or the server error. A server failure still drains, because the other
  transport is still taking traffic. **Verdict:** accepted; skipping the
  drain on failure rejected.
- **Wait also waits for the watcher.** A reload stuck on a hung volume
  holds the exit until SIGKILL. Commented, not changed: I'd rather see the
  hang than leak the goroutine. **Verdict:** accepted as a comment.
- **Kill deadline rule undocumented.** `terminationGracePeriodSeconds` must
  exceed DRAIN_DELAY + SHUTDOWN_TIMEOUT plus margin. On the config field
  now; k8s and the operations doc carry it too. **Verdict:** accepted.
- **`:http` or `:99999` passed config and failed at bind (exit 1).** addr
  now requires a numeric port from 0 to 65535. Tests added for that, for
  StaleError equal to StaleWarn, for a shared port, and for trimming.
  **Verdict:** accepted.
- **Unknown `GEOIPCHECK_*` names are ignored silently.** A typo'd optional
  variable falls back to its default. Catching it means reading
  `os.Environ` in Load, and the gain is small for eleven variables.
  **Verdict:** rejected. Revisit if the variable list grows.
- **e2e repeated the integration test's "503 before the database".**
  Removed; e2e keeps only readyz staying up without a file.
  **Verdict:** accepted.
- **e2e config test counted problems instead of naming them.** It now
  checks that each variable is named. **Verdict:** accepted.
- **Drain test only checked HTTP, with a 1s window.** It now checks gRPC
  health NOT_SERVING and both transports serving, inside a 3s drain.
  **Verdict:** accepted.
- **Unchecked type assertion and a missing `t.Helper`.** Fixed.
  **Verdict:** accepted.
- **Nits:** split the long server literals, added a log line between the
  drain and the stop, corrected the build-info comment, and pass
  `context.Background()` to listen so a SIGTERM mid-bind isn't a startup
  failure. **Verdict:** accepted. Removing the unreachable non-`*config.Error`
  branch in Run: **rejected**. It costs three lines and keeps Run correct if
  Load ever returns a different error.

**Code-review subagent findings, round 2:**
- **The duplicate-port check compared strings.** `:80` and `:080` both
  passed and only failed at bind. Ports compare as numbers now; tests use
  `:07000` and `:00`. **Verdict:** accepted.
- **`shutdownCause` reports "signal" if a signal follows a failure before
  it runs.** The exit code is still right. Commented, not fixed.
  **Verdict:** accepted as a comment.
- **One more unchecked type assertion in the e2e helper.** Fixed.
  **Verdict:** accepted.
- **The "port closed after exit" check can see another process take the
  ephemeral port.** The window is microseconds on a loopback port.
  **Verdict:** rejected.

### Phase 6 — deploy and CI

**Prompt:** "Phase 6: multi-stage Dockerfile on distroless, compose with
geoipupdate, kustomize base (Deployment, Service, RWX PVC, CronJob, PDB,
HPA, NetworkPolicy), GitHub Actions running lint, all three test layers
with -race, govulncheck, docker build and kubeconform."

**Tool versions pinned in the Makefile; CI calls `make tools-*`.** The first
CI draft repeated versions inline. One place now.
**Verdict:** accepted.

**AI self-catch: tools built with the system Go couldn't load the module.**
`go install` outside the module used Go 1.26; golangci-lint and govulncheck
refused a 1.27 module. Rebuilt with `GOTOOLCHAIN=go1.27.2`. CI is unaffected
since setup-go reads go.mod.
**Verdict:** accepted.

**govulncheck found four reachable HTTP/2 server vulnerabilities** in
`golang.org/x/net` v0.59.0, pulled in by grpc. Bumped to v0.60.0; clean.
**Verdict:** accepted.

**First golangci-lint run: 23 issues.** Fixed the real ones: doc comments on
exported const blocks, a tautological `New() == New()` in a test.
Annotated the intentional ones with a reason: `%v` where wrapping a second
error would make `errors.Is` match two categories, and identity comparison
with `http.ErrAbortHandler`, which is how net/http itself checks it.
Excluded noctx and bodyclose in tests: helpers close bodies in `t.Cleanup`,
which bodyclose can't follow.
**Verdict:** accepted.

**No `replicas` in the Deployment.** The HPA owns the count. A `replicas`
field resets it on every apply.
**Verdict:** accepted.

**No CPU limit, memory limit 128Mi.** Throttling hurts a latency-bound
service more than it protects the node. Memory is bounded: the database is
about 10 MB, twice that during a reload.
**Verdict:** accepted. *Revisit if* the cluster policy requires CPU limits.

**geoipcheck gets deny-all egress.** It reads a local file and calls
nothing.
**Verdict:** accepted.

**Not applied to a live cluster.** No cluster or Docker here. Validation is
`kustomize build | kubeconform -strict` and a review.
**Verdict:** accepted, stated in deploy/k8s/README.md.

**Code-review subagent findings:**
- **[bug] fsGroup does nothing on most RWX drivers.** The seed job would
  fail to write if the export root is root-owned. README now says the
  volume root must be writable by 65532; added `fsGroupChangePolicy:
  OnRootMismatch` for drivers that do honor it. **Verdict:** accepted.
- **[bug] service.yaml points at docs/operations.md, which doesn't exist
  yet.** It's written in Phase 7. **Verdict:** rejected as already planned.
- **PDB blocks node drains while every pod is not ready** (before the first
  download). Added `unhealthyPodEvictionPolicy: AlwaysAllow`.
  **Verdict:** accepted.
- **geoipupdate under readOnlyRootFilesystem may write outside /data and
  /tmp.** Can't verify without running the image. Called out in the README
  as the first thing to check, rather than mounting guessed paths.
  **Verdict:** accepted as documentation.
- **Drift check used `git diff`, which misses new untracked files.** Now
  `git status --porcelain`. **Verdict:** accepted.
- **`buf breaking` against the https URL needs credentials on a private
  repo.** Now reads `origin/<base>` from a full-depth checkout.
  **Verdict:** accepted.
- **Separate gofmt step duplicates golangci-lint's formatters.** Verified
  with a scratch file that `golangci-lint run` reports gofmt, then removed
  the step. **Verdict:** accepted.
- **Fuzz crashers are lost on failure.** Uploaded as an artifact.
  **Verdict:** accepted.
- **A `.golangci.yml` rule hid every test-file issue containing "G".**
  Deleted; lint is still clean. **Verdict:** accepted.
- **Nits:** redundant `USER` in the Dockerfile (removed), relative links in
  the k8s README (fixed), list CNIs where probes bypass NetworkPolicy
  (rejected; not verified on any).

### Phase 7 — docs

**Prompt:** "Phase 7: README with the full HTTP and gRPC reference and the
gateway contract, operations (metrics, alerts, runbook, shutdown, gRPC load
balancing), data-updates, extensions, SUBMISSION_NOTES, and fix design.md
where the code moved on."

**Every fact in the docs checked against the code, not the plan.** Error
codes, log messages and fields, metric names and labels, and the request ID
rules were read from source before being written down.
**Verdict:** accepted.

**AI self-catch: the docs said `MaxConnectionAge` wasn't set.** The first
draft of operations.md and extensions.md listed it as future work. Phase 4
already added it (5m, 30s grace). Both docs now describe what's there, and
the extension is the headless Service for per-call balancing.
**Verdict:** accepted.

**AI self-catch: the README claimed `UKR` gets a hint for the UK.** `UKR` is
Ukraine's alpha-3 code and hints `UA`. The README now says `UK` and common
alpha-3 codes get a suggestion.
**Verdict:** accepted.

**design.md fixes.** The architecture list gained `apimetrics`,
`requestid`, `testutil`, and `test/e2e`. D16 says `./internal/...` and
`app.Run`, matching the Makefile. D9 said GeoLite2-City was "hundreds of
MB". It's about 70 MB.
**Verdict:** accepted.

**Alert thresholds are starting points.** The stale alert fires at 10 days,
between the 7-day log warning and the 14-day error. Nothing has run on real
traffic, so the doc says to tune them.
**Verdict:** accepted.

**Code-review subagent findings:**
- **[bug] The error alert couldn't fire on HTTP-only traffic.** It added
  two `sum()`s, and the latency series aren't pre-created. A sum over no
  series is empty, and `empty + x` is empty, so with no gRPC errors yet
  the whole alert was silent. Now one `sum()` over two selectors joined with
  `or`. **Verdict:** accepted. Real bug, and the kind that only shows up
  during the outage it was meant to catch.
- **[bug] The benchmark row called a fake lookup a real one.**
  `BenchmarkCheck` uses a fake resolver. The README now labels it that way,
  and a full real check is about 930 ns, not 689 ns. Throughput is about
  1M/s per core, not 1.4M. **Verdict:** accepted.
- **The README said every gRPC error carries ErrorInfo.** Only validation
  errors and `UNAVAILABLE` do. The others are bare status codes. Fixed in
  the README and SUBMISSION_NOTES. **Verdict:** accepted.
- **The runbook grouped gRPC errors by `error_code`, which gRPC doesn't set
  for Internal or DeadlineExceeded.** It now says to group by `grpc_code`.
  It also notes that grpc-go returns Internal for an undecodable message,
  which is a client bug. **Verdict:** accepted.
- **The reload alert resolves after an hour even if the bad file is still
  there**, because a bad file is tried once. Said so in the alert and the
  runbook. **Verdict:** accepted. I'd rather document it than add a gauge
  for it.
- **504 is effectively gRPC only**, since HTTP carries no deadline. The
  README row says so. **Verdict:** accepted.
- **Request ID echo caveats on gRPC**, 499 being HTTP only, duplicates
  counting toward the 250 cap, and distinct ports being required: all
  documented. **Verdict:** accepted.
- **Nits:** the normal DB age is 0–5 days, not 0–4; the missing log lines
  (`panic serving request`, `db_path`); "off by default" for reflection;
  the allowlist size; the line wraps; and the architecture block alignment.
  **Verdict:** accepted, all fixed.

### Docs cleanup

**Prompt:** "I want our resources cleaned up. read me should state problem,
initial requirements, optional requirements with justification, API
contracts, alternate designs." Follow-ups: SUBMISSION_NOTES is my overview
to Avoxi and walks through each ask in the brief; alternate designs go in
the README as a summary table, with design.md keeping the full records.

**README restructured around the brief.** Problem, a requirements table
mapping each ask to how it's met, each optional item with its
design-meeting justification, the API contracts, and a D1–D16 table of
what was chosen and what was rejected.
**Verdict:** accepted.

**One home per topic.** data-updates.md merged into operations.md as
"Keeping the database fresh" and deleted. design.md dropped its copy of the
API reference and points to the README. The README is the only API
reference.
**Verdict:** accepted. Two copies of a contract drift.

**SUBMISSION_NOTES rewritten as the overview to Avoxi.** It covers the core
ask, each grading area, each bonus item, what was and wasn't verified, and
a summary of this log.
**Verdict:** accepted.

**AI self-catch: D12 credited me with the format-only proposal.** This log
says the AI proposed it and found the flaw. design.md now says the same.
**Verdict:** accepted.

**AI self-catch: a "manual curl and grpcurl run" was listed as verified.**
Nothing recorded it. Removed. Every claim left under "verified" was rerun
or is in this log.
**Verdict:** accepted.

**Links checked by script.** Every relative link and anchor in every `.md`
file resolves. One broken anchor (`#grpc-api`) was fixed.
**Verdict:** accepted.

**Code-review subagent findings:**
- **[bug] 499 and 504 were documented as normal disconnect and timeout
  handling.** `Check` reads `ctx.Err()` only after the lookup fails, and the
  lookup doesn't take ctx. So a client that disconnects mid-check is logged
  with the real status, and 499/504 only happen with no database loaded.
  The runbook blamed `DeadlineExceeded` on CPU starvation. Docs corrected.
  The code is right: a 1 µs lookup doesn't need cancellation.
  **Verdict:** accepted.
- **[bug] design.md said main wires the packages.** `internal/app` does.
  **Verdict:** accepted.
- **[smell] The notes said CI runs `-race` and builds the image.** The
  workflow has never run; the repo has no remote. Now says it's set up to,
  and that the first push is the first run. **Verdict:** accepted.
- **[smell] D8 rejected fsnotify for symlink swaps.** That's ConfigMap
  behavior. The real reason for this volume is that NFS doesn't deliver
  inotify events for writes from another node. **Verdict:** accepted.
- **[smell] AI_USAGE doesn't show the 10/6 decision split.** design.md's
  "By" lines are the record; the notes now point there. **Verdict:**
  accepted.
- **Nits:** "no mutable global state", the test-style claim, ErrorInfo
  scope, protobuf in the dependency list, Teredo/NAT64 in the non-routable
  row, log field order, the service.yaml balancing comment, the mmap
  wording. **Verdict:** accepted, all fixed. 9 MB vs 10 MB across docs:
  left; both are "about".
