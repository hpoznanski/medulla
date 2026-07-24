# Medulla — Design Spec

Web admin tool for Elasticsearch/OpenSearch clusters. Cerebro functionality, extended with multi-cluster management and app-level RBAC. Single Go binary, minimal dependencies, Kubernetes-native deployment.

Date: 2026-07-21. Status: approved for planning.

## Goals

- Full Cerebro feature parity plus: multi-cluster, RBAC, OpenSearch support.
- Minimal dependency surface — stdlib-first, no JS toolchain, no DB.
- Secure by default: scratch image, non-root, least-privilege, redacted logging.
- Simple to operate: one binary, one YAML config, stateless replicas.

## Non-goals (v1)

- Persisted audit trail (audit goes to stdout logs only; log pipeline owns retention).
- OIDC (LDAP + local users only; OIDC is v2).
- Per-user ES credential pass-through (service-account proxy model only).
- Document-level search/browse UI — this is an admin tool, not a data explorer.
- Managed cloud offerings (Elastic Cloud, AWS OpenSearch Service / SigV4). Self-hosted ES/OpenSearch only.

## Architecture

Single Go binary. Server-rendered HTML (`html/template`) + htmx (vendored via `go:embed`) for dynamic fragments. `net/http` stdlib router (Go 1.22+ method/path patterns).

**Cluster access model: service-account proxy.** App holds per-cluster credentials from config; all ES calls go through the backend with those credentials; app-level RBAC decides per-user, per-cluster capability. Users never hold ES credentials.

### Dependencies (complete list)

| Dependency | Purpose |
|---|---|
| `gopkg.in/yaml.v3` | config parsing |
| `github.com/go-ldap/ldap/v3` | LDAP bind |
| `golang.org/x/crypto` | bcrypt for local user hashes |

Everything else stdlib. No ES client library — Medulla is a smart proxy; plain `net/http` against the REST API. Official clients are version-split (v7/v8/opensearch-go = three modules), typed for document CRUD not admin APIs, and add churn without value here.

### Layout

```
cmd/medulla/main.go
internal/config/      # YAML load, validation, ${VAR} + ${file:...} interpolation, redacted marshal
internal/auth/        # LDAP bind, local users, HMAC-signed cookie sessions
internal/rbac/        # permission model, route middleware
internal/es/          # thin REST client (~300 LOC): auth injection, TLS, flavor detection, JSON decode
internal/web/         # handlers, templates, embedded static assets (htmx, CSS)
```

## Multi-cluster and flavor handling

- Clusters declared in YAML: name, URL, auth (basic / API key / none), TLS (custom CA, insecure flag). Basic auth covers both native ES security and clusters fronted by an authenticating reverse proxy (nginx/haproxy) — same `Authorization` header either way, no special handling.
- Self-hosted ES/OpenSearch only; no cloud-provider request signing.
- On first contact per cluster: `GET /` → read `version.number` and `version.distribution` → flavor = ES7 | ES8 | OpenSearch. Capability differences isolated in `internal/es` (a small capability map; e.g. component templates, ILM vs ISM).
- Routes namespaced `/c/{cluster}/...`; cluster switcher in the page header.

## RBAC

### Permission atoms

`view`, `index:write` (create/delete/open/close/refresh/flush/forcemerge/clear-cache/settings), `alias:write`, `template:write`, `snapshot:write`, `rest:get` (console, GET/HEAD only), `rest:full`, `admin` (implies all).

REST console enforcement for `rest:get`: HTTP method allowlist (GET, HEAD) plus path denylist for state-changing GET endpoints.

### Default roles

| Role | Permissions | Intended user |
|---|---|---|
| `admin` | `admin`, all clusters | platform team |
| `operator` | `view, index:write, alias:write, template:write, snapshot:write, rest:full` | on-call / SRE |
| `developer` | `view, rest:get` (optionally `index:write` on dev clusters) | debugging, mapping inspection |
| `viewer` | `view` | read-only stakeholders |

Roles are YAML-defined; defaults above are shipped examples. Cluster scoping is orthogonal — a role binding lists which clusters it covers (`"*"` allowed). Custom roles compose from the same atoms.

### Config shape

```yaml
clusters:
  - name: prod-eu
    url: https://es-prod-eu:9200
    auth: { type: basic, username: medulla, password: ${ES_PROD_EU_PASSWORD} }
    tls: { ca_file: /etc/medulla/ca/prod-eu.pem }

roles:
  ops-admin:
    clusters: ["*"]
    permissions: [admin]
  dev-read:
    clusters: [staging, dev]
    permissions: [view, rest:get]

ldap:
  url: ldaps://ldap.example.com
  bind_dn: ${LDAP_BIND_DN}
  bind_password: ${LDAP_BIND_PASSWORD}
  user_base: ou=people,dc=example,dc=com
  group_to_role:
    "cn=es-admins,ou=groups,dc=example,dc=com": ops-admin

local_users:
  - name: admin
    password: ${ADMIN_PASSWORD}        # plaintext, or "bcrypt:$2a$..."
    roles: [ops-admin]

session:
  secret: ${SESSION_SECRET}            # comma-separated keys: sign with first, verify all
```

## Authentication and sessions

- Login: LDAP bind first (when configured), then local users. Local passwords accept plaintext or `bcrypt:`-prefixed hashes (plaintext acceptable because the source is a K8s Secret; bcrypt recommended).
- Sessions: stateless HMAC-signed cookie (username, roles, expiry). No server-side session store, no DB.
- Multi-replica: all replicas share `SESSION_SECRET` from the same K8s Secret — any replica validates any cookie; no sticky sessions. `secret` accepts comma-separated keys (sign with first, verify against all) for zero-logout rotation.
- Dev fallback: auto-generated key with boot warning. Refuse to start when `MEDULLA_ENV=production` and no secret set.
- Login rate limit: in-memory token bucket per source IP.

## Secrets handling

Chain: eyaml-encrypted values.yaml in git → decrypted at deploy → K8s Secret → container.

- Recommended: non-sensitive config as ConfigMap-mounted YAML; sensitive values via Secret-backed env vars using `${VAR}` interpolation. Alternative: whole config as Secret file mount, `defaultMode: 0400`. Both supported.
- Interpolation supports `${VAR}` (env) and `${file:/path}` (mounted file) — deployment chooses per value, no code difference.
- Env vars are safe in this deployment: scratch image (no shell, no exec tooling, single process, no children), values never inline in manifests (`secretKeyRef` only), redaction rule in our code.
- Never: secrets in image layers, CLI args, or logs. Config structs redact on marshal/String().
- Secrets held in memory only, loaded once at boot.
- Outside app scope, flagged to cluster admins: etcd encryption-at-rest, namespace RBAC on `get secrets`.
- Real blast-radius limiter: least-privilege ES service accounts per cluster — only the APIs Medulla needs, never superuser.

## Logging and audit

- All logs JSON via `log/slog` stdlib JSON handler, to stdout.
- Audit events: every state-changing request and every REST-console request emits a slog record with `type=audit`: timestamp, user, roles, cluster, HTTP method, path, response status, duration, source IP. **No request payloads** — bodies can contain sensitive data. Login attempts (success/failure, user, IP) audited too.
- Retention/shipping is the log pipeline's job (stdout → collector). No in-app storage.

## Features (full Cerebro parity + extensions)

- **Overview:** cluster health, node stats (disk/heap/CPU), shard allocation grid, relocating/unassigned shards with reasons.
- **Index ops:** create (settings/mappings), delete, open/close, refresh, flush, force-merge, clear cache, settings edit, mapping view.
- **Aliases:** list/add/remove, filtered aliases.
- **Templates:** index templates + component templates, flavor-aware CRUD.
- **Snapshots:** repository CRUD, snapshot create/restore/delete, status.
- **Analysis:** `_analyze` playground (analyzer/tokenizer/filter).
- **Cat browser:** any `_cat` endpoint, sortable tables.
- **REST console:** free-form request editor, pretty-printed response, permission-gated (`rest:get` / `rest:full`).
- **Extensions over Cerebro:** OpenSearch flavor support, app-level RBAC, multi-cluster switcher, shard-level reroute, dark mode.

## Error handling

ES errors surfaced verbatim in a UI banner (status + reason), never swallowed. Config validation fails fast at boot (crash loop is the visible failure mode). Per-cluster connectivity errors shown on the cluster switcher, do not block other clusters.

## Testing

Stdlib `testing` only — no assertion/mocking libraries. Table-driven throughout. Every feature lands with its tests in the same phase; a phase is done only when its tests pass.

- `internal/config`: valid/invalid YAML, every validation rule, `${VAR}` and `${file:...}` interpolation (set, unset, missing file), redaction (no secret appears in marshal/String output), production guard (`MEDULLA_ENV=production` without session secret refuses to start).
- `internal/auth`: local user login (plaintext, `bcrypt:`, wrong password, unknown user), LDAP bind against a fake LDAP server (success, bad credentials, group→role mapping, unreachable server), cookie sign/verify (valid, expired, tampered signature, tampered payload, key rotation — old key verifies, new key signs), login rate limit (bucket exhaustion and refill).
- `internal/rbac`: every permission atom × every route class (allow and deny), cluster scoping (`"*"`, named list, cluster not in list), `admin` implies-all, REST console gating for `rest:get` (method allowlist, state-changing-GET denylist), unauthenticated request → login redirect.
- `internal/es`: `httptest` fake cluster — flavor detection (ES7, ES8, OpenSearch, unknown), auth header injection (basic, API key, none), custom CA / insecure TLS, error propagation (4xx/5xx body surfaced verbatim), timeouts, unreachable cluster.
- `internal/web`: handler tests via `httptest` for every route — happy path plus ES-error path; audit log emission (record fields present, no request payload in output) for every state-changing route; security headers present on all responses.
- Coverage gate: `go test -cover` in CI; target ≥80% per package, failures block the phase.
- Fake-ES fixtures are real response bodies captured from live ES7/ES8/OpenSearch (checked into `testdata/`), so unit tests speak genuine cluster JSON.
- Integration smoke layer: build-tagged (`//go:build integration`) tests run against the docker-compose stack (ES8 + OpenSearch, URLs via env). ~10 tests: flavor detection, index create/delete, alias, fs-repo snapshot, `_analyze`, cat endpoints. Guards against fixture drift; run locally via `docker compose up -d && go test -tags integration`, in CI as a separate job. Not part of the coverage gate. No testcontainers dependency — reuses the dev compose file.
- No browser E2E framework in v1 — htmx pages are server-rendered, handler tests cover the contract. Manual smoke check via docker-compose stack per phase.

## Deployment

- Multi-stage Dockerfile: builder → `scratch` + CA certs. Static binary, `CGO_ENABLED=0`.
- Runtime posture: `runAsNonRoot`, `readOnlyRootFilesystem: true`, all capabilities dropped, no shell.
- 2–3 replicas across zones; stateless, no coordination needed.
- Dev stack `docker-compose.yml`: medulla + ES8 single node + OpenSearch single node + OpenLDAP.

## Delivery phases (all v1)

1. **P1 — skeleton:** config, auth (LDAP + local), RBAC middleware, sessions, cluster registry, overview + shard grid.
2. **P2 — index ops:** index CRUD/lifecycle, aliases, settings/mappings, cat browser.
3. **P3 — console + analysis:** REST console with gating, `_analyze` UI.
4. **P4 — templates + snapshots:** template CRUD, repos, snapshot create/restore.
5. **P5 — hardening:** rate limits, security headers/CSP, JSON/audit log polish, docker-compose stack, docs.

Each phase ships a working, testable increment.
