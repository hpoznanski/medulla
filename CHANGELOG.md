# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
