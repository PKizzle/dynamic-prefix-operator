# Changelog

All notable changes to the `PKizzle/dynamic-prefix-operator` fork are documented here.

This changelog follows the fork's published GitHub releases and does not align with upstream's releases.

## v0.0.10 - 2026-08-05

### Fixed

- Fixed the operator permanently orphaning one entry per prefix rotation in every field it shares with the user. Ownership was decided geometrically, by testing each existing entry against the prefixes in `status` (current + `Status.History`). That test holds until the moment a prefix is evicted from the history window — at which point the address the operator itself wrote for that prefix no longer matches any managed prefix, is reclassified as a user-supplied static entry, and is preserved forever. With the default `maxPrefixHistory: 2` this leaked one entry per rotation per object, without bound: an affected cluster reached 109 blocks in a `CiliumLoadBalancerIPPool` and 108 addresses in a single Service's `lbipam.cilium.io/ips`. The consequence was not merely untidy. The accumulated addresses saturated Cilium's L2 announcer, which then stopped answering neighbour solicitations for most current-prefix addresses, so IPv6 connectivity failed for most Services while DNS still resolved correctly and every other layer looked healthy. The operator now records what it wrote — `dynamic-prefix.io/managed-ips`, `-managed-targets`, `-managed-blocks`, `-managed-cidrs` — in the same update as the field it describes, and diffs against that record instead of inferring ownership from address shape. Note that inferring from shape cannot be made correct here: users pin ULA addresses that share the reserved host suffix with the operator's own addresses, so any structural rule would have to either special-case them or silently delete them. Objects written by earlier versions fall back to the old prefix test for a single pass, which over-preserves rather than deleting; entries leaked before the upgrade are not adopted retroactively and need a one-time cleanup.
- Fixed `external-dns.alpha.kubernetes.io/target` leaking the same way after an outage. That field holds only the current address, so an ordinary rotation always rewrites it while it is still inside the history window; but an operator down for longer than `maxPrefixHistory` rotations returns to find its own address aged out, and preserved it as a user's static target.
- Fixed a crash that took the whole operator down when a DHCPv6-PD server replied with an IA_PREFIX carrying a zero prefix length. The wire format places the lifetimes ahead of the prefix-length byte, so such an option decodes to a valid lifetime with a nil prefix, which was then dereferenced in a goroutine outside controller-runtime's panic recovery. A prefix-length above 128 is now rejected as well, instead of being accepted as a `/0` covering the entire address space.
- Fixed a panic and a correctness bug in subnet calculation. `SubnetSpec.Offset` has no upper bound, and a large value overflowed the 128-bit address arithmetic, leaving the resource in a permanent panic/requeue loop; smaller out-of-range offsets silently produced subnets outside the delegated prefix, which the operator then advertised despite not owning them. Offsets are now bounded by how many subnets actually fit in the base prefix, and the base prefix is masked before use so host bits cannot push a subnet out of range.
- Fixed pool synchronization selecting the wrong backend when pools of different kinds share a name. A reconcile request carries no kind, and the API server ignores the namespace when reading a cluster-scoped resource, so a request for a namespaced MetalLB `IPAddressPool` resolved to a same-named cluster-scoped Cilium pool and the MetalLB pool was never synced. Backends are now matched by scope.
- Fixed reconciliation errors being swallowed. PoolSync returned a nil error on every failure path, so failed syncs never reached `controller_runtime_reconcile_errors_total` and retried at a flat 30s instead of backing off; BGPSync logged and discarded advertisement create/update/delete failures, so a conflict was never retried. Both now surface their errors.
- Fixed the `dynamic_prefix_pools_synced` gauge only ever being set to 1, which meant it could never report the out-of-sync state its help text describes.
- Fixed pool synchronization silently discarding decode errors on `spec.blocks` and `spec.externalCIDRs`. A field of an unexpected type read back as empty and the subsequent write replaced it wholesale, destroying the unmanaged entries the preservation logic exists to protect.
- Fixed a DynamicPrefix receiver being torn down on any API error rather than only on NotFound, so a transient API-server failure no longer interrupts prefix acquisition.
- Fixed receivers being started with the reconcile context. They own goroutines that outlive the call, and were only safe because no reconciliation timeout is configured; they now use the manager's context.
- Fixed `status.subnets[].bgpAdvertisement` being cleared on every DynamicPrefix reconcile, which left the field permanently blank because the dependent-change predicate could not re-trigger BGPSync for it.
- Fixed prefix history recording the DynamicPrefix's creation timestamp as each entry's acquisition time.
- Fixed an IPv4 `dynamic-prefix.io/suffix` being silently mapped into IPv6 instead of rejected.
- Fixed Router Advertisement prefixes being used without masking or length validation, so host bits and out-of-range lengths could reach status and subnet calculation.
- Fixed a shared Router Advertisement receiver entry leaking its fan-out goroutine and NDP socket for the life of the process when a caller re-armed an entry that had already been stopped and removed from the pool.
- Fixed `AddressCount` underflowing to a near-`MaxUint64` value for a reversed range, and removed a dead loop in `RangeToCIDR` whose result was discarded.
- Added validation to `AddressRangeSpec.Start`/`.End` and `RouterAdvertisementSpec.Interface`, which were unconstrained while their sibling fields were validated.

### Added

- Added a `govulncheck` workflow (push, pull request and weekly) and a Dependabot configuration for Go modules, GitHub Actions and Docker, with Kubernetes libraries grouped so `controller-runtime` and `k8s.io/*` move together.
- Added the Artifact Hub `repositoryID` to `artifacthub-repo.yml` to enable the verified publisher badge.

### Changed

- Release images are now published as a single **dual image**: the version tag's index carries the standard OCI manifests alongside a Nydus manifest per architecture, so ordinary clients (docker, plain containerd) resolve the OCI half exactly as before while a Nydus-aware snapshotter picks up the accelerated half for lazy, on-demand pulls. The separate `-nydus` tag is no longer published.
- The release pipeline now builds `nydusify` and `nydus-image` from the `PKizzle/nydus` fork instead of downloading upstream's Go tooling, because `--attach-oci-manifest` — the flag that puts both manifests under one tag — exists only in the Rust implementation.
- Nydus conversion runs in zran (`--oci-ref`) mode, so the Nydus half references the OCI half's gzip layers instead of duplicating them: roughly 3-5% additional registry storage rather than doubling it.
- Generated GitHub release notes describe the dual image instead of advertising a separate `-nydus` tag.
- Migrated event emission to the `events.k8s.io/v1` API (`k8s.io/client-go/tools/events`), replacing the deprecated core/v1 recorder. The operator's ClusterRole now grants `events.k8s.io` in addition to the core group, which controller-runtime's leader election still uses. This requires a Kubernetes API server of 1.19 or newer, which is well below the floor the client libraries already impose.
- Updated `sigs.k8s.io/controller-runtime` to v0.24.1 and `k8s.io/api`, `k8s.io/apimachinery` and `k8s.io/client-go` to v0.36.3. These move in lockstep: each controller-runtime minor pins the Kubernetes minor it was built against. The envtest assets follow automatically (the Makefile derives both versions from `go.mod`), so the controller suite now runs against a Kubernetes 1.36 API server. Generated CRDs are unchanged.
- Pinned the Go toolchain to 1.26.5 via a `toolchain` directive. CI resolves its Go version from the `go` directive, which names 1.26.0 — a release carrying 20 standard-library advisories that failed the govulncheck job even though the shipped image builds on a patched 1.26.x.

### Security

- Updated `golang.org/x/text` to v0.40.0 and `golang.org/x/net` to v0.57.0, closing vulnerabilities reachable from Router Advertisement parsing — untrusted input from the local link. Also updated `go.opentelemetry.io/otel/sdk` to v1.44.0 and `google.golang.org/grpc` to v1.82.1.

## v0.0.9 - 2026-05-25

### Added

- Added release workflow support for publishing Nydus-compatible multi-architecture container images using the latest upstream `nydusify` tooling.

### Changed

- Switched the default controller-runtime/zap logging configuration to production-oriented output in the manager binary, Helm chart, and kustomize manager manifest while keeping zap flags configurable.
- Updated generated GitHub release notes to document the new `-nydus` GHCR image tag alongside the standard multi-arch release image.

### Fixed

- Fixed Helm repository publishing so the GitHub Pages Helm index points to the correct GitHub release chart assets without duplicating the packaged charts on Pages.
- Reduced Router Advertisement receiver log noise by moving steady-state lifecycle, solicitation, advertisement, and renewal messages to verbose levels instead of default info output.

### Tests

- Added unit coverage for Router Advertisement log verbosity behavior and the default production logger configuration.

## v0.0.8 - 2026-05-22

### Fixed

- Reduced no-op fan-out by ignoring DynamicPrefix lease-expiry and condition-only updates in dependent PoolSync, ServiceSync, and BGPSync watches.
- Refreshed the latest-release README badge URL to avoid stale invalid badge rendering.

### Documentation

- Documented that automatic workload restart orchestration after prefix changes is not implemented yet.

## v0.0.7 - 2026-05-22

### Added

- Added generic PoolSync backend dispatch with optional CRD discovery for supported pool APIs.
- Added MetalLB `IPAddressPool` synchronization through `dynamic-prefix.io/*` annotations, including address-range, subnet, history, and unmanaged-address preservation support.
- Added Calico `IPPool` synchronization through `dynamic-prefix.io/*` annotations, including safe exact-CIDR handling for address ranges.
- Added Prometheus metrics for prefix acquisition, prefix changes, lease expiry, and pool synchronization.
- Added Kubernetes events for prefix acquisition, prefix changes, prefix transition lifecycle, receiver startup failures, and pool updates.
- Added CEL/OpenAPI validation for acquisition configuration, named range/subnet lists, and non-negative subnet offsets.
- Added Helm repository publishing for GitHub Pages and Artifact Hub metadata for chart discovery.

### Changed

- Updated manager startup so PoolSync can run with Cilium, MetalLB, Calico, or any combination of available backend CRDs instead of requiring Cilium before registering pool synchronization.
- Updated README, architecture, prefix-acquisition, implementation-plan, and sample manifests to document DHCPv6-PD, subnet mode, MetalLB, and Calico as implemented features.
- Updated observability documentation to list dynamic-prefix specific metrics and emitted events.
- Updated install documentation and release notes to prefer the Helm repository while retaining OCI install instructions.
- Corrected subnet offset documentation to describe offset as the Nth target subnet inside the delegated prefix.

### Tests

- Added unit coverage for optional pool backend discovery, MetalLB/Calico backend update behavior, and Kubernetes event emission.

## v0.0.6 - 2026-05-22

### Added

- Added explicit leader-election lifecycle logging so standby replicas announce when they are waiting for the lease and when they become active.

### Changed

- Improved manager startup wording to clarify whether controllers will activate immediately or only on the elected replica.
- Clarified operator and Helm documentation so warm-standby HA is documented as requiring `replicaCount >= 2`, while non-leader replicas continue serving probes and metrics.

### Fixed

- Updated release automation to build GitHub release notes from `CHANGELOG.md`, preserve prerelease handling, and refresh notes when uploading assets to an existing release.

### Tests

- Added unit coverage for leader-election status logger transitions.

## v0.0.5 - 2026-05-22

### Fixed

- Suppressed steady-state reconciliation writes by skipping no-op updates in controllers.
- Scoped ServiceSync Service watches so unrelated Service events do not trigger unnecessary reconciliation.
- Shared Router Advertisement receivers by interface to avoid duplicate RA listeners for multiple `DynamicPrefix` resources on the same interface.
- Restored lintable Helm chart metadata and chart defaults.
- Updated golangci-lint to `v2.11.4`.

### Tests

- Added coverage for shared RA receiver lifecycle behavior, ServiceSync watch scoping, and no-op reconciliation paths.

## v0.0.4 - 2026-02-18

### Added

- Added background Cilium API availability detection so controllers can wait for Cilium resources before reconciling dependent objects.
- Added the `dynamic-prefix.io/skip-external-dns-update` Service annotation for HA mode deployments that should keep ExternalDNS targets unmanaged.

### Changed

- Updated the project to Go `1.26.0`.
- Updated golangci-lint to `v2.10.1`.

### Fixed

- Reworked release publishing to use the GitHub CLI and repaired release asset upload behavior.
- Fixed release tag formatting in CI.
- Fixed Docker image publishing behavior that produced excess untagged images.

### Tests

- Added unit coverage for Cilium API version discovery and ExternalDNS skip-annotation behavior.

## v0.0.3 - 2026-02-17

### Added

- Added Cilium API version discovery so the operator can adapt to available Cilium resource versions.

### Changed

- Updated project references and copyright metadata for the fork.
- Refreshed Cilium CRD test data used by controller tests.

### Tests

- Added unit coverage for Cilium version discovery.

## v0.0.2 - 2026-02-15

### Added

- Published the initial fork release with the core `DynamicPrefix` API, controller runtime wiring, and Router Advertisement based prefix acquisition.
- Added address-range mode for home/SOHO IPv6 deployments that reserve part of a delegated `/64` for Kubernetes services.
- Added Cilium `LoadBalancerIPPool` and `CIDRGroup` synchronization through annotations.
- Added simple and HA prefix transition modes, including multi-IP Service handling and DNS target management.
- Added BGP advertisement support for subnet-mode experiments.
- Added Helm chart packaging, Kubernetes manifests, and multi-architecture container image publishing.
- Added static IPv6 suffix support and dual-stack ServiceSync preservation for IPv4, static IPv6, and hostname entries.

### Fixed

- Fixed Router Advertisement handling for prefixes with `autonomous=false`.
- Fixed raw ICMPv6 socket permissions by documenting and configuring the required root, host-network, and `NET_RAW` settings.
- Fixed Helm and RBAC coverage for Service and Cilium BGP advertisement resources.
- Fixed release image naming, chart templating, and tag handling for lowercase GHCR package names.
- Fixed Docker build path and Helm deployment configuration issues.

### Documentation and Tests

- Added and refreshed user documentation for address-range mode, transition behavior, HA mode, static suffixes, and dual-stack DNS limitations.
- Added unit and edge-case tests for address range calculation, prefix receivers, pool synchronization, ServiceSync dual-stack handling, and BGP synchronization.