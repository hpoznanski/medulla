<p align="center">
  <img src="assets/logo.svg" width="130" alt="Medulla logo">
</p>

# ◉ Medulla

Web admin UI for **Elasticsearch** and **OpenSearch** clusters. A spiritual successor to [Cerebro](https://github.com/lmenezes/cerebro) — same job, plus the things Cerebro never had: **multi-cluster management, built-in RBAC, LDAP auth, audit logging** — in a single ~10 MB static binary.

Medulla is the brain stem: small, always on, controls the vital functions.

## Features

- **Clusters landing page** — every cluster at a glance: health, version/flavor, nodes, indices, docs, unassigned shards. Cluster switcher on every page.
- **Overview** — node stats (heap/disk/CPU), Cerebro-style shard allocation grid with per-shard hover details, and an *allocation explain* panel that shows the actual root cause for every unassigned shard (deciders parsed, grouped by cause).
- **Index management** — create, delete (type-name-to-confirm), open/close, refresh, flush, force-merge; settings/mappings/aliases detail view.
- **Aliases** — list, add, remove.
- **Index templates** — list, inspect, create/update via JSON editor, delete.
- **Snapshots** — repository management, create/restore/delete snapshots.
- **Cluster settings** — persistent/transient/defaults browser, edit with reset-to-default.
- **Analyze** — `_analyze` playground for analyzers and tokenizers.
- **Cat browser** — all `_cat` endpoints as sortable tables, column order preserved.
- **REST console** — free-form requests, permission-gated: `rest:get` users get GET/HEAD only, enforced server-side.
- **ES 7, ES 8 and OpenSearch** — flavor auto-detected per cluster.

## Philosophy: fewer dependencies, smaller attack surface

The entire dependency tree, deliberately:

| Dependency | Why it exists |
|---|---|
| `gopkg.in/yaml.v3` | config parsing |
| `github.com/go-ldap/ldap/v3` | LDAP bind |
| `golang.org/x/crypto` | bcrypt |

Everything else is the Go standard library. Consequences:

- **No JavaScript at all.** Server-rendered `html/template` pages under a `default-src 'none'` CSP. No npm, no lockfile with 1,400 transitive packages, no supply-chain surface. Dynamic UI (dropdowns, collapsibles, tooltips) is native HTML/CSS.
- **No ES client library.** Medulla is a smart proxy; a ~300-line `net/http` client covers every admin API on ES7/ES8/OpenSearch — no version-split client modules, no churn when ES 9 lands.
- **No database.** Config is one YAML file; sessions are stateless HMAC-signed cookies. Replicas need zero coordination.
- **`FROM scratch` image.** Static binary + CA certs. No shell, no package manager — a compromised container has no tooling.

Every dependency you don't have is a CVE you don't patch.

## Architecture

```
browser ──► medulla (single binary) ──► ES/OpenSearch clusters
             │  auth: LDAP bind + local users (bcrypt)
             │  sessions: stateless HMAC cookies (key rotation supported)
             │  rbac: per-route middleware, per-cluster scoping
             │  audit: JSON logs to stdout (type=audit), no payloads
             └─ config: /etc/medulla/config.yaml (${ENV} / ${file:...} interpolation)
```

**Access model:** service-account proxy. Medulla holds per-cluster credentials (basic auth / API key — also works behind authenticating reverse proxies); users authenticate to Medulla, and app-level RBAC decides what each user may do on which cluster. Users never see ES credentials. Give Medulla's ES accounts least privilege — that is the real blast-radius limiter.

### RBAC

Permission atoms: `view`, `index:write`, `alias:write`, `template:write`, `snapshot:write`, `cluster:write`, `rest:get`, `rest:full`, `admin` (implies all; `rest:full` implies `rest:get`).

Roles are named sets of atoms scoped to a cluster list (`"*"` for all). LDAP groups map to roles; local users get roles directly. UI controls render only when permitted, and every route enforces server-side. Note: `rest:get` reads the *entire* ES API — treat it as read-admin.

```yaml
roles:
  operator:
    clusters: ["*"]
    permissions: [view, index:write, alias:write, template:write, snapshot:write, cluster:write, rest:full]
  developer:
    clusters: [staging]
    permissions: [view, rest:get]
```

### Security notes

- Secrets enter via `${ENV_VAR}` or `${file:/path}` interpolation — from K8s Secrets; nothing sensitive in the config file itself. Values with newlines are rejected (YAML injection guard). Secrets redact themselves in all log/marshal output.
- Session cookies: HttpOnly, SameSite=Lax, Secure by default; `session.secret` accepts a comma-separated key list for zero-logout rotation. Multi-replica works with a shared secret, no sticky sessions.
- Login rate limiting per client IP; set `trusted_proxies` (ingress CIDRs) so X-Forwarded-For is honored only from your proxies.
- CSRF: SameSite=Lax + Origin check on all non-GET requests.
- Audit: every login attempt, denial, and state-changing request logged as JSON with user/roles/method/path/outcome/IP — never request bodies.

## Quick start

```sh
docker-compose up -d --build
# → http://localhost:8080
#   reader / readerpw   (viewer: read-only)
#   writer / writerpw   (operator: full ops)
```

Dev stack: Medulla + 2-node ES 8.13 + OpenSearch 2.13, snapshot repo path prewired.

## Configuration

See [`config.example.yaml`](config.example.yaml). Minimal:

```yaml
clusters:
  - name: prod
    url: https://es-prod:9200
    auth: {type: basic, username: medulla, password: "${ES_PASSWORD}"}
roles:
  admin: {clusters: ["*"], permissions: [admin]}
local_users:
  - {name: admin, password: "${ADMIN_PASSWORD}", roles: [admin]}
session:
  secret: "${SESSION_SECRET}"   # >=32 chars, required when env: production
```

Run: `medulla -config /etc/medulla/config.yaml`. Logs are JSON on stdout. `/healthz` for probes.

## Kubernetes

Helm chart in [`deploy/helm/medulla`](deploy/helm/medulla) — hardened defaults (nonroot, read-only rootfs, no capabilities, no SA token), config-checksum rollouts, zone spreading, PDB. Secrets come from an `existingSecret` you create (e.g. from an eyaml pipeline); the chart never renders secret material.

```sh
helm install medulla deploy/helm/medulla -f deploy/helm/medulla/values-example.yaml
```

## Development

```sh
go build ./cmd/medulla     # build
go test -race ./...        # unit tests (fake ES via httptest — no docker needed)
go vet ./... && gofmt -l . # hygiene
```

Layout: `internal/config` (YAML + interpolation + validation), `internal/auth` (LDAP, local users, sessions, rate limit), `internal/rbac` (permission checks), `internal/es` (thin REST client, flavor detection), `internal/web` (handlers, embedded templates/CSS).

## License

TBD.
