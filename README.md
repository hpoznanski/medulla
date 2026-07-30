<p align="center">
  <img src="assets/logo.svg" width="130" alt="Medulla logo">
</p>

# ◉ Medulla

Web admin UI for **Elasticsearch** and **OpenSearch** clusters: **multi-cluster management, built-in RBAC, LDAP auth, audit logging** — in a single ~10 MB static binary.

Medulla is the brain stem: small, always on, controls the vital functions.

## Features

- **Clusters landing page** — every cluster at a glance: health, version/flavor, nodes, indices, docs, unassigned shards. Cluster switcher on every page.
- **Overview** — node stats (heap/disk/CPU), a visual shard allocation grid with per-shard hover details, and an *allocation explain* panel that shows the actual root cause for every unassigned shard (deciders parsed, grouped by cause).
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
- Login rate limiting per username — independent of proxy topology, immune to X-Forwarded-For spoofing. Set `trusted_proxies` (ingress CIDRs) so audit logs record real client IPs instead of the proxy's.
- CSRF: SameSite=Lax + Origin check on all non-GET requests.
- Audit: every login attempt, denial, and state-changing request logged as JSON with user/roles/method/path/outcome/IP — never request bodies.

## Demo

Try everything locally in one command — Medulla plus a 2-node ES 8.13 cluster and an OpenSearch 2.13 node, snapshot repository prewired:

```sh
docker-compose up -d --build
```

Open http://localhost:8080 and sign in as:

| user | password | role |
|---|---|---|
| `reader` | `readerpw` | viewer — read-only everywhere |
| `writer` | `writerpw` | operator — full cluster operations |

The two accounts demonstrate RBAC: log in as each and compare what the UI offers. The demo stack is for evaluation only — plaintext demo credentials, no TLS.

## Configuration

One YAML file. Full annotated reference: [`config.example.yaml`](config.example.yaml). Secrets never go in the file — use `${ENV_VAR}` or `${file:/path}` interpolation anywhere a value is expected.

Minimal working config:

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

### Top level

```yaml
listen: ":8080"        # default
env: production        # enforces session.secret; omit for dev
trusted_proxies:       # ingress/reverse-proxy CIDRs; audit logs then record the
  - 10.42.0.0/16       # real client IP from X-Forwarded-For instead of the proxy's
```

### Clusters

Three auth types — `none`, `basic`, `api_key` — plus optional TLS settings per cluster:

```yaml
clusters:
  - name: dev                     # no auth (dev, or network-trusted)
    url: http://es-dev:9200

  - name: prod-eu                 # basic auth
    url: https://es-eu:9200
    auth: {type: basic, username: medulla, password: "${ES_EU_PASSWORD}"}

  - name: prod-us                 # API key (ES 8 / OpenSearch)
    url: https://es-us:9200
    auth: {type: api_key, api_key: "${ES_US_API_KEY}"}

  - name: onprem                  # custom CA / self-signed
    url: https://es-onprem:9200
    auth: {type: basic, username: medulla, password: "${ONPREM_PASSWORD}"}
    tls:
      ca_file: /etc/medulla/ca.pem   # omit when the cert chains to a system CA
      # insecure: true               # skip verification — never in production
```

Give each Medulla service account the least ES privilege that covers what its users need — that is the real blast-radius limiter.

### Roles

Named permission sets scoped to clusters (atoms listed under [RBAC](#rbac)):

```yaml
roles:
  admin:     {clusters: ["*"],       permissions: [admin]}
  operator:  {clusters: ["*"],       permissions: [view, index:write, alias:write, template:write, snapshot:write, cluster:write, rest:full]}
  developer: {clusters: [staging],   permissions: [view, rest:get]}
  viewer:    {clusters: ["*"],       permissions: [view]}
```

### Users: LDAP

Service-account bind, then user search + bind. Roles come from directory groups (`group_to_role`), individual users (`user_to_role`), or both — results are unioned. Authenticated users with no mapped role are denied.

```yaml
ldap:
  url: ldaps://ldap.example.com
  bind_dn: "${LDAP_BIND_DN}"
  bind_password: "${LDAP_BIND_PASSWORD}"
  user_base: ou=people,dc=example,dc=com
  user_filter: "(uid=%s)"                # default; use (sAMAccountName=%s) for AD
  group_to_role:
    "cn=es-admins,ou=groups,dc=example,dc=com": admin
    "cn=devs,ou=groups,dc=example,dc=com":      developer
  user_to_role:                          # single users, no directory group needed
    jdoe: operator
```

### Users: local

Work standalone or as fallback when LDAP is unreachable. Passwords: bcrypt hash (recommended) or plaintext via interpolation:

```yaml
local_users:
  - name: admin
    password: "bcrypt:$2a$10$..."        # htpasswd -bnBC 10 "" 'pw' | tr -d ':\n'
    roles: [admin]
  - name: breakglass
    password: "${BREAKGLASS_PASSWORD}"
    roles: [operator]
```

### Sessions

Stateless HMAC-signed cookies — replicas share the secret, no store, no sticky sessions:

```yaml
session:
  secret: "${SESSION_SECRET}"   # >=32 chars; "new,old" list rotates keys without logouts
  ttl: 12h
  # insecure_cookie: true       # plain-HTTP dev only; cookies are Secure by default
```

## Production deployment (Kubernetes)

Helm chart in [`deploy/helm/medulla`](deploy/helm/medulla) — hardened defaults (nonroot, read-only rootfs, no capabilities, no SA token), config-checksum rollouts, zone spreading, PDB. See the [chart README](deploy/helm/medulla/README.md) for installation, secret handling, and the security model.

```sh
helm install medulla oci://ghcr.io/hpoznanski/charts/medulla --version 0.1.0 -f my-values.yaml
```

## Development

```sh
go build ./cmd/medulla     # build
go test -race ./...        # unit tests (fake ES via httptest — no docker needed)
go vet ./... && gofmt -l . # hygiene
```

Layout: `internal/config` (YAML + interpolation + validation), `internal/auth` (LDAP, local users, sessions, rate limit), `internal/rbac` (permission checks), `internal/es` (thin REST client, flavor detection), `internal/web` (handlers, embedded templates/CSS).

## Contributing

Issues and merge requests are welcome — bug reports, features, docs. Keep the dependency philosophy in mind: PRs adding dependencies need a strong justification.

## License

[AGPL-3.0](LICENSE). © Hubert Poznanski.

In short: use it, self-host it, modify it, contribute back — freely. If you distribute a modified version or run one as a service for others, your changes must stay open under the same license.
