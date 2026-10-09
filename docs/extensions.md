# Extensions

What I'd build next, in rough priority order, and where each change goes.
None of this is built. I don't want to design for consumers that don't exist
yet. Each item says what would make it worth doing.

## VPN and proxy detection

Phase 1 ignores VPNs. The obvious next step is a signal for anonymizing
networks, so a customer can deny logins from them.

- **Data:** MaxMind GeoIP2 Anonymous IP (paid), or the free GeoLite2-ASN
  database plus a list of hosting-provider ASNs. ASN is weaker. A VPS isn't
  always a VPN, and residential proxies don't show up at all.
- **API:** a new reason under `denied`, for example `anonymous_network`,
  gated by a new optional request field (`deny_anonymous: true`). Callers
  that don't send it get today's answers. Adding a reason doesn't break
  callers that branch on `decision` (README, "Branch on `decision`").
- **Code:** a second `Reader` in `internal/geoip` with its own watcher and
  metrics, and one more step in `countrycheck.Service.Check` before the
  country lookup. Readiness needs a decision: ready with country only, or
  only with both. I'd say country only. Anonymous detection degrades, the
  login check doesn't.

Worth doing when: a customer asks, or fraud data shows logins via VPN.

## Batch checks

`POST /v1/country-checks:batch` with up to N IPs against one allowlist. Only
useful if a caller re-checks many sessions at once, for example after a
customer tightens its allowlist. The gateway calls once per login, so
today there's no consumer.

## Region-level rules

`allowed_regions` (ISO 3166-2, like `US-CA`) needs GeoLite2-City. City is
about 70 MB against Country's 9 MB, which hits the D9 revisit trigger: two
copies in memory during reload start to matter, and mmap with reference
counting becomes the better trade.

## Allowlist by reference

Today the gateway sends the full allowlist on every call. If allowlists grow
or the gateway wants a smaller payload, the service could take a
`customer_id` and read the allowlist itself. I don't think this is a good
trade. It gives the service a dependency on the customer DB, a cache, and
an invalidation problem, all to save a few hundred bytes on a typical call
(1.5 KiB at the 250-country cap). The current boundary (D4) keeps the
service stateless.

## Server reflection and richer grpcurl output

Not enabled ([README](../README.md#api-contracts)). Turning it on is one line
in `internal/grpcapi`. I'd enable it behind a config flag for non-production
environments only, since it advertises the full API surface to anyone who
can reach the port.

## Per-call gRPC balancing

A headless Service so gateway clients can use `round_robin` instead of
waiting out `MaxConnectionAge`
([operations.md](operations.md#grpc-load-balancing)). One manifest. Worth
doing when the gateway moves to gRPC without a mesh and 5-minute
rebalancing after a scale-up is too slow.

## Tracing

OpenTelemetry spans via `otelhttp` and `otelgrpc`, and the request ID
attached as a span attribute. The service is one hop that takes under 1 ms,
so a span says little that the access log doesn't. Worth doing when the
gateway is traced and someone wants the hop in the waterfall.

## Faster reload detection

fsnotify as a fast path, with the 60s poll as the floor (D8 revisit).
Worth doing if someone needs a new DB live in seconds instead of a minute.
Nobody does today. MaxMind publishes twice a week.
