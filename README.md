# ddo-cloudflare

The Cloudflare sidecar for [docker-dns-operator](https://github.com/mrkhachaturov/docker-dns-operator). It owns the conversation with Cloudflare. The operator tells it which DNS records should exist; this process writes them through the Cloudflare REST API.

One container, one Cloudflare account. Got two accounts (personal + work)? Run two containers — the operator references each by its own env var, the user routes records per-container with a label.

## What it does

The sidecar is a small Go process with three jobs:

- Apply changes the operator sends (create, update, delete DNS records in the zones the API token can manage).
- Read the current state back so the operator can spot drift and reconcile.
- Tag every row it writes with the operator's instance label in the record's `comment` field, so two operators can share a Cloudflare account without overwriting each other's records.

It does not own DNS records of its own. If the operator stops sending a record, the sidecar will delete it on the next cycle.

## How to configure

Required:

| Env | What it is |
|---|---|
| `CLOUDFLARE_API_TOKEN` (or `CLOUDFLARE_API_TOKEN_FILE`) | Cloudflare API token. Needs Zone:Read + Zone:DNS:Edit scopes on the zones it manages. |

Optional:

| Env | Default | Notes |
|---|---|---|
| `CLOUDFLARE_ZONES` | `""` | Comma-separated zone names. When set, only records inside one of these zones are accepted. Empty means "every zone the token can see". |
| `CLOUDFLARE_DEFAULT_TTL` | `1` | TTL in seconds for records sent without an explicit TTL. The Cloudflare convention: `1` means "automatic". |
| `CLOUDFLARE_PROXIED_DEFAULT` | `false` | Default Proxied (orange-cloud) value for A and CNAME records when the operator hasn't sent an explicit `cloudflare-proxied` providerSpecific entry. |
| `WEBHOOK_LISTEN` | `:9090` | HTTP bind address. |

The Cloudflare API token should be a scoped token, not the global API key. Create it under My Profile → API Tokens with `Zone:DNS:Edit` and `Zone:Zone:Read` for the relevant zones.

## How to run

Drop it next to the operator in compose:

```yaml
services:
  ddo-cloudflare:
    image: ddo-cloudflare:dev
    build: ./sidecars/ddo-cloudflare
    environment:
      CLOUDFLARE_API_TOKEN: ${CLOUDFLARE_API_TOKEN}
      CLOUDFLARE_ZONES: example.com

  docker-dns-operator:
    image: mrkhachaturov/docker-dns-operator
    environment:
      WEBHOOK_CF_URL: http://ddo-cloudflare:9090
```

The operator picks the sidecar up because of the `WEBHOOK_<NAME>_URL` pattern. The `<NAME>` part (here `CF`) is what container labels then reference: `"providers": ["cf"]`.

### Multiple Cloudflare accounts

The reason this sidecar exists. Run a container per account, hand each its own token, route records:

```yaml
services:
  ddo-cloudflare-personal:
    image: ddo-cloudflare:dev
    environment:
      CLOUDFLARE_API_TOKEN: ${CLOUDFLARE_API_TOKEN_PERSONAL}

  ddo-cloudflare-work:
    image: ddo-cloudflare:dev
    environment:
      CLOUDFLARE_API_TOKEN: ${CLOUDFLARE_API_TOKEN_WORK}

  docker-dns-operator:
    environment:
      WEBHOOK_CF_PERSONAL_URL: http://ddo-cloudflare-personal:9090
      WEBHOOK_CF_WORK_URL:     http://ddo-cloudflare-work:9090
```

Then on a target container:

```yaml
labels:
  docker-dns-operator:1: |
    [
      { "type": "A", "name": "blog.mydomain.com",  "address": "10.0.0.5", "providers": ["cf-personal"] },
      { "type": "A", "name": "app.workdomain.com", "address": "10.0.0.5", "providers": ["cf-work"] }
    ]
```

Same image, two backends, no extra cleverness.

## How ownership tagging works

Every row this sidecar writes gets a value stamped into the record's `comment` field on Cloudflare. That value is whatever `labels.owner` arrives in the operator's request, written through verbatim. Two consequences:

The sidecar never touches a row whose comment doesn't match the calling operator's label. Other rows are visible on read but never modified or deleted. That covers manual entries you made in the Cloudflare dashboard, and rows belonging to a different operator instance pointed at the same account.

The sidecar does not read `PROJECT_LABEL` or `INSTANCE_ID` itself. The operator decides the label; the sidecar persists it. The same sidecar image can be shared by any number of operators, each writing under its own identity. If a create lands on a name that already has a row with a different comment, the create is logged and skipped. No silent overwrite, ever.

## The Cloudflare proxy toggle

Cloudflare's defining feature: A and CNAME records can be "proxied" (orange-cloud, hides origin behind Cloudflare) or "DNS-only" (grey-cloud, just resolves). This sidecar round-trips the toggle through external-dns' standard providerSpecific property:

```jsonc
{
  "dnsName": "app.example.com",
  "recordType": "A",
  "targets": ["10.1.2.3"],
  "labels": { "owner": "docker-dns-operator:1" },
  "providerSpecific": [
    { "name": "external-dns.alpha.kubernetes.io/cloudflare-proxied", "value": "true" }
  ]
}
```

Rules:

- The property is honored on A and CNAME records only. Cloudflare itself rejects proxy on MX/NS, so the sidecar zeroes the field on those types regardless of what the operator sends.
- An explicit `"false"` always wins over `CLOUDFLARE_PROXIED_DEFAULT`.
- Absence of the property falls back to `CLOUDFLARE_PROXIED_DEFAULT` (default `false`).
- On `GET /records`, every managed A/CNAME row carries the current proxy state back to the operator so drift detection sees it.

The operator's container-label `providerOptions.cf.proxy: true|false` becomes this providerSpecific entry — the user doesn't need to know the internal wire format.

## Record types

| Type | Cloudflare fields used |
|---|---|
| A | `content`, `proxied` |
| AAAA | `content` |
| CNAME | `content`, `proxied` |
| NS | `content` |
| MX | `content` (mail server), `priority` (split from `"<priority> <host>"`) |

Anything else (TXT, SRV, etc.) is logged and skipped.

## Health probe

`GET /healthz` issues a cheap `zones.list?per_page=1` against Cloudflare. 200 if the API responds, 503 if it doesn't. Use it as a Docker Compose healthcheck.

## Local development

```bash
go build ./cmd/webhook
go test ./... -race -count=1
```

The HTTP layer is plain `net/http`. The Cloudflare layer uses `github.com/cloudflare/cloudflare-go/v7`. `internal/orchestrator/` is where the apply/list logic lives, including the comment-tag semantics and the proxy round-trip. Everything else is glue.

## License

MIT.
