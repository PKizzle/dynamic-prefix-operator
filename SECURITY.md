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

- **It runs as UID 0 with `NET_RAW`, usually with `hostNetwork: true`.** Raw
  ICMPv6 sockets need it, and Router Advertisements only arrive in the host's
  network namespace. Everything else is dropped: `readOnlyRootFilesystem`,
  `allowPrivilegeEscalation: false`, all capabilities except `NET_RAW`, and the
  `RuntimeDefault` seccomp profile.
- **It can rewrite annotations on any Service in the cluster.** It has to: users
  annotate arbitrary Services to opt them in. It only writes to objects carrying
  `dynamic-prefix.io/name` or an ownership record it wrote itself, but that is a
  behavioural limit, not an authorization one — the RBAC grant is cluster-wide.
- **It takes its prefix from Router Advertisements, which are unauthenticated.**
  RAs are validated as RFC 4861 §6.1.2 requires (link-local source, hop limit
  255), which rules out off-link senders, but anyone with access to the link can
  still send a conforming advertisement and move the prefix. That is the same
  exposure every SLAAC host on the link has. If the link is not trusted, use RA
  Guard on the switch, or DHCPv6-PD instead.
- **Anyone who can edit a Service can ask for an address from the delegated
  prefix**, by adding the operator's annotations. This is address squatting
  within your own prefix, not a way to make the operator write an address of the
  attacker's choosing: supplied suffixes are masked against the current prefix
  and range-checked before use.

Reports that these are possible are not vulnerabilities. Reports that one of the
stated limits does not hold — an address escaping the delegated prefix, an
operator-written entry deleting something the user pinned, an RA accepted from
off-link — very much are.
