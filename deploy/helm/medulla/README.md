# Medulla Helm chart

Deploys [Medulla](../../../README.md) — web admin UI for Elasticsearch/OpenSearch with RBAC.

## What you need to start

1. A Kubernetes cluster and Helm 3.8+.
2. Reachable ES/OpenSearch endpoints (direct or behind a reverse proxy).
3. One Kubernetes Secret with your credentials (see [How secrets work](#how-secrets-work)).

Minimal working `my-values.yaml`:

```yaml
existingSecret: medulla-secrets      # keys: SESSION_SECRET, ADMIN_PASSWORD, ES_PROD_PASSWORD

config:
  env: production
  clusters:
    - name: prod
      url: https://es-prod.internal:9200
      auth: {type: basic, username: medulla, password: "${ES_PROD_PASSWORD}"}
  roles:
    admin: {clusters: ["*"], permissions: [admin]}
  local_users:
    - {name: admin, password: "${ADMIN_PASSWORD}", roles: [admin]}
  session:
    secret: "${SESSION_SECRET}"
```

Install:

```sh
# from the published OCI registry
helm install medulla oci://ghcr.io/hpoznanski/charts/medulla --version 0.1.0 -f my-values.yaml

# or from a repo checkout
helm install medulla deploy/helm/medulla -f my-values.yaml

# no ingress yet? try it via port-forward
kubectl port-forward svc/medulla 8080:8080   # → http://localhost:8080
```

For a full production example (4 clusters, LDAP, all roles, ingress with TLS) see [`values-example.yaml`](values-example.yaml) — commit-safe, no secret material.

## Adding clusters

Each entry under `config.clusters` is one cluster in the UI. Add an entry, add its credential key to the Secret, `helm upgrade` — pods roll automatically (config checksum).

```yaml
config:
  clusters:
    # no auth (dev, or network-trusted)
    - name: sandbox
      url: http://es-sandbox.internal:9200

    # basic auth — native ES security or an authenticating reverse proxy
    # in front; identical config either way
    - name: prod-eu
      url: https://es-eu-proxy.internal:9443
      auth: {type: basic, username: medulla, password: "${PROXY_EU_PASSWORD}"}

    # API key (ES)
    - name: prod-us
      url: https://es-us.internal:9200
      auth: {type: api_key, api_key: "${ES_US_API_KEY}"}

    # private CA for the endpoint's TLS cert (ca_file is optional —
    # omit it when the cert chains to a system-trusted CA)
    - name: prod-asia
      url: https://es-asia.internal:9200
      auth: {type: basic, username: medulla, password: "${ES_ASIA_PASSWORD}"}
      tls: {ca_file: /etc/medulla/ca/private-ca.pem}
```

The `ca_file` case needs the bundle mounted:

```yaml
extraVolumes:
  - name: es-ca
    configMap: {name: es-ca-bundle}     # kubectl create configmap es-ca-bundle --from-file=private-ca.pem
extraVolumeMounts:
  - name: es-ca
    mountPath: /etc/medulla/ca
    readOnly: true
```

Then scope who sees the new cluster — roles list clusters explicitly (or `"*"` for all):

```yaml
config:
  roles:
    admin:    {clusters: ["*"],                permissions: [admin]}
    eu-team:  {clusters: [prod-eu, sandbox],   permissions: [view, index:write, rest:get]}
```

ES version/flavor (ES 7/8, OpenSearch) is detected automatically per cluster — nothing to configure.

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
| `config.trusted_proxies` | `[]` | set to your ingress pod CIDRs — enables real client IPs in audit logs (login rate limiting is per username, unaffected) |
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
