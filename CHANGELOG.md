# Changelog

All notable changes to **ddo-cloudflare** are recorded here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ddo-cloudflare is a webhook sidecar for [docker-dns-operator](https://github.com/mrkhachaturov/docker-dns-operator), implementing the [external-dns webhook provider v1 contract](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/) against the Cloudflare API. The same sidecar works with the upstream kubernetes-sigs/external-dns controller.

## [Unreleased]

## [0.1.2] — 2026-06-07

### Fixed
- `GET /` now emits the domain filter as `include` (per external-dns `endpoint.DomainFilter`) instead of the legacy `filters` key, which upstream parses as an unset filter — previously every record was routed through the sidecar regardless of zone. (Same fix shipped in ddo-mikrotik / ddo-rfc2136 0.2.0.)

### Notes
- Wildcard DNS names (`*.example.com`) work as-is — Cloudflare supports wildcard records natively and the sidecar passes the name straight through, so no code change was needed (verified end-to-end against the live Cloudflare API). This brings cf to parity with the wildcard support added to the other sidecars in 0.2.0.

## [0.1.1] — 2026-05-25

### Added
- `healthcheck` subcommand on the webhook binary. Invoked as `/usr/local/bin/webhook healthcheck`, performs a local `GET /healthz` (using `WEBHOOK_LISTEN` for the address) and exits `0` on a 2xx response, `1` otherwise.
- `HEALTHCHECK` directive in the Dockerfile wired to that subcommand. The image is distroless (no shell), so the binary is its own probe — the canonical distroless Go pattern, avoiding a second artifact to build/sign/SBOM.

## [0.1.0] — 2026-05-25

First tagged release.

### Added
- External-dns webhook provider v1 endpoints: `GET /`, `GET /records`, `POST /records`, `POST /adjustendpoints`, `GET /healthz`.
- Cloudflare DNS via [cloudflare-go/v7](https://github.com/cloudflare/cloudflare-go) — A, AAAA, CNAME, MX, NS.
- Optional zone allow-list via `CLOUDFLARE_ZONES` (empty = every zone the token can see).
- Cloudflare "orange-cloud" proxy toggle round-tripped through the `external-dns.alpha.kubernetes.io/cloudflare-proxied` providerSpecific property; default controlled by `CLOUDFLARE_PROXIED_DEFAULT`.
- Default TTL via `CLOUDFLARE_DEFAULT_TTL` (Cloudflare convention: `1` = automatic).
- Ownership round-trip through the Cloudflare record `comment` field — the operator stamps `labels.owner` on each Endpoint, the sidecar persists it verbatim and reads it back on `GET /records`. Two operators with different `INSTANCE_ID`s can safely share the same zone.
- API token loadable via `CLOUDFLARE_API_TOKEN` (env) or `CLOUDFLARE_API_TOKEN_FILE` (Docker secret).
- Optional `CLOUDFLARE_DEBUG=true` to dump SDK request/response bodies via `option.WithDebugLog`.

### Fixed
- HTTP/2 connection pool poisoning against `api.cloudflare.com`. The SDK's default `http.Client` has no timeout and reuses keep-alive H2 connections; intermediate NAT/edge silently drops idle TCP without RST, and the next request blocks forever on a TLS read. Mitigated with a custom `http.Client` (`DisableKeepAlives=true`, `Timeout=60s`, `ResponseHeaderTimeout=30s`, `IdleConnTimeout=20s`) plus `option.WithRequestTimeout(30s)` as defence in depth. Fresh TCP per request — the ~100ms handshake overhead is irrelevant at CRON-tick cadence.
- SDK `MaxRetries` lowered from the default 10 to 2 — caps the existing retry storm on 408/409/429/5xx so a stuck request can no longer run for ten cumulative retry windows.

### Notes
- Distroless image, pure Go, CGO disabled.
- See [README.md](README.md) for env vars and deployment examples.

[Unreleased]: https://github.com/mrkhachaturov/ddo-cloudflare/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/mrkhachaturov/ddo-cloudflare/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/mrkhachaturov/ddo-cloudflare/releases/tag/v0.1.1
[0.1.0]: https://github.com/mrkhachaturov/ddo-cloudflare/releases/tag/v0.1.0
