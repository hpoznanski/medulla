#!/usr/bin/env bash
# Render tests for the medulla Helm chart. Requires only helm + coreutils.
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)"
FAILED=0

fail() { echo "FAIL: $1"; FAILED=1; }
pass() { echo "ok:   $1"; }

render() { helm template test "$CHART" "$@" 2>/dev/null; }

BASE=(--set existingSecret=medulla-secrets
      --set 'config.clusters[0].name=prod'
      --set 'config.clusters[0].url=https://es:9200'
      --set 'config.clusters[0].auth.type=basic'
      --set 'config.clusters[0].auth.username=medulla'
      --set 'config.clusters[0].auth.password=${ES_PROD_PASSWORD}')

OUT="$(render "${BASE[@]}")"
DEPLOY="$(render "${BASE[@]}" --show-only templates/deployment.yaml)"

# 1. Secrets never land in rendered manifests: the ${VAR} placeholder survives
#    verbatim in the ConfigMap and nothing secret-like reaches the Deployment.
echo "$OUT" | grep -q '\${ES_PROD_PASSWORD}' \
  && pass "placeholder stays verbatim in ConfigMap" || fail "placeholder missing or expanded"
echo "$DEPLOY" | grep -qi 'password' \
  && fail "password material in Deployment spec" || pass "no secret values in Deployment spec"

# 2. envFrom wired to the named Secret.
echo "$DEPLOY" | grep -A2 'envFrom:' | grep -q 'name: medulla-secrets' \
  && pass "envFrom -> secretRef medulla-secrets" || fail "envFrom secretRef wrong"

# 3. Without existingSecret: no envFrom at all.
render --show-only templates/deployment.yaml | grep -q 'envFrom' \
  && fail "envFrom rendered without existingSecret" || pass "no envFrom without existingSecret"

# 4. Exactly one inline env var, and it is MEDULLA_ENV (non-sensitive).
ENV_NAMES="$(echo "$DEPLOY" | awk '/^ +env:$/,/^ +(envFrom|volumeMounts):/' | grep -c '\- name:')"
echo "$DEPLOY" | grep -q 'name: MEDULLA_ENV' && [ "$ENV_NAMES" -eq 1 ] \
  && pass "inline env is MEDULLA_ENV only" || fail "unexpected inline env (count=$ENV_NAMES)"

# 5. Security posture.
for want in 'automountServiceAccountToken: false' 'runAsNonRoot: true' \
            'readOnlyRootFilesystem: true' 'allowPrivilegeEscalation: false'; do
  echo "$DEPLOY" | grep -q "$want" || fail "security regression: missing '$want'"
done
echo "$DEPLOY" | grep -A1 'capabilities:' | grep -q 'ALL' \
  && pass "security context hardened" || fail "capabilities not dropped"

# 6. Probes on /healthz (liveness + readiness).
[ "$(echo "$DEPLOY" | grep -c 'path: /healthz')" -eq 2 ] \
  && pass "liveness+readiness on /healthz" || fail "probes missing"

# 7. Config change rolls pods: checksum annotation differs.
SUM1="$(render "${BASE[@]}" | grep 'checksum/config')"
SUM2="$(render "${BASE[@]}" --set 'config.clusters[0].name=other' | grep 'checksum/config')"
[ "$SUM1" != "$SUM2" ] && pass "checksum/config changes with config" || fail "checksum static"

# 8. ConfigMap carries the cluster config.
echo "$OUT" | grep -q 'url: https://es:9200' \
  && pass "ConfigMap contains cluster config" || fail "cluster config missing"

# 9. Ingress off by default, renders host when enabled.
render | grep -q 'kind: Ingress' \
  && fail "ingress rendered while disabled" || pass "ingress disabled by default"
render "${BASE[@]}" --set ingress.enabled=true --set ingress.host=m.example.com \
  | grep -q 'host: m.example.com' && pass "ingress renders host" || fail "ingress host missing"

# 10. extraVolumes/extraVolumeMounts wire through to the pod.
EXTRA="$(render "${BASE[@]}" \
  --set 'extraVolumes[0].name=es-ca' --set 'extraVolumes[0].configMap.name=es-ca-bundle' \
  --set 'extraVolumeMounts[0].name=es-ca' --set 'extraVolumeMounts[0].mountPath=/etc/medulla/ca' \
  --show-only templates/deployment.yaml)"
echo "$EXTRA" | grep -q 'name: es-ca-bundle' && echo "$EXTRA" | grep -q 'mountPath: /etc/medulla/ca' \
  && pass "extraVolumes/mounts wired" || fail "extraVolumes broken"

# 11. PDB + zone spread present by default.
echo "$OUT" | grep -q 'kind: PodDisruptionBudget' && pass "PDB rendered" || fail "PDB missing"
echo "$OUT" | grep -q 'topology.kubernetes.io/zone' && pass "zone spread rendered" || fail "zone spread missing"

# 12. The shipped example values render.
helm template test "$CHART" -f "$CHART/values-example.yaml" >/dev/null 2>&1 \
  && pass "values-example.yaml renders" || fail "values-example.yaml broken"

[ "$FAILED" -eq 0 ] && echo "ALL PASS" || exit 1
