# Security Policy

## Reporting a vulnerability

Report suspected vulnerabilities through GitHub's private advisory form:
[**Report a vulnerability**](https://github.com/PKizzle/dynamic-prefix-operator/security/advisories/new).

Please do not open a public issue for anything exploitable.

This is a single-maintainer project, so expect an acknowledgement within about a
week rather than within hours. A report that includes the version, the install
method (Helm chart or `install.yaml`), and what an attacker gains is much faster
to act on than one that does not.

## Supported versions

Fixes go into the latest release. There are no maintained release branches, so
"upgrade to the newest tag" is the remediation for anything found.

## What this operator can do, by design

Some properties look alarming but are inherent to what the operator is for.
Knowing which is which saves everyone time:

- **It runs as UID 0 with `NET_RAW` and `NET_BIND_SERVICE`, with
  `hostNetwork: true`.** Raw ICMPv6 sockets need the first, the DHCPv6 client's
  UDP 546 bind needs the second, and both read the uplink, which only exists in
  the host's network namespace. Everything else is dropped:
  `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, all other
  capabilities, and the `RuntimeDefault` seccomp profile.
- **It can rewrite annotations on any Service in the cluster.** It has to: users
  annotate arbitrary Services to opt them in. It only writes to objects carrying
  `dynamic-prefix.io/name` or an ownership record it wrote itself, but that is a
  behavioural limit, not an authorization one — the RBAC grant is cluster-wide.
- **It can take its prefix from Router Advertisements, which are
  unauthenticated.** RAs are validated as RFC 4861 §6.1.2 requires (link-local
  source, hop limit 255), which rules out off-link senders, but every host on
  the segment satisfies both, so anyone with access to the link can send a
  conforming advertisement and move the prefix. That is the same exposure every
  SLAAC host on the link has. On a link you do not control, combine any of:
  `acquisition.routerAdvertisement.trustedRouters` to believe only named
  routers; `acquisition.prefixFilter.minPrefixLength`/`maxPrefixLength` to bound
  what a plausible delegation looks like, for every source; RA Guard on the
  switch, which protects the whole segment rather than only this operator; or
  DHCPv6-PD, which does not take delegation from advertisements at all. Drops
  are counted in `dynamic_prefix_rejected_router_advertisements_total` and
  reported on the resource.
- **With the kube-vip backend enabled, it can rewrite one namespace's
  ConfigMaps.** The grant is a namespaced Role bound only when
  `kubevip.enabled` is set, and the informer is scoped to the single ConfigMap
  named by `--kubevip-configmap`. Left off, the operator needs no access to
  ConfigMaps at all.
- **Anyone who can edit a Service can ask for an address from the delegated
  prefix**, by adding the operator's annotations. This is address squatting
  within your own prefix, not a way to make the operator write an address of the
  attacker's choosing: supplied suffixes are masked against the current prefix
  and range-checked before use.

Reports that these are possible are not vulnerabilities. Reports that one of the
stated limits does not hold — an address escaping the delegated prefix, an
operator-written entry deleting something the user pinned, an RA accepted from
off-link — very much are.
