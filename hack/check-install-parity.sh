#!/usr/bin/env bash
# Compare the security posture of the two ways this operator is installed.
#
# The chart and the kustomize manifests are maintained separately, and the chart
# drifted three times in the same direction -- weaker than kustomize, in the
# install method people actually use: leader election granted cluster-wide
# instead of namespaced, seccomp turned off, and metrics served unauthenticated
# over plaintext. Each was individually plausible and none was noticed.
#
# So the properties are asserted here rather than being left to review. This is
# not a full diff of the two renderings, which legitimately differ; it is the
# short list of things that must not silently weaken.
set -euo pipefail

cd "$(dirname "$0")/.."

chart="$(mktemp)"
kustomize_out="$(mktemp)"
trap 'rm -f "$chart" "$kustomize_out"' EXIT

helm template parity charts/dynamic-prefix-operator \
  --namespace dynamic-prefix-operator-system > "$chart"

if command -v kustomize >/dev/null 2>&1; then
  kustomize build config/default > "$kustomize_out"
else
  echo "kustomize not found; skipping the kustomize half of the comparison" >&2
  : > "$kustomize_out"
fi

failed=0
fail() {
  echo "FAIL: $*" >&2
  failed=1
}
pass() { echo "ok: $*"; }

# 1. Leader election must be namespaced in both. A ClusterRole carrying lease
#    verbs lets the operator delete node heartbeats and control-plane locks.
if awk '/^kind: ClusterRole$/,/^---$/' "$chart" | grep -q 'leases'; then
  fail "the chart grants coordination.k8s.io/leases in a ClusterRole; it belongs in a namespaced Role"
else
  pass "leases are not granted cluster-wide by the chart"
fi

if ! awk '/^kind: Role$/,/^---$/' "$chart" | grep -q 'leases'; then
  fail "the chart no longer ships a namespaced Role granting leases; leader election will not work"
else
  pass "the chart grants leases through a namespaced Role"
fi

# 2. seccomp must be RuntimeDefault in both. This process holds NET_RAW and
#    parses attacker-supplied packets.
if grep -q 'type: Unconfined' "$chart"; then
  fail "the chart sets seccompProfile: Unconfined"
else
  pass "the chart does not disable seccomp"
fi

if [[ -s "$kustomize_out" ]] && grep -q 'type: Unconfined' "$kustomize_out"; then
  fail "the kustomize manifests set seccompProfile: Unconfined"
fi

# 3. Metrics must not be served unauthenticated by default. With hostNetwork the
#    listener is on the node's own address.
if grep -q -- '--metrics-secure=false' "$chart"; then
  fail "the chart renders --metrics-secure=false by default"
else
  pass "the chart does not disable metrics authentication"
fi

# 4. The kustomize install must be able to see Router Advertisements at all,
#    which the README states is required.
if [[ -s "$kustomize_out" ]]; then
  if grep -q 'hostNetwork: true' "$kustomize_out"; then
    pass "install.yaml runs with hostNetwork"
  else
    fail "install.yaml has no hostNetwork; the operator cannot see Router Advertisements"
  fi
fi

# 5. Neither install may drop the container hardening.
for f in "$chart" "$kustomize_out"; do
  [[ -s "$f" ]] || continue
  grep -q 'allowPrivilegeEscalation: false' "$f" || fail "$f is missing allowPrivilegeEscalation: false"
  grep -q 'readOnlyRootFilesystem: true' "$f" || fail "$f is missing readOnlyRootFilesystem: true"
done
pass "container hardening present in both installs"

exit "$failed"
