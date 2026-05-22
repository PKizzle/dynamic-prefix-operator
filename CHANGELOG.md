# Changelog

All notable changes to the `PKizzle/dynamic-prefix-operator` fork are documented here.

This changelog follows the fork's published GitHub releases and does not align with upstream's releases.

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