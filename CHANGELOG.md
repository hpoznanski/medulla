# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.8.0] - 2026-11-06

Release candidate for 1.0.0: no new features, everything here hardens what
1.0 will promise.

### Added

- Signed releases: the container image and the Helm chart are signed with
  cosign keyless signing (GitHub OIDC identity of the release workflow,
  recorded in the Rekor transparency log). The image also carries an SPDX
  SBOM and SLSA provenance (`mode=max`). Verification commands in the README;
  they need cosign v3 or later (v2 does not read the new bundle format and
  reports `no signatures found`).
- Support matrix tested in CI on every change: Elasticsearch 7.17, 8.19, 9.2
  and OpenSearch 2.19, 3.3, each through `scripts/compat.sh` — every page,
  index/alias/template/settings/routing writes, cache invalidation after each
  write, RBAC denials, and an unreachable cluster on the landing page.
  Releases are gated on it.
- `values.schema.json` for the Helm chart: a misspelt top-level key, an
  invalid `service.type` or `replicaCount: 0` fails at install time instead of
  being silently ignored.
- README: *Stability and compatibility* — what 1.0 freezes (config keys, RBAC
  atoms, URLs and their parameters, audit fields, image tags, chart values,
  supported versions) and the deprecation rule.
- README: *Revoking all sessions*, and the note that signing out cannot revoke
  a copied cookie.

### Fixed

- Elasticsearch 9 was shown as `unknown`: flavor detection only knew 7 and 8.
  Any Elasticsearch from 7 up is now `es<major>`.
- Cluster version was cached for the life of the process, so after a rolling
  upgrade the old version showed until Medulla restarted. It now follows the
  shared 5-second cache.
- README advised the zero-logout rotation after a suspected leak; that keeps
  the old key valid and so keeps a stolen cookie working. It now says to
  replace the key outright.

### Accessibility

- Critical red raised to `#e07a70` and badge tints lowered, so every text
  colour meets WCAG AA (4.5:1) on every surface it appears on; danger buttons
  use dark text (white on red failed).
- Health conveyed by colour alone now has screen-reader text (cluster cards,
  shard matrix index names); decorative sort arrows and layout icons are
  hidden from screen readers (`aria-sort` carries the state).
- The landing filter no longer takes focus on each auto-refresh.
- The node-drain and snapshot-delete confirmation inputs have labels.

## [0.7.0] - 2026-11-06

### Added

- Indices page: sortable columns (index, health, state, pri, rep, docs, size —
  numbers and sizes compared by value, so `9 < 10 < 100` and `900kb < 2gb`),
  a health summary that doubles as a `?health=` filter, a name filter with
  native `<datalist>` suggestions (capped at 1,000 names), and opt-in
  auto-refresh.
- `_cat` browser: sort on any column the endpoint returns, a row filter
  across all cells, and auto-refresh. Rows keep ES's order until a column is
  chosen; the sortable columns are allowlisted per response, so an unknown
  `?sort=` is ignored. The README's "sortable tables" is now true.
- Empty cells sort last in either direction on both pages.
- Injection tests extended to the indices and `_cat` pages.

### Changed

- The index list and `_cat` reads go through the shared 5-second cache;
  creating, deleting or changing an index through Medulla clears it, so the
  list after the action shows the result.

## [0.6.1] - 2026-11-06

### Changed

- Cluster reads behind the overview and landing page (health, nodes, shards,
  stats, routing) go through a shared 5-second cache per cluster.
  Concurrent viewers wait on one in-flight request instead of each querying
  ES, so auto-refresh across many operators costs ES at most one request per
  path per window per replica. Errors are cached too, so a dead cluster is not
  hammered. Any write made through Medulla clears the cache, so the page
  after a change shows it. A viewer leaving mid-load no longer cancels the
  fetch for the others.
- Allocation explains on the overview run up to 4 at a time instead of one
  after another: a sick cluster's overview waits at most two rounds of the ES
  timeout instead of eight.
- Response time on the landing page is the ES round trip, not time spent
  waiting on the cache.
- Large counts (docs, deleted docs) read as `12.3M`, with the exact figure on
  hover.
- Shard matrix: a copy being built (`INITIALIZING`, e.g. the target of a
  relocation) shows dashed in the warning colour instead of as a settled
  shard; only shards that are not `STARTED` take keyboard focus, so a large
  cluster is no longer hundreds of Tab stops.
- The Medulla logo links to the clusters page from every page.

### Security

- Weekly scheduled `govulncheck` run, independent of pushes: new standard
  library advisories surface without a code change, and Dependabot does not
  raise the `go` directive. A failed scheduled run notifies the owner.
- `govulncheck` pinned to v1.9.0 instead of `@latest`, consistent with the
  SHA-pinned Actions; the vulnerability database it queries is always current.
- Builder image pinned by digest (`golang:1.27-alpine@sha256:…`); Dependabot's
  docker ecosystem keeps it current.

## [0.6.0] - 2026-11-06

### Added

- Clusters landing page, cards or list: a toggle remembered in a preference
  cookie (`HttpOnly`, `SameSite=Lax`, `Secure` unless `insecure_cookie`; only
  `cards` or `list` are accepted). The list is a sortable table — health,
  name, version, nodes, indices, docs, shards, unassigned, relocating, store
  size, response time — with quick links to overview, indices and (when
  permitted) console.
- Problems first: clusters are ordered unreachable, red, yellow, green by
  default in both layouts.
- Health summary bar (`5 clusters · 2 unreachable · 3 yellow`), each count a
  filter link (`?health=`).
- Cluster name filter with native `<datalist>` suggestions while typing — no
  JavaScript. Only clusters the user's roles can view are ever suggested.
- Auto-refresh on the landing page (`off · 10s · 30s · 60s`), shared with the
  overview.
- Unreachable clusters are classified — `timeout`, `refused`, `dns`, `tls`,
  `auth` (ES answered 401/403), `http NNN`, `error` — with the full error a
  click away. Missing figures render as `—`, never `0`, and sort last in either
  direction.
- Response time per cluster.
- Security test suite: no script, event handler, `javascript:` URL or inline
  style in any template; CSP pinned; injection attempts on every list-page
  parameter are neither reflected nor turned into off-origin links or a meta
  refresh; tampered layout cookie ignored; hidden clusters never leak through
  cards, counts or suggestions; ES credentials in a cluster URL never appear in
  error text.

### Fixed

- Landing page showed `0 indices · 0 docs` and no version when
  `_cluster/stats` or `/` failed but health answered. It now shows `—` with a
  `partial` badge and the cause.

- CI no longer tags the container image `latest`. The v0.5.0 image was
  published with a `latest` tag because `docker/metadata-action` adds one to
  semver tags by default; the documented policy is version tags only.

## [0.5.0] - 2026-11-06

### Added

- Overview auto-refresh: `off · 10s · 30s · 60s`, opt-in per page via
  `?refresh=N` and a plain `<meta http-equiv="refresh">` — no JavaScript, CSP
  unchanged. Fixed intervals only; each refresh repeats every ES call behind
  the page, so there is no free-form value and no interval under 10s.
- Sortable Nodes table: click a column header (node, ip, heap, disk, cpu, load,
  shards) to sort server-side; metrics open worst-first. New `shards` column
  with the number of shards each node holds.
- Shard allocation is now an index × node matrix: one row per index, one column
  per node, unhealthy indices (unassigned primary, then replica) listed first,
  index names link to the index detail page. Sticky header and index column on
  wide clusters, and a case-insensitive index-name filter.

### Fixed

- A relocating shard is now counted on its source node. `_cat/shards` reports
  its node as `src -> ip id dst`, which matched no node, so a draining node's
  `N left` undercounted while shards were moving.

### Security

- Go toolchain raised from 1.26.6 to 1.26.10 in `go.mod` (CI builds and
  tests with it). Clears 13 standard-library advisories that `govulncheck`
  reported as reachable, among them `html/template` escaping fixes
  (GO-2026-6599, GO-2026-6600) on the page-rendering path and
  `crypto/x509` (GO-2026-6809). `govulncheck` now reports no reachable
  vulnerabilities. The release image already built with Go 1.27.
- `golang.org/x/crypto` v0.54.0 → v0.58.0, `github.com/go-ldap/ldap/v3`
  v3.4.14 → v3.4.15.
- Known and not applicable: GO-2026-5932 flags the `x/crypto` module for its
  unmaintained `openpgp` package. Medulla imports only `x/crypto/bcrypt`, and
  the fix is not in a tagged release yet, so the module stays on a tagged
  version instead of an untagged commit.

## [0.4.0] - 2026-08-14

### Added

- Shard routing controls on the overview: drain a single node by excluding it
  from allocation (type-the-name confirmation, since it relocates every shard
  off that node), and cluster-wide `allocation.enable` / `rebalance.enable`
  selectors. A standing banner warns while any routing restriction is in place.
  All gated on `cluster:write`.
- Node allocation status per node: `draining · N left` while shards remain,
  `drained — safe to stop` once the node holds nothing.
- Index settings editor on the index detail page: every effective setting with
  ES defaults shown alongside explicitly set values, and edit/reset for any
  dynamic setting. Gated on `index:write`.
- Cluster totals on the overview: indices, docs, deleted docs, store size.
- REST console: request and response JSON pretty-printed, and a replayable
  history of recent requests in an HMAC-signed cookie — survives restarts and
  works across replicas without shared state. Bodies over 800 bytes are stored
  without the body rather than truncated into invalid JSON.
- README: Operations runbooks (rolling restart, node drain, unassigned shards,
  secret rotation, LDAP outage, audit log, upgrades), a Troubleshooting table,
  a Limits and tunables table, and a "What Medulla is not" section.
- README: full RBAC permission table, including the note that write atoms do
  not imply `view`.
- README: container image section — coordinates, multi-arch support
  (`linux/amd64`, `linux/arm64`), tagging scheme, and the operational
  consequences of the `FROM scratch` image (no shell, uid 65534).
- README: CI, release, license and Go version badges.
- CI: `e2e` job running `scripts/e2e.sh` against real Elasticsearch and
  OpenSearch from docker-compose. The script already existed but nothing ran
  it; unit tests use a fake ES, so this is the only coverage of flavor
  detection, real `_cat` shapes and the RBAC-gated write paths.
- CI: `fuzz` job covering the untrusted-input boundaries — console paths
  reaching the cluster, secrets interpolated into config text, and session
  cookies. Seven targets at 30s each, Go's native fuzzing, no new dependency.
- e2e coverage for the index settings editor, shard routing controls, node
  drain confirmation, and console pretty-printing and history (93 checks).

### Changed

- Go toolchain to 1.26.6, clearing six standard-library advisories reported by
  govulncheck (`net/url`, `html/template`, `crypto/tls`, `net/http`,
  `encoding/asn1`, `x/net/idna`). No Medulla code changed.
- Routing state is read with `filter_path`, trimming `_cluster/settings` from
  ~34 kB to ~150 bytes on every overview render.
- Helm chart 0.4.0: app version bump only, no template changes.
- CI: GitHub Actions pinned to commit SHAs rather than floating tags. These
  workflows hold `packages: write`, and a tag can be moved under you.
  Dependabot's github-actions ecosystem keeps the pins current.
- CI: `image` and `chart` now require `fuzz` and `e2e` in addition to `test`.

### Fixed

- Shard grid hover tooltips were clipped by an `overflow-x` container, hiding
  per-shard details on the top row.
- Overview stat tiles used `auto-fit`, so a lone tile on the last row stretched
  to full width instead of aligning with the grid.
- The allocation column no longer renders for users without `cluster:write`
  when no node is excluded, instead of showing an empty column.
- A cluster whose settings cannot be read is reported as unknown rather than
  as restricted, and its routing controls are hidden instead of submitting a
  guessed current value.
- Chart README pinned a stale `--version 0.1.0` in its install example.

## [0.3.0] - 2026-07-30

### Added

- LDAP `user_to_role`: map a single LDAP user directly to a role without
  requiring a directory group; unioned with `group_to_role` results.
- `SECURITY.md` with private vulnerability reporting policy.
- README: full configuration reference (clusters/auth/TLS, roles, LDAP,
  local users, sessions, trusted proxies).

### Changed

- Login rate limiting is now keyed on username instead of client IP: behavior
  is identical with or without reverse proxies and cannot be bypassed by
  spoofing `X-Forwarded-For`. `trusted_proxies` now only affects the client IP
  recorded in audit logs.
- Helm chart 0.3.0: app version bump only, no template changes.

## [0.2.0] - 2026-07-27

### Added

- CI: govulncheck vulnerability scan step
- CI: helm lint and chart render tests
- Helm chart render test script (`deploy/helm/medulla/tests/render_test.sh`)
- End-to-end smoke test script (`scripts/e2e.sh`)
- Helm chart README with configuration reference

## [0.1.0] - 2026-07-27

### Added

- Initial release: single-binary web admin tool for Elasticsearch 7/8 and OpenSearch
- Cluster overview with shard grid and per-shard allocation-explain panel
- Indices: list, create, detail, close/open/forcemerge/refresh, type-name-to-confirm delete
- Aliases and index templates management
- Snapshots: repositories, create, restore, delete
- REST console with server-enforced method restrictions (`rest:get` = GET/HEAD only)
- Cat API browser (16 endpoints)
- `_analyze` tool
- Multi-cluster support with landing page cards and cluster switcher
- Auth: LDAP and local users, stateless HMAC-cookie sessions with key rotation
- RBAC: per-cluster permission atoms, default roles admin/operator/developer/viewer
- JSON audit logging via slog (no request/response payloads)
- Server-side rendering only (html/template + htmx), CSP `default-src 'none'`, dark theme
- Deploy: scratch Docker image, Helm chart, docker-compose dev stack (ES 8 two-node + OpenSearch 2)
- CI: gofmt, go vet, race tests, build; image and chart publish to GHCR on version tags
