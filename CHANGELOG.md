# Changelog

All notable changes to the `PKizzle/dynamic-prefix-operator` fork are documented here.

This changelog follows the fork's published GitHub releases and does not align with upstream's releases.

## v0.0.14 - 2026-08-10

This release is an audit round: one security fix in the operator itself, three in
the Helm chart, and a set of lifecycle defects that all shared a shape — the
operator knew what it had written and not when to stop maintaining it.

**Upgrade the chart, not just the image.** Three of the fixes are chart-side and
have no effect if only the image tag is bumped.

### Added

- Added `dynamic-prefix.io/force-l2-nudge`, which applies the [L2 announcer nudge](README.md#cilium-l2-announcer-nudge) whatever version detection concluded. Detection could previously only be told to stand *down*, so a fork or repackaging whose tag misreports left no recovery short of retagging the image — and because the resulting failure is silent, no way to discover one was needed. `dynamic-prefix.io/skip-l2-nudge` still wins if both are set.
- The `PoolsSynced` condition is now set. It has been declared since the first release and shown in the README's status example while nothing ever wrote it, so `kubectl wait --for=condition=PoolsSynced` waited forever and a pool that failed to sync was visible only in a log line and a Prometheus gauge. It aggregates across every pool referencing the prefix, so it describes the whole set rather than whichever pool reconciled last.
- Prefix changes now reach the controller by push rather than by polling. Every receiver has always published an events channel and nothing ever read one, so a rotation reached status only through the periodic requeue — capped at five minutes, during which every derived address, pool block and DNS target still described a withdrawn prefix. The poll remains as a backstop. Lease renewals are deliberately not pushed, since they move only the lease expiry.

### Fixed

- **Security (chart):** the chart granted `coordination.k8s.io/leases` **cluster-wide**, letting the operator's ServiceAccount delete or overwrite any Lease in the cluster — the node heartbeats in `kube-node-lease`, the control-plane leader locks in `kube-system` — while `config/rbac` had always scoped the identical rule to a namespaced Role. The chart now ships that Role. **Upgrade the chart, not just the image.**
- **Security (chart):** `seccompProfile` was `Unconfined` on a UID-0 process that holds `NET_RAW` and parses ICMPv6 and DHCPv6 straight off the wire. It is `RuntimeDefault`, as the kustomize install has always been.
- **Security (chart):** metrics were served as unauthenticated plaintext because the template hardcoded `--metrics-secure=false` over a binary whose default is secure; with `hostNetwork` that listener sits on the node's own address. Secure is now the default, with the TokenReview/SubjectAccessReview ClusterRole the authenticated path needs.
- **Security:** Router Advertisements are validated as RFC 4861 §6.1.2 requires — link-local source, hop limit 255 — before anything in them is believed. Neither check was performed by this code or by `mdlayher/ndp`, so the prefix the whole cluster derives its addresses from was taken from whatever advertisement arrived last, from any source, forwarded or not. This does not make RA delegation a trusted channel (anyone on the link can still forge a conforming advertisement, which is inherent); it restores the floor every conforming NDP implementation provides and rules out off-link senders.
- The L2 announcer version gate believed a tag too readily, and every misreading fails in the expensive direction — a wrong "already fixed" silently stops announcing rotated addresses. Four paths concluded "fixed" where the documentation promised a fallback to nudging: a leniently parsed tag turned a date-stamped nightly (`cilium:20260810`) or a bare build number into an enormous version; any image sitting in the DaemonSet was trusted, so an injected sidecar's version could decide it; a DaemonSet whose agent container is named something else fell back to the first container; and the template was read without checking whether the nodes had caught up, leaving a whole Cilium rolling upgrade unprotected. Ambiguity between several labelled DaemonSets is now reported rather than settled by list order, and verdict changes are logged at info.
- Deleting a `DynamicPrefix` released nothing. The delete event was filtered out of the dependent watches, so pools kept their blocks and Services kept an `external-dns` target that stops resolving at the next rotation, while PoolSync error-looped on the missing reference forever and ServiceSync polled for it every 30 seconds, equally forever.
- Switching `transition.mode` from `ha` back to `simple` stranded the annotations HA mode had written; the fan-out refused to notify non-HA prefixes at all, so the Service was never reconsidered.
- Calico was outside the ownership model: `spec.cidr` carried no ownership record, so a de-annotated `IPPool` matched no watch predicate and was never handed back, and the draining sibling `IPPool`s — objects the operator creates, with no owner reference — were pruned only from inside a successful sync, so de-annotating the parent left them allocating from a prefix the ISP had already withdrawn.
- The release path dispatched on which record annotation was present rather than on the matched backend, so a pool carrying another backend's record had `spec.blocks` read and written on it — creating that field on objects whose schema has no such thing.
- Editing `spec.acquisition` did nothing. Receivers were cached by name, and an interface, source or filter is fixed when one is built, so moving the operator to another NIC or switching between RA and DHCPv6-PD was accepted by the API server and then ignored until the pod restarted, with nothing logged.
- A prefix that flaps A → B → A left A in history while A was again current. Cilium and MetalLB dedupe and only do redundant work, but Calico renders the duplicate as a sibling `IPPool` holding its parent's CIDR, which Calico rejects as an overlap — so the sync failed and kept failing for as long as the flap lasted.
- Historical addresses for a Service are derived by measuring the distance from its assigned address to the start of its range, with no sign check: an address *below* the start — a pin, or a range narrowed after assignment — produced a distance just under 2¹²⁸ that wrapped around to an address unrelated to any managed prefix. It was then written into `lbipam.cilium.io/ips` *and recorded as the operator's*. The only check was `IsValid()`, which `netip.AddrFrom16` satisfies for any 16 bytes.
- ServiceSync swallowed every error and two PoolSync paths still did: a misspelled address-range name, a malformed suffix, a rejected update or a lost conflict returned nil with a flat requeue, so they retried at a fixed interval forever, never backed off, and never appeared in `controller_runtime_reconcile_errors_total`. v0.0.10 fixed this for PoolSync and BGPSync; ServiceSync was never converted.
- A receiver could be started with the reconcile request's context, which is cancelled the moment that call returns, leaving a dead receiver cached under a live name and the resource waiting for a prefix that could never arrive. The receiver context is now created during setup rather than captured from a Runnable racing the controllers.
- A nil `ReceiverFactory` silently substituted a mock receiver, which in a misconfigured binary means reporting a fabricated prefix and writing it into real pools. It is an error now.
- `spec.acquisition.routerAdvertisement.enabled` could not express `false`: a plain bool carrying both `omitempty` and `default=true` makes the zero value indistinguishable from unset, so a Go client round-tripping the object dropped it and the API server defaulted it back to true.
- BGPSync's hand-rolled condition helpers discarded `LastTransitionTime` on every write and compared only status and message, so a change of reason alone was computed and then dropped. A decode failure on a pool's `spec.serviceSelector` was treated as "no selector defined", which builds an advertisement matching every Service instead of the intended subset.
- Metric series were never deleted, so a released pool went on reporting itself in sync and a deleted `DynamicPrefix` kept publishing a lease expiry, while the label set grew for the life of the process.
- The released `install.yaml` had no `hostNetwork`, so the manifest the README recommends installed cleanly and then never saw a Router Advertisement.
- Artifact Hub reported `error scanning image ghcr.io/pkizzle/dynamic-prefix-operator:v0.0.13: image not found`. The chart is published within a minute of the tag while the image workflow needs around half an hour for the multi-arch build and the nydus attach, so the release advertised an image the registry did not have yet — a failed scan, and an `ImagePullBackOff` for anyone installing in that window. The release now waits for the tag to be pullable and names the image explicitly for the scanner.
- The default `image.tag` was `main`, a continuously overwritten tag; it now falls back to the chart's `appVersion`.

### Changed

- The three pool backends each carried their own copy of the same shared-list sync, and the copies had already drifted once in a way that shipped (v0.0.11's MetalLB leak). The procedure now exists once, and `dupl` is enforced on production code so the next divergence is caught rather than released.
- CI can now fail for the reasons it exists: the suite runs under `-race` with a coverage floor, `go mod tidy` is checked rather than run, and four linters are added (`gosec`, `errorlint`, `nilerr`, `bodyclose`). A new parity check asserts the chart-versus-kustomize security properties that drifted three times.
- Supply chain: every GitHub action is pinned by commit SHA, two third-party actions were removed entirely, both container base images are pinned by digest, `govulncheck` is no longer installed as `@latest`, and the nydus fork used to build the dual image is checked out at a commit rather than a mutable branch of another repository — it is built from source and executed in the job holding `packages: write`. Published images are signed with keyless cosign, given an SBOM and scanned.
- Added [`SECURITY.md`](SECURITY.md), which also records which alarming-looking properties are inherent to the operator's job and which are not.

## v0.0.13 - 2026-08-10

### Added

- The L2 announcer nudge added in v0.0.12 now applies itself only where it is needed, instead of on every cluster. The operator reads the tag of the Cilium agent DaemonSet's image (`k8s-app=cilium`) and nudges only on a release predating [cilium/cilium#47579](https://github.com/cilium/cilium/pull/47579); `v1.20.1` and newer are treated as fixed, a threshold that covers the 1.21 line too because `1.21.0-pre.0` sorts above `1.20.1`. Upgrading Cilium therefore switches the workaround off by itself, within five minutes and without editing any Service. Previously it had to be disabled by hand, per Service, via `dynamic-prefix.io/skip-l2-nudge` — which remains as an escape hatch for a fork whose tag misreports, or a cluster not using L2 announcements.

  Detection resolves every uncertain case to "nudge": an unreadable tag, a digest-only image pin, an unfamiliar fork, a missing DaemonSet and absent RBAC all fall back to the workaround. The failure directions are not symmetric — a wrong "already fixed" silently stops announcing rotated addresses and looks healthy from every other angle, while a wrong "still broken" costs one annotation write per change to a Service's address set. The verdict expires every five minutes so a Cilium upgraded underneath a running operator is noticed without a restart.

  This needs read-only `get`/`list`/`watch` on `apps/daemonsets`, added to the chart's ClusterRole. Existing installations must upgrade the chart, not just the image; without the rule the version cannot be read and the operator keeps nudging unconditionally, which is correct but redundant.

## v0.0.12 - 2026-08-10

### Added

- Added a workaround for a Cilium L2 announcer bug that left every managed Service unreachable at its new address after a prefix rotation. Cilium builds a Service's announced addresses from the frontend table but only recomputes them on Service, policy, node or lease events — never when a frontend appears. On a rotation the operator's annotation update reaches the announcer *before* LB-IPAM has assigned the new address, so the announcer stores the previous set; LB-IPAM then creates the frontend and, with no further Service event, the address is never announced. Nothing else looks wrong — the pool block, `lbipam.cilium.io/ips`, `status.loadBalancer.ingress` and the datapath frontends all carry the address — so the only symptom is that it silently fails to answer ARP/NDP, and it is permanent rather than slow (measured: still absent 120 minutes after a rotation, then announced within seconds of any unrelated Service update). The operator now writes `dynamic-prefix.io/l2-announce-nudge` once the address is present in `status.loadBalancer.ingress`, supplying the event Cilium is missing. The value is a fingerprint of the assigned address set, so it is written once per change rather than on every reconcile. Set `dynamic-prefix.io/skip-l2-nudge: "true"` to disable it for a Service.

  Upstream fixed this in [cilium/cilium#47579](https://github.com/cilium/cilium/pull/47579) (merged 2026-07-29), which is in `v1.21.0-pre.0` and on the `v1.20` branch but **not in `v1.20.0`** — it landed about 2½ hours after that release was built, and is expected in `v1.20.1`. Upgrading Cilium is the real fix; this workaround only bridges until then, and is redundant on any release carrying #47579. Root-cause record: [`docs/cilium-l2announcer-bug-report.md`](docs/cilium-l2announcer-bug-report.md).

## v0.0.11 - 2026-08-06

### Added

- Added `spec.acquisition.prefixFilter.requireGlobalUnicast` (default `true`), which rejects any acquired prefix outside `2000::/3`. A delegated prefix is global unicast by definition, but a link commonly advertises more than one prefix, and the Router Advertisement receiver only *preferred* a global one: an advertisement carrying no global prefix — during upstream renegotiation, or on a link where a unique-local prefix is advertised alongside — was accepted as though the delegation had changed. Every derived address then moved into a range that is not routable off-link, and the real prefix aged out of `status.history` as if it had been retired, so anything keyed on "is this prefix still mine" disowned addresses that were still in use. Set the field to `false` to track a prefix that is deliberately not global unicast. Enforcement is applied in both receivers and again in the DynamicPrefix controller before anything reaches status, because a receiver is created once and shared per interface and can outlive a configuration change. Rejection is non-destructive: status keeps the last good prefix and the resource reports `PrefixAcquired=False` with reason `PrefixRejected`.

### Fixed

- Fixed the MetalLB backend still deciding ownership of `spec.addresses` geometrically, so it leaked one address per prefix rotation without bound — the same defect the other backends had fixed, on the one backend that was never converted. It now uses an ownership record (`dynamic-prefix.io/managed-addresses`) like the rest.
- Fixed the no-record fallback path deleting user entries that *contain* a managed prefix. Containment was tested in both directions, so pinning the whole delegation while the operator managed a subnet of it made the pin look like the operator's own entry. This fired on the first reconcile after upgrading any existing installation, silently and exactly once per object. Containment is now tested in one direction only, and an address range must have both ends inside a single managed prefix to be claimed.
- Fixed a pool block that yields no usable identity being written but never recorded. Such a block could not be recognised as the operator's on any later pass, so it was preserved as a user entry and a fresh copy appended on every reconcile — reintroducing unbounded growth. Blocks with no identity are now refused rather than written.
- Fixed ownership being decided by the exact spelling of an address. An entry pinned in a different case or with leading zeros was neither recognised as the user's nor deduplicated against the operator's, so the same address could be requested twice. Addresses, prefixes and pool blocks are now compared by identity, and a block with an empty `stop` now matches one with no `stop` at all.
- Fixed the operator claiming an address or block that the user had already pinned. Recording it granted the operator the right to remove it once it stopped being generated, turning a deliberate pin into a delayed failure several rotations later. Entries already present and unowned are now left to the user; the operator's duplicate is suppressed instead.
- Fixed removing the `dynamic-prefix.io/name` annotation stranding everything the operator had written. The watch predicate matched only on that annotation, so its removal delivered no event at all and the entries stayed behind — including an `external-dns` target that stops resolving at the next rotation. Objects carrying an ownership record are now watched until the recorded entries have been released.
- Fixed `dynamic-prefix.io/skip-external-dns-update` leaving a stale target behind when it is set after the target was already managed. Opting out now hands the field back rather than merely ceasing to update it.
- Fixed pool synchronisation reconciling only the first backend kind matching a request. A reconcile request carries no kind and several backends are cluster-scoped, so a pool whose name it shared with another kind was never reconciled — with no error to show for it. Every matching kind is now synced, with errors aggregated.
- Fixed the Calico backend having no drain window. `spec.cidr` holds a single prefix, so writing the new one removed the old in the same operation and anything still using an address from it lost connectivity the instant the delegation rotated. Draining prefixes now live in sibling IPPools, created and removed as the history window moves. This requires `create` and `delete` on `projectcalico.org/ippools`, which the bundled ClusterRole now grants.
- Fixed Services without a `dynamic-prefix.io/suffix` annotation being unable to follow a rotation. Such a Service derived everything from its already-assigned address, so after a rotation it kept requesting the superseded one and never asked for an address in the new prefix, waiting on an assignment that only arrives once something else moves first. The host part is now inferred from the assigned address and rebuilt against the current and historical prefixes.
- Fixed DHCPv6-PD accepting a delegated prefix without masking it, so host bits sent by the server reached `status`, change detection and every derived address. The Router Advertisement path has always masked.
- Fixed generated CRDs and the copy bundled with the Helm chart being kept in step by hand; `make manifests` now syncs them.

## v0.0.10 - 2026-08-05

### Fixed

- Fixed the operator permanently orphaning one entry per prefix rotation in every field it shares with the user. Ownership was decided geometrically, by testing each existing entry against the prefixes in `status` (current + `Status.History`). That test holds until the moment a prefix is evicted from the history window — at which point the address the operator itself wrote for that prefix no longer matches any managed prefix, is reclassified as a user-supplied static entry, and is preserved forever. With the default `maxPrefixHistory: 2` this leaked one entry per rotation per object, without bound. The consequence was not merely untidy: once enough entries accumulated they saturated Cilium's L2 announcer, which stopped answering neighbour solicitations for most current-prefix addresses, so IPv6 connectivity failed for most Services while DNS still resolved correctly and every other layer looked healthy. The operator now records what it wrote — `dynamic-prefix.io/managed-ips`, `-managed-targets`, `-managed-blocks`, `-managed-cidrs` — in the same update as the field it describes, and diffs against that record instead of inferring ownership from address shape. Note that inferring from shape cannot be made correct here: a unique-local address pinned for stable internal reachability commonly shares the reserved host suffix with the operator's own addresses, so any structural rule would have to either special-case it or silently delete it. Objects written by earlier versions fall back to the old prefix test for a single pass, which over-preserves rather than deleting; entries leaked before the upgrade are not adopted retroactively and need a one-time cleanup.
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