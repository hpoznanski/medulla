# Medulla Helm chart

Deploys [Medulla](../../../README.md) — web admin UI for Elasticsearch/OpenSearch with RBAC.

## Install

```sh
# from the published OCI registry
helm install medulla oci://ghcr.io/hpoznanski/charts/medulla --version 0.1.0 -f my-values.yaml

# or from a repo checkout
helm install medulla deploy/helm/medulla -f my-values.yaml
```

Start from [`values-example.yaml`](values-example.yaml) — a complete, commit-safe production example (4 clusters behind authenticating proxies, LDAP, roles, break-glass admin).

## How secrets work

The chart is built so that **no secret material ever passes through Helm** — not in values files, not in rendered manifests, not in `helm get values`. The flow:

```
your secret manager  ──deploy pipeline──►  Kubernetes Secret  ──envFrom──►  container env
                                                                                   │
values.yaml config: password: "${ES_PROD_PASSWORD}"  ──ConfigMap──►  medulla resolves ${VAR}
                                                                     at startup, in memory
```

1. Create a Secret (outside the chart) whose keys are the env var names your config references:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata: {name: medulla-secrets}
   stringData:
     SESSION_SECRET: "<32+ random chars>"
     LDAP_BIND_PASSWORD: "..."
     ES_PROD_PASSWORD: "..."
     ADMIN_PASSWORD: "..."
   ```

2. Point the chart at it: `existingSecret: medulla-secrets`.
3. In `config`, reference values as `${ES_PROD_PASSWORD}`. The ConfigMap holds only these placeholders — safe to commit, safe to `kubectl describe`.

Session key rotation without logouts: set `SESSION_SECRET: "newkey,oldkey"`, roll out, later drop the old key.

### Why environment variables and not mounted Secret files?

Both **are** Kubernetes Secrets — the question is only the delivery into the process: `envFrom` vs a volume mount. Env vars have a bad reputation from environments where they leak: `/proc/<pid>/environ` readable by co-located processes, `kubectl exec env`, inheritance by child processes, frameworks dumping the environment into error reports. None of those paths exist here:

- The image is `FROM scratch`: no shell, no `env` binary, nothing to `kubectl exec` — exec has no executable to run.
- Medulla is a single static process that spawns no children, and there are no third-party frameworks that could dump the environment.
- Values are never inlined in manifests (`envFrom` a Secret), so `kubectl describe pod` shows only the reference.
- Any attacker who could read `/proc/self/environ` already has code execution inside the container — at which point mounted files are equally readable. Env vs file is not a security boundary against that attacker.

The real boundaries are elsewhere: Kubernetes RBAC on `get secrets` in the namespace, etcd encryption-at-rest, and least-privilege ES service accounts. Focus hardening there.

If you still prefer file delivery, it is fully supported: mount the Secret as a volume and reference values as `${file:/etc/medulla/secrets/es-password}` — Medulla resolves both forms identically. Choose per value; no code or chart changes needed beyond the mount (`extraVolumes`/`extraVolumeMounts`).

## Key values

| Value | Default | Notes |
|---|---|---|
| `replicaCount` | `2` | stateless; any count works, no sticky sessions |
| `image.repository` / `image.tag` | `ghcr.io/hpoznanski/medulla` / chart appVersion | |
| `existingSecret` | `""` | Secret name providing env vars — **required in practice** |
| `config` | minimal skeleton | the entire Medulla config.yaml, rendered verbatim into a ConfigMap |
| `config.trusted_proxies` | `[]` | set to your ingress pod CIDRs — enables real client IPs for login rate limiting and audit |
| `config.session.secret` | `${SESSION_SECRET}` | ≥32 chars; comma-separated list rotates keys |
| `ingress.*` | disabled | className, host, TLS |
| `extraVolumes` / `extraVolumeMounts` | `[]` | e.g. private CA bundle for `tls.ca_file` |
| `topologySpreadConstraints` | zone spread | replicas across zones by default |
| `podDisruptionBudget` | `minAvailable: 1` | |

## Operational notes

- **Config changes roll pods automatically** — the deployment carries a `checksum/config` annotation.
- **Probes** — liveness and readiness both hit `/healthz`. Readiness intentionally does not check ES: clusters being down must not take the UI out of rotation.
- **Security posture** — `runAsNonRoot` (65534), `readOnlyRootFilesystem`, all capabilities dropped, seccomp `RuntimeDefault`, no service-account token mounted, scratch image with no shell.
- **Custom CA** — set `tls.ca_file` on a cluster and mount the bundle via `extraVolumes`; omit it entirely for system-trusted certs (it is optional).
- **Logs** — JSON on stdout; audit records have `"type":"audit"`. Point your log pipeline at them; Medulla stores nothing.
