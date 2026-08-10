# L2 announcer never announces a LoadBalancer IP added after the Service's last event

> **STATUS: ALREADY FIXED UPSTREAM — DO NOT FILE THIS.**
>
> Cilium [PR #47579](https://github.com/cilium/cilium/pull/47579), *"l2announcer: re-evaluate
> services on frontend changes"* (commit `48b4dd178`, merged 2026-07-29) fixes exactly this, in
> exactly the way proposed under [Suggested fix](#suggested-fix) below.
>
> | Release | Has the fix |
> |---|---|
> | `v1.20.0` (what we run) | **no** |
> | `v1.21.0-pre.0` | yes |
> | `v1.20` branch | yes — will ship in `v1.20.1`, not yet tagged |
> | `v1.19.x` | no |
>
> We missed it by about 2½ hours: our `v1.20.0` image was built from `450c5314` at 06:53 UTC on
> 2026-07-29, and the fix merged at 09:34 UTC the same day.
>
> This document is kept as the root-cause record for the operator's workaround. **The real fix is
> upgrading Cilium** once `v1.20.1` is tagged; the workaround exists only to bridge until then.
> Everything below was written as a report to file, before the upstream fix was found.

> Drafted against `cilium/cilium` issue template `.github/ISSUE_TEMPLATE/bug_report.yaml`.
> Each `##` heading below maps to one field of that form.

## Is there an existing issue for this?

- [x] I have searched the existing issues

Closest existing issues, each checked and distinct from this one:

- **#44311** — IPv6 L2 announcements, node does not join the solicited-node multicast group for
  LoadBalancer IPs. That issue explicitly states the address *is* present in the `l2-announce`
  StateDB table; here it is **absent**, so the failure is one stage earlier.
- **#26586** — "L2 announcements stop working after a time". No identified mechanism, and the
  reporter's addresses do not change; here the trigger is precisely an address being *added*.
- **#45068** — L2 responder reconciler deleting non-existent BPF map entries. Fixed before v1.20.0
  and concerns deletion, not addition.

## Version

`equal or higher than v1.20.0 and lower than v1.21.0`

## What happened?

When a new LoadBalancer IP is assigned to an **existing** Service, the L2 announcer never announces
it. The address is fully functional everywhere else — LB-IPAM assigns it, it appears in
`status.loadBalancer.ingress`, and the datapath programs frontends for it — but it is missing from
the `l2-announce` table, so the node never answers ARP/NDP for it and the address is unreachable.

The state is permanent, not slow: measured across a full rotation, addresses that were not announced
within 3 seconds were still not announced **120 minutes** later. Any subsequent event that touches
the Service fixes it instantly and permanently.

`cilium-dbg statedb dump` on the lease-holding node, for one affected Service. All three prefixes are
assigned to the Service and identical in every respect except which is newest:

| Prefix | `lbipam.cilium.io/ips` | `status.loadBalancer.ingress` | frontends | `l2-announce` |
|---|---|---|---|---|
| `2003:db8:...:6900::` (older)  | yes | yes | 42 | **5** |
| `2003:db8:...:7900::` (older)  | yes | yes | 42 | **5** |
| `2003:db8:...:8100::` (newest) | yes | yes | 42 | **0** |

No errors appear in the agent log at any level; the announcer simply never recomputes.

**Expected:** an address added to a Service is announced, as the older addresses on the same Service
were when they were added.

### Root cause

`pkg/l2announcer/l2announcer.go` derives a Service's announced addresses from the **frontend** table
(`upsertSvc`, v1.20.0 L326-339):

```go
func (l2a *L2Announcer) upsertSvc(svc *loadbalancer.Service) error {
	txn := l2a.params.StateDB.ReadTxn()
	fes := l2a.params.Frontends.List(txn, loadbalancer.FrontendByServiceName(svc.Name))
	...
		lbAddresses = append(lbAddresses, fe.Address.Addr())
```

But the event loop (L189-235) wakes on only four sources — and the frontend table is not among them:

```go
select {
case <-ctx.Done():
case <-svcWatch:                         // Services table
case event, more := <-policyChan:        // CiliumL2AnnouncementPolicy
case event, more := <-localNodeChan:     // local node
case event := <-l2a.leaderChannel:       // lease / leader election
}
```

`Frontends` is injected into `l2AnnouncerParams` but is only ever `List()`ed synchronously from
inside those four handlers. **A frontend appearing generates no wakeup**, so `lbAddresses` is only
ever as fresh as the last Service, policy, node or lease event.

That makes the ordering decisive. Adding an IP to an existing Service produces two changes, in this
order:

1. the Service is updated (e.g. `lbipam.cilium.io/ips` gains an address) → `upsertSvc` runs → **the
   frontend does not exist yet**, so the announcer records the *previous* address set;
2. LB-IPAM assigns the address and creates the frontend → **no Service event follows** → `upsertSvc`
   never re-runs → the address is never announced.

This also explains the reports of L2 announcements that "fix themselves" or behave erratically: any
later unrelated Service update, policy change, lease failover or agent restart recomputes from the
current frontend table and silently repairs it.

## How can we reproduce the issue?

1. Install Cilium v1.20.0 with `enableLBIPAM: true` and a `CiliumL2AnnouncementPolicy` with
   `loadBalancerIPs: true`.
2. Create a `CiliumLoadBalancerIPPool` with a block, and a LoadBalancer Service requesting one
   address from it via `lbipam.cilium.io/ips`. Confirm it is announced and reachable.
3. Add a **second** block to the pool, then append a second address from that block to the same
   Service's `lbipam.cilium.io/ips` — i.e. update the existing Service rather than creating a new one.
4. Observe that LB-IPAM assigns the second address (`status.loadBalancer.ingress` has both) and the
   datapath creates frontends for it:

   ```
   cilium-dbg statedb dump | jq '.frontends[] | select(.Address | test("<new-address>"))'
   ```

5. Observe that `l2-announce` contains only the **first** address:

   ```
   cilium-dbg statedb dump | jq '.["l2-announce"][] | {IP, svc: .Origins[0].Name}'
   ```

6. The new address does not answer ARP/NDP and is unreachable. It stays that way indefinitely.
7. Touch the Service in any way (`kubectl annotate svc <name> foo=bar --overwrite`) and the address
   is announced within seconds — and stays announced even if the annotation is then removed.

Step 7 is the confirming test: nothing about the address, pool or policy changed, only the arrival of
a Service event.

### Observed reproduction

Environment rotates its ISP-delegated IPv6 prefix roughly daily; an operator rewrites
`lbipam.cilium.io/ips` on each Service with the new prefix's address, which is exactly step 3 above.
Every rotation leaves all public IPv6 addresses unreachable. Across one rotation of 7 Services:

- 3 announced within 3 s (each had received a further Service event by chance),
- 4 never announced — still absent after 120 minutes,
- all 7 announced within seconds of a no-op `kubectl annotate`, verified by `curl` returning 200 over
  the previously dead addresses.

`externalTrafficPolicy` (`Local` vs `Cluster`), lease-holder node, and lease age were each ruled out
by controlled comparison: the single `Local` Service was among the failures, one node held 5 leases
with 1 announced and 4 not, and all leases had been acquired hours before the rotation.

## Cilium Version

```
Client: 1.20.0 450c5314 2026-07-29T08:53:01+02:00 go version go1.26.5 linux/arm64
Daemon: 1.20.0 450c5314 2026-07-29T08:53:01+02:00 go version go1.26.5 linux/arm64
```

Images: `quay.io/cilium/cilium:v1.20.0`, `quay.io/cilium/operator-generic:v1.20.0`, Helm chart
`cilium-1.20.0`.

## Kernel Version

```
Linux 7.1.0-rc4-v8-16k #2 SMP PREEMPT Tue May 26 00:30:29 CEST 2026 aarch64 GNU/Linux
```

Debian 13 (arm64), 5-node Raspberry Pi 4/5 cluster.

## Kubernetes Version

```
Client Version: v1.36.3
Server Version: v1.36.2+k3s1
```

## Regression

Unknown. The affected code path reads the frontend table via StateDB, which is how the announcer has
worked since the `loadbalancer` StateDB tables were introduced, so this is more likely long-standing
than a recent regression. Not bisected — the environment has only run v1.20.0 with IPv6 L2
announcements, and IPv6 L2 announcement support itself only arrived in v1.19.0.

## Sysdump

Available on request.

## Relevant log output

```shell
# Agent log at the moment of the failure -- nothing about L2 announcements at any level.
# The only entries are an unrelated best-effort XDP message on a NIC that lacks support:
level=info msg="Failed to attach XDP program, ignoring due to best-effort mode" \
  module=agent.datapath.loader \
  error="attaching XDP program: attaching program cil_xdp_entry using bpf_link: \
  failed to attach link: create link: operation not supported" device=end0

# Service has the address:
$ kubectl get svc example -o jsonpath='{.status.loadBalancer.ingress[*].ip}'
192.0.2.10 2003:db8:0:6900:0:ffff:0:3 2003:db8:0:7900:0:ffff:0:3 2003:db8:0:8100:0:ffff:0:3

# Datapath has frontends for it (42, same as for the older prefixes):
$ cilium-dbg statedb dump | jq '[.frontends[] | select(.Address | test("8100"))] | length'
42

# The announcer does not:
$ cilium-dbg statedb dump | jq '[.["l2-announce"][] | select(.IP | test("8100"))] | length'
0

# After `kubectl annotate svc example nudge=1 --overwrite`:
$ cilium-dbg statedb dump | jq '[.["l2-announce"][] | select(.IP | test("8100"))] | length'
5
```

## Anything else?

### Suggested fix

Have the announcer wake on frontend changes as it already does on Service changes — take a
`statedb.Table[*loadbalancer.Frontend]` change iterator alongside `svcChangeIter` and select on its
watch channel, re-running `upsertSvc` for the affected Service. `upsertSvc` is already idempotent and
already recomputes the full address set, so no other change appears necessary.

A narrower alternative would be to have LB-IPAM's assignment produce a Service-table event, but
watching the table the data is actually read from seems the more robust fix.

**This is what upstream did.** PR #47579 adds precisely that, plus a shared read transaction:

```go
	frontendChangeIter, err := l2a.params.Frontends.Changes(wtxn)
	...
		frontendChanges, frontendWatch := frontendChangeIter.Next(rtxn)
		for event := range frontendChanges {
			if err := l2a.processFrontendEvent(rtxn, event); err != nil { ... }
		}
	...
		case <-frontendWatch:
```

### Workaround

For anyone hitting this: writing any annotation to the Service after the address appears in
`status.loadBalancer.ingress` supplies the missing event. Doing it once per change to the assigned
address set (keyed on a fingerprint of that set) avoids an update loop.

### Impact note

The failure is quiet — pool, annotation, status and frontends all look correct, and the agent logs
nothing — so it presents as an unreachable address with no diagnostic pointing at the announcer. The
`frontends`-vs-`l2-announce` comparison above is the quickest way to identify it.

## Cilium Users Document

- [ ] Are you a user of Cilium? Please add yourself to the Users doc

## Code of Conduct

- [x] I agree to follow this project's Code of Conduct
