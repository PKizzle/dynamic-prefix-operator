# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Dynamic Prefix Operator is a Kubernetes operator that manages dynamic IPv6 prefix delegation for bare-metal and home/SOHO clusters. It automatically updates supported pool backends (Cilium LoadBalancerIPPools/CIDRGroups, MetalLB IPAddressPools, Calico IPPools) when ISP-delegated IPv6 prefixes change, detected via Router Advertisements or DHCPv6-PD.

## Build Commands

```bash
make build            # Build manager binary
make test             # Run unit tests with coverage (race detector enabled)
make test-coverage    # Run tests and enforce the coverage floor, as CI does
make tidy-check       # Fail if go.mod/go.sum are not tidy, as CI does
make lint             # Run golangci-lint
make lint-fix         # Run linter with auto-fixes
make generate         # Generate code (deepcopy, CRDs)
make manifests        # Generate CRDs and RBAC manifests
make run              # Run operator locally against configured cluster
make docker-build     # Build container image
make helm-lint        # Lint Helm chart
make test-e2e         # Run e2e tests with Kind cluster
```

## Architecture

### Core Components

1. **Prefix Receiver Interface** (`internal/prefix/types.go`): Abstraction for prefix acquisition with channel-based events for prefix changes. Implementations: `RAReceiver` (Router Advertisements, pooled per interface *and* acceptance policy via `shared_ra_receiver.go`), `DHCPv6PDReceiver` (a DHCPv6-PD client; one per interface, enforced by the factory), `CompositeReceiver` (DHCPv6-PD primary with RA fallback), and `MockReceiver`. Acceptance rules live in `internal/prefix/policy.go` (`Policy`, `RAPolicy`) and are fixed at construction, so a spec edit rebuilds the receiver. Receivers implement two optional interfaces reconcile polls: `AcquisitionHealth` (why acquisition is failing — failure events are deliberately *not* forwarded to the reconciler) and `RARejectionStats` (dropped advertisements).

2. **DynamicPrefix CRD** (`api/v1alpha1/dynamicprefix_types.go`): Custom resource defining prefix acquisition settings, address range definitions, and transition configuration. Status tracks current prefix, calculated ranges, and conditions (PrefixAcquired, PoolsSynced, Degraded).

3. **DynamicPrefixReconciler** (`internal/controller/dynamicprefix_controller.go`): Main reconciliation loop that manages prefix receivers, updates status, and handles graceful transitions with finalizer-based cleanup.

4. **Address Range Calculation** (`internal/prefix/addressrange.go`): Combines prefix with start/end suffixes to calculate full address ranges within the /64.

5. **BGPSyncReconciler** (`internal/controller/bgpsync_controller.go`): Manages `CiliumBGPAdvertisement` for subnets marked `bgp.advertise`, and writes the `BGPAdvertisementReady` condition. Cilium-only; MetalLB and Calico BGP configuration stays user-managed.

6. **PoolSyncReconciler** (`internal/controller/poolsync_controller.go`): Syncs annotated supported pool backends with calculated address ranges/subnets. Supports multi-block or multi-entry mode where the backend can represent it, keeping entries for current prefix plus historical prefixes. Backends are unstructured; `backendForGVK` in `poolsync_backends.go` maps GVK to implementation. The kube-vip ConfigMap backend (`kubevip_backend.go`) is opt-in via `--kubevip-configmap` rather than discovered, because ConfigMaps exist everywhere. Also writes the referenced DynamicPrefix's `PoolsSynced` condition (`internal/controller/poolsync_condition.go`), aggregated across every pool bound to it — so `DynamicPrefix.status` has two writers, this one and the DynamicPrefixReconciler.

7. **ServiceSyncReconciler** (`internal/controller/servicesync_controller.go`): HA mode controller that manages LoadBalancer Services. Writes the load-balancer address annotation named by `dynamic-prefix.io/lb-provider` (`cilium` by default, `kube-vip` also supported) for multi-IP assignment and the external-dns target annotation for DNS targeting, under every key in `--external-dns-target-annotation-keys` (default: both the `external-dns.alpha.kubernetes.io/` and `external-dns.kubernetes.io/` spellings, since external-dns v0.22 changed its default prefix with no fallback); a known key that is no longer configured is released from the Services it owns (skippable per-Service via `dynamic-prefix.io/skip-external-dns-update: "true"`). Supports two IP calculation modes: **static suffix** (explicit `dynamic-prefix.io/suffix` annotation, preferred for dual-stack) and **dynamically assigned** (inferred from Cilium-assigned IP). Preserves non-managed entries (hostnames, IPv4, static IPv6) in both annotations via `extractUnmanagedIPs()`, supporting dual-stack NAT setups where IPv4 uses a hostname and IPv6 uses direct addresses. Also nudges Cilium's L2 announcer (`ciliuml2nudge.go`) after an address change, skipped when the provider is not Cilium.

### Data Flow

```
ISP/Router → RA Receiver or DHCPv6-PD client → DynamicPrefix CR (status.currentPrefix)
    → Pool Controller (watches annotated pools, builds multi-block configs)
    → Supported pool backends (Cilium, MetalLB, Calico, kube-vip ConfigMap updated with current + historical entries where supported)
    → BGP Controller (subnets marked bgp.advertise → CiliumBGPAdvertisement)
    → Service Controller (HA mode: manages Service IPs and DNS targeting)

Pool Controller → DynamicPrefix CR (status PoolsSynced condition)
```

### Pool Integration

Uses annotation-based binding (inspired by 1Password Operator):
- `dynamic-prefix.io/name`: References the DynamicPrefix CR
- `dynamic-prefix.io/address-range`: Specifies which address range to use

- `dynamic-prefix.io/kubevip-key`: kube-vip only — which pool key of the ConfigMap to manage

The operator watches annotated Cilium, MetalLB, Calico and (when enabled) kube-vip resources and auto-updates backend-specific fields: Cilium `spec.blocks`/`spec.externalCIDRs`, MetalLB `spec.addresses`, Calico `spec.cidr`, or one `cidr-*`/`range-*` key of the kube-vip ConfigMap.

### Transition Modes

- **Simple mode** (default): Pools contain multiple blocks for current + historical prefixes. Services keep old IPs until blocks are pruned.
- **HA mode**: ServiceSync controller manages multi-IP Services with DNS targeting for zero-downtime transitions.

## Testing

- **Unit tests**: mostly standard `testing` with table-driven cases in `*_test.go` files; some suites (notably parts of `internal/controller`) use the Ginkgo/Gomega BDD framework against envtest. Match whichever style the file you are editing already uses.
- **Integration tests**: `internal/integration/` for ISP simulation scenarios
- **E2E tests**: `test/e2e/` using Kind clusters

Run a single test file:
```bash
go test -v ./internal/prefix/addressrange_test.go ./internal/prefix/addressrange.go ./internal/prefix/types.go
```

Run tests matching a pattern:
```bash
go test -v ./... -run TestAddressRange
```

## Key Technologies

- **Go 1.26.0** with controller-runtime v0.24.1 (Kubebuilder 4.10.1)
- **mdlayher/ndp**: Router Advertisement (NDP) monitoring
- **Helm 3** and **Kustomize** for deployment

## Code Patterns

- Kubebuilder reconciler pattern with controller-runtime
- Interface-based design for pluggable prefix acquisition methods
- Channel-based event system for asynchronous prefix changes
- Annotation-based binding instead of separate binding CRDs
