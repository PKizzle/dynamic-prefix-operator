# Prefix Acquisition Modes

This guide explains how the Dynamic Prefix Operator works with your network setup.

## The Problem: Dynamic IPv6 Prefixes

Most home and small office internet connections receive a dynamic IPv6 prefix from the ISP. This prefix can change periodically (e.g., after router reboots or lease expiry). When running Kubernetes services that need stable IPv6 addresses (like LoadBalancers), you need a way to:

1. Detect when your prefix changes
2. Update your Kubernetes IP pools automatically
3. Ensure the addresses you use don't conflict with other devices

## How IPv6 Prefix Delegation Works

Understanding the typical home/SOHO network helps clarify the setup:

```
ISP
 │
 │ Delegates /56 via DHCPv6-PD (e.g., 2001:db8:abcd::/56)
 ▼
Your Router (UniFi, OpenWRT, etc.)
 │
 │ Assigns /64 per VLAN via Router Advertisement
 │ (e.g., 2001:db8:abcd:01::/64 for VLAN 1)
 ▼
Your Network/VLAN
 │
 ├── Device A (gets 2001:db8:abcd:01::<random>/64 via SLAAC)
 ├── Device B (gets 2001:db8:abcd:01::<random>/64 via SLAAC)
 └── K8s Nodes (get 2001:db8:abcd:01::<random>/64 via SLAAC)
```

**Key insight**: Your Kubernetes nodes typically only see the `/64` that the router advertises to their VLAN, not the full `/56` that the ISP delegated.

---

## Address Range Mode (Recommended)

**Use a reserved range within your existing /64**

This is the simplest approach and works for most home and small office setups.

### How it works

```
Router advertises:     2001:db8:abcd:01::/64
SLAAC/DHCPv6 uses:     2001:db8:abcd:01:0:* through 2001:db8:abcd:01:efff:*
Operator reserves:     2001:db8:abcd:01:f000:* through 2001:db8:abcd:01:ffff:*
                       └── 4096 addresses for LoadBalancers
```

The operator observes the /64 via Router Advertisements and allocates addresses from a range that your router is configured to leave unused.

### Requirements

1. **Configure your router** to exclude a range from DHCPv6/SLAAC
   - UniFi: Network → IPv6 → DHCPv6 Range (leave out the high range)
   - OpenWRT: Set DHCPv6 pool to exclude your reserved range

2. **Tell the operator** which range to use (must match router config)

### Example Configuration

```yaml
apiVersion: dynamic-prefix.io/v1alpha1
kind: DynamicPrefix
metadata:
  name: home-prefix
spec:
  acquisition:
    routerAdvertisement:
      interface: eth0
      enabled: true

  addressRanges:
    - name: loadbalancers
      # Reserve the last portion of the /64
      start: "::f000:0:0:0"
      end: "::ffff:ffff:ffff:ffff"
```

### Advantages

- **Simple setup** - no BGP or advanced routing required
- **Works immediately** - the /64 is already routed to your VLAN
- **Automatic updates** - when ISP prefix changes, operator detects new /64 and updates pools
- **Compatible with any router** - just needs DHCPv6 range configuration

### Considerations

- **Requires router coordination** - must configure router to not hand out addresses in your range
- **Shared address space** - your K8s services share the /64 with other devices (though in separate ranges)

### Who should use this

- Home labs and small office Kubernetes clusters
- Users who want simple, working IPv6 LoadBalancers
- Setups where the router handles DHCPv6-PD (most common)

---

## Router Configuration Examples

### UniFi

1. Go to **Network** → **Settings** → **Internet**
2. Under **IPv6**, find DHCPv6 settings
3. Set the DHCPv6 range to exclude your reserved range:
   - Start: `::1`
   - End: `::efff:ffff:ffff:ffff`
   - This leaves `::f000:0:0:0` through `::ffff:ffff:ffff:ffff` for K8s

### OpenWRT

In `/etc/config/dhcp`:
```
config dhcp 'lan'
    option dhcpv6 'server'
    option ra 'server'
    list dns '2001:4860:4860::8888'
    # Limit the pool to avoid your reserved range
    option pool_start '::1000'
    option pool_end '::efff:ffff:ffff:ffff'
```

---

## Troubleshooting

### "Operator isn't detecting my prefix"

- Ensure the operator pod has access to the network interface
- For hostNetwork mode, verify the interface name is correct
- Check that Router Advertisements are being sent (use `rdisc6` or `tcpdump`)

### "Addresses conflict with my devices"

- Verify your router's DHCPv6 range excludes your operator's range
- Check that no static IPs are assigned in the reserved range
- Ensure the start/end don't overlap with SLAAC range

---

## Subnet Mode with BGP (Advanced)

Subnet mode is implemented for users who receive a larger delegated prefix (for example `/56` or `/48`) and want to carve dedicated service subnets from it. The operator calculates `status.subnets` from `spec.subnets`, updates annotated pools that reference `dynamic-prefix.io/subnet`, and can create Cilium `CiliumBGPAdvertisement` resources for subnets with `bgp.advertise: true`.

```yaml
apiVersion: dynamic-prefix.io/v1alpha1
kind: DynamicPrefix
metadata:
  name: advanced-ipv6
spec:
  acquisition:
    dhcpv6pd:
      interface: eth0
      requestedPrefixLength: 56
    routerAdvertisement:
      interface: eth0
      enabled: true

  subnets:
    - name: loadbalancers
      # Nth /64 inside the delegated prefix; with 2001:db8:1234::/56,
      # offset 255 becomes 2001:db8:1234:ff::/64.
      offset: 255
      prefixLength: 64
      bgp:
        advertise: true
        community: "65001:100"
```

### Requirements

- A prefix source that exposes a prefix larger than the target subnet. DHCPv6-PD is supported directly via `spec.acquisition.dhcpv6pd`; Router Advertisement monitoring can still be configured as a fallback.
- For Cilium BGP advertisement, Cilium BGP Control Plane and peering must be configured separately. The operator manages what to advertise, not the peer/session setup.
- For MetalLB or Calico backends, configure their advertisement/peering resources separately. The operator manages pool addresses only.

### Who should use this

- Users with a delegated `/56` or larger prefix.
- Setups that need strict separation between Kubernetes service space and the LAN `/64`.
- Operators who are comfortable managing BGP peering and route filters.

---

## How the prefix is acquired

The two modes above describe what the operator does with a prefix. This section
is about where it comes from, which is a separate choice.

| | Router Advertisements | DHCPv6-PD |
|---|---|---|
| Spec | `acquisition.routerAdvertisement` | `acquisition.dhcpv6pd` |
| What it sees | The `/64` the router advertises to this VLAN | The prefix the upstream delegates, typically a `/56` or `/48` |
| Capability | `NET_RAW` | `NET_BIND_SERVICE` |
| Works behind switch-side RA Guard | No | Yes |
| Trust | Unauthenticated; anything on the link can send one | The exchange is addressed to a server, and a rogue reply has to win a race |

Configure both and the operator runs DHCPv6-PD as the primary source with
Router Advertisements as a fallback, reporting `Degraded` while it is serving an
advertisement-derived prefix because the DHCPv6 client is failing.

### DHCPv6-PD mode

```yaml
apiVersion: dynamic-prefix.io/v1alpha1
kind: DynamicPrefix
metadata:
  name: home-ipv6
spec:
  acquisition:
    dhcpv6pd:
      # The interface facing the upstream. The client sources from this
      # interface's link-local address, which is why the operator runs with
      # hostNetwork.
      interface: eth0
      # A hint, not a demand: the server may delegate something shorter.
      requestedPrefixLength: 56
  subnets:
    - name: services
      prefixLength: 64
      offset: 1
```

The client identifies itself with a DUID derived from the interface's hardware
address and an IAID derived from its index. Both are stable across restarts, so
a restarted operator is offered the same delegation without needing to persist a
lease. It follows the ordinary lease lifecycle: SOLICIT and REQUEST to acquire,
RENEW at T1, REBIND at T2, and a fresh SOLICIT if the server reports it holds no
binding. Failed acquisitions back off from ten seconds to five minutes and
return to ten seconds as soon as one succeeds.

**Only one DynamicPrefix may run a DHCPv6-PD client on a given interface.** Two
would present the same DUID and IAID, and each would overwrite the other's
lease; the second is refused with `PrefixAcquired=False` and reason
`ReceiverCreationFailed`. One delegation is one DynamicPrefix, and it can carry
as many `addressRanges` and `subnets` as you need.

**Nothing else on the node may hold UDP 546 on that interface.** `dhcpcd`,
`systemd-networkd` and NetworkManager all bind it when DHCPv6 is enabled.
Either disable DHCPv6 on that interface host-side, or let the host client do the
delegation and have the operator watch the resulting Router Advertisements
instead.

Known limits, none of which have caused a problem in practice:

- Where an upstream delegates several prefixes in one IA_PD, only the first
  usable one is taken.
- Reconfigure is not supported, so a server cannot push a change; the next
  renewal picks it up.
- Rapid Commit is not used, so acquisition is the full four-message exchange.
- No RELEASE is sent on shutdown. That is deliberate: keeping the binding is
  what lets a restarted operator come back to the same prefix.

### Operating behind switch-side RA Guard

RA Guard (RFC 6105) drops Router Advertisements that did not come from a
designated port, which is the right thing to run on a segment you do not fully
control — it protects every host on it, not just this operator. It also means
advertisements never reach the nodes, so RA monitoring cannot work there.

DHCPv6-PD is the answer: it is a solicited exchange with a server, and DHCPv6
relays pass through RA Guard untouched. Configure `acquisition.dhcpv6pd` alone
and leave `routerAdvertisement` out entirely — a spec with only DHCPv6-PD is
valid, and the operator will not open an NDP socket at all.

Where the switch cannot filter, the operator can apply its own trust rules
instead. See "Naming the routers you believe" in the README: it takes a list of
link-local router addresses and a plausible range of prefix lengths, and reports
what it drops.

---

## Further Reading

- [IPv6 Prefix Delegation (RFC 8415)](https://datatracker.ietf.org/doc/html/rfc8415)
- [SLAAC (RFC 4862)](https://datatracker.ietf.org/doc/html/rfc4862)
