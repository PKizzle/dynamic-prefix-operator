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

# The checks below that hold for both installs iterate over the two renderings,
# so name them rather than reporting a temporary path back to the reader.
name_of() {
  case "$1" in
    "$chart") echo "the chart" ;;
    "$kustomize_out") echo "install.yaml" ;;
    *) echo "$1" ;;
  esac
}

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

# 4. Both installs must be able to see Router Advertisements at all, which the
#    README states is required. The chart shipped without it for long enough
#    that its own DHCPv6-PD example could not work.
for f in "$chart" "$kustomize_out"; do
  [[ -s "$f" ]] || continue
  if grep -q 'hostNetwork: true' "$f"; then
    pass "$(name_of "$f") runs with hostNetwork"
  else
    fail "$(name_of "$f") has no hostNetwork; the operator cannot see Router Advertisements or source DHCPv6 from the uplink"
  fi
done

# 5. Neither install may drop the container hardening.
for f in "$chart" "$kustomize_out"; do
  [[ -s "$f" ]] || continue
  grep -q 'allowPrivilegeEscalation: false' "$f" || fail "$(name_of "$f") is missing allowPrivilegeEscalation: false"
  grep -q 'readOnlyRootFilesystem: true' "$f" || fail "$(name_of "$f") is missing readOnlyRootFilesystem: true"
done
pass "container hardening present in both installs"

# 6. Both capabilities are required, for different acquisition methods, and
#    `drop: ALL` means neither survives being left off the add list -- including
#    for root. Dropping NET_BIND_SERVICE fails the DHCPv6 client's UDP 546 bind
#    with EACCES, which surfaces only in the pod log.
for f in "$chart" "$kustomize_out"; do
  [[ -s "$f" ]] || continue
  grep -q 'NET_RAW' "$f" || fail "$(name_of "$f") does not add NET_RAW; Router Advertisement monitoring cannot open its socket"
  grep -q 'NET_BIND_SERVICE' "$f" || fail "$(name_of "$f") does not add NET_BIND_SERVICE; the DHCPv6-PD client cannot bind UDP 546"
done
pass "both installs grant NET_RAW and NET_BIND_SERVICE"

# 7. ConfigMap access is for the kube-vip backend alone, and stays namespaced
#    and opt-in. In a ClusterRole it would be write access to every ConfigMap in
#    the cluster, for every install, including the ones not using kube-vip.
if awk '/^kind: ClusterRole$/,/^---$/' "$chart" | grep -q 'configmaps'; then
  fail "the chart grants configmaps in a ClusterRole; the kube-vip backend uses a namespaced Role"
else
  pass "configmaps are not granted cluster-wide by the chart"
fi

kubevip_chart="$(mktemp)"
trap 'rm -f "$chart" "$kustomize_out" "$kubevip_chart"' EXIT
helm template parity charts/dynamic-prefix-operator \
  --namespace dynamic-prefix-operator-system \
  --set kubevip.enabled=true > "$kubevip_chart"

if awk '/^kind: Role$/,/^---$/' "$kubevip_chart" | grep -q 'configmaps'; then
  pass "enabling kube-vip renders a namespaced Role for the pool ConfigMap"
else
  fail "kubevip.enabled=true renders no Role granting configmaps; the backend cannot write its pool"
fi

if grep -q -- '--kubevip-configmap=' "$kubevip_chart"; then
  pass "enabling kube-vip names the ConfigMap on the manager's command line"
else
  fail "kubevip.enabled=true does not pass --kubevip-configmap; the backend stays off"
fi

exit "$failed"
