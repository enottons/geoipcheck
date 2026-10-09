# Submission notes

This is my solution to the country login restriction challenge. `geoipcheck`
is a Go service the API gateway calls on each login with the requesting IP
and the customer's allowed countries. It answers `allowed`, `denied`, or
`unknown`, with a reason, over HTTP and gRPC. It reads a local MaxMind
GeoLite2-Country database that hot-reloads.

The core ask is done, along with every bonus item. Unit, integration, and
end-to-end tests pass. Lint and govulncheck are clean.

The [README](README.md) has the problem, requirements, API contracts, and
alternate designs. This file walks through what you asked for and where to
find it.

## The core ask

**Restrict UI logins to selected countries, phase 1 is IP only.** The service
decides from the presented IP alone. VPN detection is the obvious phase 2.
It's sketched in [docs/extensions.md](docs/extensions.md).

**An HTTP API that receives an IP and a list of allowed countries.**
`POST /v1/country-checks` with `{"ip": "...", "allowed_countries": [...]}`.
The gateway owns the customer lookup and sends the list, as the team
designed. The service stays stateless and never touches the customer
database.

**Return an indicator of whether the IP is in an allowed country.** The
indicator is `decision`, and it has three values, not two. Some IPs have no
country: private addresses behind a misconfigured proxy, or ranges MaxMind
can't place. If the service maps those to "denied", it has made a policy
choice the customer should own. If it maps them to "allowed", it opens a
loophole. `unknown` lets the gateway apply the customer's policy. Every
response also carries a `reason` (`country_allowed`, `country_not_allowed`,
`non_routable_address`, `country_unresolved`) for logs and support.

**MaxMind GeoLite2.** GeoLite2-Country in `.mmdb` form, held in memory.
A check takes about a microsecond. I rejected the MaxMind web service: its
free tier is 1,000 lookups a day, and it adds a network hop to every login.

**Go.** Go 1.27, standard library HTTP routing and `log/slog`. The
dependencies are gRPC and protobuf, the Prometheus client, the MaxMind
reader, and `errgroup`.

## What you said you'd look at

**Coding style.**
- **Formatting and lint:** gofmt and goimports, plus a strict golangci-lint
  config (`.golangci.yml`) that reports 0 issues.
- **No hidden state:** no mutable global state, no `init()`, and every
  dependency passed in explicitly.
- **Comments say why:** every exported identifier has a doc comment, and
  comments explain decisions, not mechanics.
- **Tests:** each test name reads as a statement of behavior. Tables are
  used where one rule has many cases.

**Code organization.**
- **Packages:** one per responsibility under `internal/` (D6). The domain is
  `countrycheck` and knows nothing about transports. `httpapi` and `grpcapi`
  are thin adapters, and `geoip` owns the database and hot reload. `app`
  wires everything, and `cmd/geoipcheck` is a few lines of `main`.
- **Interfaces:** adapters depend on one-method interfaces defined where
  they're used, so each package tests without the others.
- **Tests:** three layers. Each behavior is asserted once, at the layer
  that owns it (D16).

**API design.**
- **Path:** versioned (`/v1/`), with a JSON body. The full IP never appears
  in a URL or in access logs.
- **Validation:** strict. Unknown fields, bad IPs, and bad country codes all
  get a 400 that says what's wrong and where.
- **Country codes:** checked against the real ISO list. `UK` is rejected
  with "did you mean GB". A format-only check would accept `UK` and then
  silently deny every UK user, because MaxMind returns `GB`.
- **Errors:** RFC 9457 problem+json, with a stable `code` to branch on and a
  `detail` for humans. gRPC validation and `UNAVAILABLE` errors carry the
  same codes in `ErrorInfo`.
- **Response shape:** no `allowed` boolean next to `decision` (D15). Two
  fields that can disagree is a bug waiting to happen.

**Operations: logging and error handling.**
- **Logs:** one structured JSON line per request, with the request ID,
  decision, status, and latency. The client IP is logged only as its /24 or
  /48 prefix. The log level follows blame: client mistakes are info, server
  faults are error.
- **Errors:** they are typed in the domain (`ValidationError` with code,
  field, and suggestion; `ErrDatabaseUnavailable`) and mapped to HTTP or
  gRPC once, at the edge. A panic becomes a 500 and is logged with its
  stack. Bad configuration reports every problem at once and exits with
  code 2.
- **Metrics and health:** Prometheus metrics for decisions, latency,
  database age, and reload results. Readiness is separate from liveness.
- **Shutdown and runbook:** graceful shutdown drains before stopping. Alert
  rules and a runbook are in [docs/operations.md](docs/operations.md).

## Bonus items

**A plan for keeping the data up to date, and an implementation.** Both.
- **Downloads:** `geoipupdate` (a CronJob in Kubernetes, a container in
  compose) writes the file.
- **Reload:** the service polls it and swaps in a new copy without a
  restart. A corrupt download never replaces a good database.
- **Staleness:** measured from the database's build date. It is logged, and
  exported as a metric to alert on.
- **Readiness:** a stale database doesn't take pods out of service. A
  MaxMind outage shouldn't become our outage.
- **Plan:** [docs/operations.md](docs/operations.md#keeping-the-database-fresh).

**Dockerfile.** [deploy/Dockerfile](deploy/Dockerfile) is a multi-stage
build that produces a static binary on distroless and runs as non-root.
[deploy/compose.yaml](deploy/compose.yaml) runs the service with
`geoipupdate` locally.

**Kubernetes YAML.** [deploy/k8s/](deploy/k8s/README.md) is a kustomize base
for an existing cluster. It has the Deployment, Service, HPA, PDB,
NetworkPolicies, restricted Pod Security, the download CronJob, and a
shared read-only volume for the database.

**gRPC in addition to HTTP.** It uses the same core, the same validation,
and the same error codes, on its own port. `grpc.health.v1` is backed by
the same readiness check as `/readyz`.

**Other extensions.** Each comes with a design-meeting justification in the
[README](README.md#other-extensions):
- the three-way decision;
- ISO country validation with suggestions;
- request IDs and privacy-safe access logs;
- metrics, health, and readiness;
- graceful shutdown;
- a CI workflow for everything my machine couldn't run.

[docs/extensions.md](docs/extensions.md) covers what I'd build next and what
would make each item worth building.

## What I verified and what I didn't

Verified locally, on Windows:
- **Tests:** unit, integration, and end-to-end. The e2e tests run the whole
  service on real ports, including hot reload and graceful shutdown.
- **Static checks:** `go vet`, golangci-lint, `buf lint`, and govulncheck.
- **Manifests:** `kustomize build | kubeconform -strict`. All 9 resources
  are valid.

Not verified locally:
- **CI.** The workflow is written but hasn't run: the repo has no remote
  yet. It's the first thing to watch after the first push.
- **The race detector.** This machine has no C compiler. CI is set up to
  run every test layer with `-race`.
- **Docker.** It isn't installed here. CI is set up to build the image.
- **A live cluster.** The manifests are schema-valid but haven't been
  applied. [deploy/k8s/README.md](deploy/k8s/README.md) lists what I'd check
  first.
- **A real GeoLite2 database.** Tests use MaxMind's published test
  databases. The EULA doesn't allow committing the real one.

## AI usage

I used Claude (Claude Code) throughout. [AI_USAGE.md](AI_USAGE.md) is the
full log: every prompt (planning prompts summarized, phase prompts quoted),
every proposal and review finding, and whether I accepted or rejected it,
with the reason.

**How I worked.**
1. **Plan first.** I wrote the design with the AI before any code. The AI
   laid out options for each decision. [docs/design.md](docs/design.md)
   records each one, the alternatives it beat, and who made it: ten are
   mine (D1–D3, D5, D7, D11, D12, D14–D16), and six are AI
   recommendations I accepted.
2. **Phase by phase.** The AI implemented one phase at a time, test-first,
   against that plan.
3. **Review after each phase.** A separate AI review agent read each diff
   cold. Every finding is logged with a verdict.

**Why I accepted code.** The tests had to pass and the review had to come
back clean. The code also had to match the design and read like code I'd
write myself. Some fixes from review were real bugs I'm glad were caught:
- Teredo IPv6 addresses resolved to the relay's country, not the user's.
- The raw client IP leaked into logs through an error message.
- A clean shutdown could exit with code 1 because of a startup race.
- An alert rule could never fire for HTTP-only errors.

**Why I rejected things.** Some examples:
- **The AI committed work without being asked.** Rejected and undone. I
  commit.
- **The AI's first validation design checked country codes by format
  only.** Rejected: it accepts `UK`. The AI found that flaw itself before
  any code was written.
- **Review: skip the shutdown drain when a server fails.** Rejected: the
  other transport is still taking traffic, so it still needs the drain.
- **Review: warn on unknown `GEOIPCHECK_*` variables.** Rejected as not worth
  the code for eleven variables.
- **Review: implement gRPC health `Watch`.** Rejected: clients treat
  Unimplemented as healthy, and readiness only changes at startup and
  shutdown.
- **Review: test that the IP never appears in logs by injecting it into an
  error.** Rejected: that test can only fail. The fix went where the leak
  was.
