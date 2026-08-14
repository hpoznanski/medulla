<p align="center">
  <img src="assets/logo.svg" width="130" alt="Medulla logo">
</p>

<p align="center">
  <a href="https://github.com/hpoznanski/medulla/actions/workflows/ci.yml"><img src="https://github.com/hpoznanski/medulla/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <a href="https://github.com/hpoznanski/medulla/releases"><img src="https://img.shields.io/github/v/release/hpoznanski/medulla" alt="release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="license"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/hpoznanski/medulla" alt="go version"></a>
</p>

# ◉ Medulla

Web admin UI for **Elasticsearch** and **OpenSearch** clusters: **multi-cluster management, built-in RBAC, LDAP auth, audit logging** — in a single ~10 MB static binary.

Medulla is the brain stem: small, always on, controls the vital functions.

## Features

- **Clusters landing page** — every cluster at a glance: health, version/flavor, nodes, indices, docs, unassigned shards. Cluster switcher on every page.
- **Overview** — cluster totals (indices, docs, store size), node stats (heap/disk/CPU), a visual shard allocation grid with per-shard hover details, and an *allocation explain* panel that shows the actual root cause for every unassigned shard (deciders parsed, grouped by cause).
- **Shard routing** — drain a single node with one click (type-name-to-confirm), showing live progress until it reports *drained — safe to stop*; cluster-wide `allocation.enable` / `rebalance.enable` switches, with a standing warning while any restriction is in place.
- **Index management** — create, delete (type-name-to-confirm), open/close, refresh, flush, force-merge; settings/mappings/aliases detail view; edit or reset any dynamic index setting, with ES defaults shown alongside what is explicitly set.
- **Aliases** — list, add, remove.
- **Index templates** — list, inspect, create/update via JSON editor, delete.
- **Snapshots** — repository management, create/restore/delete snapshots.
- **Cluster settings** — persistent/transient/defaults browser, edit with reset-to-default.
- **Analyze** — `_analyze` playground for analyzers and tokenizers.
- **Cat browser** — 16 allowlisted `_cat` endpoints as sortable tables, column order preserved. Anything outside the list is reachable from the REST console.
- **REST console** — free-form requests, permission-gated: `rest:get` users get GET/HEAD only, enforced server-side. Request and response JSON are pretty-printed, and recent requests are replayable from a signed-cookie history that survives restarts and works across replicas.
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
- **No ES client library.** Medulla is a smart proxy; a ~200-line `net/http` transport core carries every admin API on ES7/ES8/OpenSearch — no version-split client modules, no churn when ES 9 lands.
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

Roles are named sets of permission atoms scoped to a cluster list (`"*"` for all). LDAP groups map to roles; local users get roles directly. UI controls render only when permitted, and every route enforces the same check server-side.

| Atom | Grants | Covers |
|---|---|---|
| `view` | Read-only access to every page | Overview (health, nodes, shard grid, allocation explain), index list and detail, effective index settings, aliases, templates, snapshots, cluster settings, `_cat` browser, `_analyze` |
| `index:write` | Index lifecycle and per-index settings | Create, delete (type-name-to-confirm), open, close, refresh, flush, force-merge; edit or reset any dynamic index setting |
| `alias:write` | Alias changes | Add and remove aliases |
| `template:write` | Index template changes | Create, update, delete index templates |
| `snapshot:write` | Snapshot and repository operations | Register repositories; create, restore, delete snapshots |
| `cluster:write` | Cluster-level routing and settings | Edit or reset persistent cluster settings; `allocation.enable` / `rebalance.enable`; **exclude a node from allocation, which drains every shard off it** |
| `rest:get` | REST console, read-only | GET and HEAD against any ES endpoint |
| `rest:full` | REST console, unrestricted | Any method against any ES endpoint. Implies `rest:get` |
| `admin` | Everything | Implies every atom above |

Only two implications exist: `admin` implies everything, and `rest:full` implies `rest:get`. Every other atom is independent.

**Pair write atoms with `view`.** They do not imply it. A role with `index:write` but no `view` cannot see the cluster in the UI at all — every page returns 403 — yet a direct `POST` still deletes an index. Grant `view` alongside any write atom so what the UI shows matches what the role can actually do.

**`rest:get` is read-admin, not read-only-ish.** It reads the *entire* ES API on that cluster, including endpoints Medulla has no page for. Grant it only to people you would trust with a shell against the cluster.

**`cluster:write` moves data.** Excluding a node is one click plus a typed confirmation, and it relocates every shard off that node — potentially terabytes of network I/O. The overview shows a persistent warning while any routing restriction is in place, because leaving `allocation.enable: none` set after a rolling restart stops the cluster healing itself.

```yaml
roles:
  admin:     {clusters: ["*"],     permissions: [admin]}
  operator:  {clusters: ["*"],     permissions: [view, index:write, alias:write, template:write, snapshot:write, cluster:write, rest:full]}
  developer: {clusters: [staging], permissions: [view, rest:get]}
  viewer:    {clusters: ["*"],     permissions: [view]}
  # index janitor on one cluster, no cluster-level or console access
  indexer:   {clusters: [staging], permissions: [view, index:write]}
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
helm install medulla oci://ghcr.io/hpoznanski/charts/medulla --version 0.4.0 -f my-values.yaml
```

## Operations

### Rolling restart of a node

Stopping a node makes Elasticsearch start rebuilding its shards elsewhere. Disable allocation first so the data stays put until the node comes back.

1. Overview → **Cluster routing** → set `allocation.enable` to `primaries`, Apply. A red banner appears and stays until you undo this.
2. Restart the node. The cluster goes yellow — expected, because replicas are not being reallocated.
3. Set `allocation.enable` back to `all`. Watch *Relocating* on the stat row drop to 0 and health return to green.

Do not skip step 3. While allocation is restricted the cluster cannot heal itself from an unrelated node failure, which is why the banner does not go away on its own.

### Draining and decommissioning a node

1. Overview → Nodes → type the node's name into its **exclude** box and confirm.
2. The node shows `draining · N left` and counts down as shards relocate.
3. When it reads **`drained — safe to stop`** the node holds nothing and can be shut down.
4. Changed your mind? **re-include** puts it straight back; shards flow back on their own.

**A drain cannot finish if the cluster has fewer nodes than `replicas + 1`.** Elasticsearch will not place a primary and its replica on the same node, so with two nodes and `number_of_replicas: 1` there is nowhere for the excluded node's shards to go. The exclusion still applies, and the badge sits on `draining · N left` indefinitely. Confirm with the REST console:

```
POST /_cluster/allocation/explain
{"index":"my-index","shard":0,"primary":true}
```

`can_remain_on_current_node: no` with a `same_shard` decider on every target means exactly this. Add a node or lower the replica count.

### Unassigned shards

The overview's *Unassigned shards — why?* panel runs `_cluster/allocation/explain` per shard, parses the deciders, and groups shards sharing a root cause — the actual reason, not the boilerplate `allocate_explanation`.

It explains **at most 8 shards per page load**; beyond that it prints "…and N more unassigned shards not analyzed (limit)". The cap is fixed, and it exists because the calls are sequential and each one can be slow on an unhealthy cluster. Where a whole index is unassigned the grouping usually means one explanation covers all of it anyway; for a wider sweep, use the console.

### Rotating the session secret

`session.secret` takes a comma-separated list. Sign with the first, verify against all — so nobody is logged out.

1. Set `secret: "${NEW_SECRET},${OLD_SECRET}"` and roll out. New sessions use the new key; existing cookies still verify against the old one.
2. Wait longer than `session.ttl` (default 12h) so every old cookie has expired.
3. Drop the old key: `secret: "${NEW_SECRET}"`.

Rotate this way after any suspected leak. Removing the old key immediately also works and simply logs everyone out.

### When LDAP is down

Login tries LDAP first, then falls back to `local_users`. An LDAP outage does not lock you out **provided a local account exists** — so always configure a breakglass local user with a bcrypt password, kept in a secret manager and not used day to day. Failed LDAP dials are logged at `ERROR` with `ldap login failed`; the user just sees "Invalid username or password."

An LDAP user who authenticates successfully but maps to no role is denied and sees the same message. Check `group_to_role` and `user_to_role` against the DNs the directory actually returns in `memberOf`.

### Reading the audit log

JSON on stdout, one object per event, `"type":"audit"`. Ship it with whatever tails container logs.

```json
{"time":"2026-08-14T09:12:44Z","level":"INFO","msg":"audit","type":"audit","event":"login",
 "outcome":"success","user":"jdoe","roles":["operator"],"ip":"10.42.3.9"}
{"time":"2026-08-14T09:13:02Z","level":"INFO","msg":"audit","type":"audit","user":"jdoe",
 "roles":["operator"],"method":"POST","path":"/c/prod/indices/logs-1/delete","outcome":"success",
 "status":303,"ip":"10.42.3.9"}
```

Logged: every login attempt (`outcome`: `success`, `bad_credentials`, `rate_limited`, `error`), every permission denial, every state-changing request, and every REST console call (with `es_method`, `es_path`, `es_status`). **Never logged: request or response bodies.** So you get "who deleted what, when, from where" but not document contents.

`ip` is the direct peer unless `trusted_proxies` covers it — behind an ingress without that set, every record shows the proxy's address.

### Upgrading

Stateless, so an upgrade is a rolling image bump: no migrations, no schema, nothing persisted but the config file. Sessions survive as long as `session.secret` is unchanged. Roll back by pinning the previous tag.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `no clusters visible for your roles` after login | The role has no `view` on any cluster. Write atoms do not imply `view`. | Add `view` to the role — see [RBAC](#rbac) |
| Cluster page 404s | Cluster name in the URL is not in `clusters` | Names are case-sensitive and must match `config.yaml` exactly |
| `forbidden` on a button that rendered | Role lost the permission between page load and submit, or the URL was hand-built | Expected — the server always re-checks |
| `too many attempts, retry later` | 5 failed logins for that username; refills at 10/min | Wait ~6s per retry, ~30s for the full burst. Per username, so changing IP does not clear it |
| `cross-origin request rejected` | `Origin` header does not match `Host` | Fix the proxy's `Host`/`X-Forwarded-Host` rather than disabling the check |
| `rest:full permission required for write requests` | `rest:get` allows GET and HEAD only | Grant `rest:full` if the user should write |
| Everyone logged out after a restart | No `session.secret`, so an ephemeral key was generated | Set `session.secret`. Startup logs `session secret not set` at WARN |
| Login works on one replica, not another | Replicas have different `session.secret` values | Share one secret across all replicas |
| Cluster card shows an error on the landing page | ES unreachable, bad credentials, or TLS failure | The card shows the raw error. Certificate problems need `tls.ca_file` |
| Overview slow or truncated on a sick cluster | Each ES call allows 30s; the HTTP response deadline is 60s | Fix the cluster; Medulla surfaces what returns in time |
| Audit log shows the ingress IP for everyone | `trusted_proxies` unset | Add your ingress CIDRs |
| Node stuck on `draining · N left` | Fewer nodes than `replicas + 1` | See [Draining a node](#draining-and-decommissioning-a-node) |
| Disk % identical across nodes | Nodes share one filesystem (typical in Docker) | Not a bug — that column is filesystem usage, not shard data |

## Limits and tunables

Fixed unless the table says otherwise. All are compile-time constants — deliberate, since each one exists to bound a failure mode rather than to be tuned per install.

| Behavior | Value | Configurable |
|---|---|---|
| Login rate limit | 5 burst, refills 10/min, per username | No |
| Session TTL | 12h default | Yes — `session.ttl` |
| ES request timeout | 30s per request | No |
| HTTP server timeouts | 5s header, 30s read, 60s write, 120s idle | No |
| Graceful shutdown | 10s | No |
| Landing page cluster probe | 5s for all clusters, queried in parallel | No |
| Allocation explains per overview | 8 shards | No |
| ES response size read | 64 MB | No |
| REST console history | 20 entries; bodies over 800 bytes stored without the body | No |
| `_cat` endpoints exposed | 16, allowlisted | No |

`env: production` requires `session.secret` and rejects any key shorter than 32 bytes. When `env` is absent from the config file, Medulla falls back to the **`MEDULLA_ENV`** environment variable — convenient for Kubernetes, where it can come from the Deployment rather than the ConfigMap.

Medulla exposes `/healthz` and nothing else. There is no `/metrics` endpoint; observability comes from the JSON logs.

## What Medulla is not

An administration tool, not a data tool. It has no document search or CRUD, no query builder, no dashboards or visualizations, no ILM/rollover management, no user management for Elasticsearch's own security realm, and no alerting. For those, use Kibana or OpenSearch Dashboards — Medulla is what you reach for when you need to see cluster state and change it, not to explore data.

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
