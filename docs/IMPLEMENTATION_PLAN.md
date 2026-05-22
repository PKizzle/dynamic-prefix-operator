# Implementation Plan: Dynamic Prefix Operator

## Executive Summary

This document outlines the implementation plan for the Dynamic Prefix Operator, a Kubernetes operator that manages dynamic IPv6 prefix delegation for bare-metal and home/SOHO clusters. The operator will be built using Go with kubebuilder, following Kubernetes operator best practices.

## Technology Stack

### Core Framework

| Component | Technology | Justification |
|-----------|------------|---------------|
| Language | Go 1.22+ | Standard for Kubernetes operators, excellent concurrency |
| Operator Framework | [Kubebuilder](https://kubebuilder.io/) | Industry standard, generates boilerplate, CRD scaffolding |
| Controller Runtime | [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime) | Kubernetes SIG project, powers kubebuilder and Operator SDK |
| Kubernetes Client | client-go | Official Kubernetes Go client |

### Networking Libraries

| Component | Library | Justification |
|-----------|---------|---------------|
| DHCPv6-PD | [insomniacslk/dhcp](https://github.com/insomniacslk/dhcp) | Most maintained DHCPv6 library in Go (261+ imports), full PD support, BSD-3 license |
| NDP/RA | [mdlayher/ndp](https://github.com/mdlayher/ndp) | Production-proven (MetalLB, DigitalOcean), MIT license |

### Observability

| Component | Technology | Justification |
|-----------|------------|---------------|
| Metrics | Prometheus (controller-runtime native) | Standard for Kubernetes operators |
| Logging | zap (controller-runtime native) | Structured logging, JSON output |
| Tracing | OpenTelemetry (optional) | Distributed tracing for debugging |

## Project Structure

Following [Go project layout best practices](https://github.com/golang-standards/project-layout) and [kubebuilder conventions](https://kubebuilder.io/reference/good-practices):

```
dynamic-prefix-operator/
├── api/
│   └── v1alpha1/
│       ├── dynamicprefix_types.go      # DynamicPrefix CRD + OpenAPI/CEL markers
│       ├── groupversion_info.go
│       ├── zz_generated.deepcopy.go
│
├── cmd/
│   └── main.go                         # Operator entrypoint
│
├── internal/
│   ├── controller/
│   │   ├── dynamicprefix_controller.go     # Main reconciler
│   │   ├── poolsync_controller.go          # Watches annotated pools
│   │   ├── poolsync_backends.go            # Cilium, MetalLB, Calico pool backends
│   │   ├── servicesync_controller.go       # HA Service management
│   │   ├── bgpsync_controller.go           # Cilium BGP advertisement sync
│   │   ├── metrics.go                      # Prometheus collectors
│   │   ├── events.go                       # Kubernetes event helpers
│   │   ├── suite_test.go                   # envtest setup
│   │   └── dynamicprefix_controller_test.go
│   │
│   ├── prefix/
│   │   ├── types.go              # Interface for prefix reception
│   │   ├── factory.go            # Receiver factory
│   │   ├── dhcpv6_receiver.go    # DHCPv6-PD client implementation
│   │   ├── ra_receiver.go        # Router Advertisement monitor
│   │   └── addressrange.go       # Address range/subnet calculation
│   │
│   └── integration/              # ISP simulation scenarios
│
├── config/
│   ├── crd/
│   │   └── bases/                # Generated CRD YAML
│   ├── rbac/
│   │   ├── role.yaml
│   │   └── role_binding.yaml
│   ├── manager/
│   │   ├── manager.yaml          # Deployment manifest
│   │   └── kustomization.yaml
│   ├── samples/
│   │   ├── dynamicprefix.yaml
│   │   └── pools_with_annotations.yaml
│   └── default/
│       └── kustomization.yaml
│
├── charts/
│   └── dynamic-prefix-operator/  # Helm chart
│
├── hack/
│   └── boilerplate.go.txt
│
├── docs/
│
├── Makefile
├── Dockerfile
├── go.mod
└── README.md
```

## Binding Model: Annotation-Based (1Password Pattern)

Instead of a separate binding CRD, pools reference the DynamicPrefix via annotations:

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: ipv6-pool
  annotations:
    dynamic-prefix.io/name: home-ipv6       # Which DynamicPrefix
    dynamic-prefix.io/subnet: loadbalancers # Which subnet
spec:
  blocks: []  # Managed by operator
```

**Why this approach:**
- Simpler than explicit binding resources
- Follows established patterns (1Password Operator, external-secrets)
- Pools are self-documenting
- No orphaned bindings to clean up

**Controller behavior:**
1. Watch for pools with `dynamic-prefix.io/name` annotation
2. Look up the referenced DynamicPrefix
3. Calculate the subnet CIDR
4. Update the pool's spec with the current CIDR
5. Re-reconcile when DynamicPrefix changes

## Implementation Phases

### Phase 1: Project Scaffolding & Core CRD (Week 1)

**Objective**: Set up the project structure and define the DynamicPrefix CRD.

#### Tasks

1. **Initialize kubebuilder project**
   ```bash
   kubebuilder init --domain dynamic-prefix.io --repo github.com/pkizzle/dynamic-prefix-operator
   ```

2. **Create DynamicPrefix CRD**
   ```bash
   kubebuilder create api --group "" --version v1alpha1 --kind DynamicPrefix --resource --controller
   ```

3. **Implement CRD types** with proper spec/status separation

4. **Add CEL/OpenAPI validation rules**

5. **Generate manifests**

#### DynamicPrefix CRD

```go
// api/v1alpha1/dynamicprefix_types.go

type DynamicPrefixSpec struct {
    // Acquisition defines how to receive the prefix
    Acquisition AcquisitionSpec `json:"acquisition"`

    // Subnets defines how to subdivide the received prefix
    Subnets []SubnetSpec `json:"subnets,omitempty"`

    // Transition defines graceful transition settings
    Transition TransitionSpec `json:"transition,omitempty"`
}

type AcquisitionSpec struct {
    // DHCPv6PD configures DHCPv6 Prefix Delegation
    DHCPv6PD *DHCPv6PDSpec `json:"dhcpv6pd,omitempty"`

    // RouterAdvertisement configures RA monitoring
    RouterAdvertisement *RASpec `json:"routerAdvertisement,omitempty"`
}

type DHCPv6PDSpec struct {
    // Interface to receive delegated prefix on
    Interface string `json:"interface"`

    // RequestedPrefixLength hints the desired prefix length
    RequestedPrefixLength *int `json:"requestedPrefixLength,omitempty"`
}

type SubnetSpec struct {
    // Name identifies this subnet
    Name string `json:"name"`

    // Offset selects the Nth subnet of PrefixLength within the received prefix
    Offset int64 `json:"offset"`

    // PrefixLength of the subnet
    PrefixLength int `json:"prefixLength"`
}

type DynamicPrefixStatus struct {
    // CurrentPrefix is the currently active prefix
    CurrentPrefix string `json:"currentPrefix,omitempty"`

    // PrefixSource indicates how the prefix was obtained
    PrefixSource string `json:"prefixSource,omitempty"`

    // LeaseExpiresAt indicates when the DHCPv6 lease expires
    LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

    // Subnets contains calculated subnet CIDRs
    Subnets []SubnetStatus `json:"subnets,omitempty"`

    // Conditions represent the current state
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

#### Deliverables
- [x] Compilable project structure
- [x] DynamicPrefix CRD with OpenAPI and CEL validation
- [x] DynamicPrefix, PoolSync, ServiceSync, and BGPSync controllers
- [x] CI pipeline (GitHub Actions)

### Phase 2: DHCPv6-PD Client (Week 2)

**Objective**: Implement the DHCPv6 Prefix Delegation client.

#### Architecture

```go
// internal/prefix/receiver.go
type Receiver interface {
    // Start begins receiving prefixes
    Start(ctx context.Context) error

    // Events returns a channel of prefix events
    Events() <-chan Event

    // CurrentPrefix returns the current prefix if any
    CurrentPrefix() *Prefix

    // Stop stops receiving
    Stop() error
}

type Prefix struct {
    Network           netip.Prefix
    ValidLifetime     time.Duration
    PreferredLifetime time.Duration
    Source            PrefixSource
    ReceivedAt        time.Time
}
```

#### DHCPv6-PD Implementation

```go
// internal/prefix/dhcpv6/client.go
type Client struct {
    iface        string
    prefixLen    uint8
    conn         net.PacketConn
    currentLease *Lease
    prefixes     chan PrefixEvent
}

func (c *Client) Start(ctx context.Context) error {
    // 1. Create DHCPv6 client
    // 2. Send SOLICIT with IA_PD
    // 3. Handle ADVERTISE/REPLY
    // 4. Start lease renewal goroutine
    // 5. Emit prefix events on changes
}
```

#### Tasks

1. **Implement DHCPv6-PD client** using `insomniacslk/dhcp`
2. **Implement lease management** (T1/T2 timers, RENEW, REBIND)
3. **Add integration tests** with mock DHCPv6 server

### Phase 3: Router Advertisement Monitor (Week 2-3)

**Objective**: Implement RA monitoring as fallback.

```go
// internal/prefix/ra/monitor.go
type Monitor struct {
    iface    string
    conn     *ndp.Conn
    prefixes chan PrefixEvent
}

func (m *Monitor) Start(ctx context.Context) error {
    // Listen for RAs, parse PIOs, emit prefix events
}
```

### Phase 4: DynamicPrefix Controller (Week 3)

**Objective**: Main controller that manages DynamicPrefix resources.

#### Reconciliation Flow

```
1. Fetch DynamicPrefix
2. Ensure prefix receiver is running
3. Get current prefix
4. Calculate subnets
5. Update status
6. If prefix changed, trigger pool reconciliation
7. Requeue before lease expires
```

```go
func (r *DynamicPrefixReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var dp v1alpha1.DynamicPrefix
    if err := r.Get(ctx, req.NamespacedName, &dp); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Get or create receiver
    receiver, err := r.receiverManager.GetOrCreate(dp.Spec.Acquisition)

    // Get current prefix
    prefix := receiver.CurrentPrefix()

    // Calculate subnets
    subnets := calculateSubnets(prefix.Network, dp.Spec.Subnets)

    // Update status
    dp.Status.CurrentPrefix = prefix.Network.String()
    dp.Status.Subnets = subnets

    // If prefix changed, find and update referencing pools
    if prefixChanged {
        r.reconcileReferencingPools(ctx, &dp)
    }

    return ctrl.Result{RequeueAfter: renewBefore}, nil
}
```

### Phase 5: Pool Controller (Week 4)

**Objective**: Watch for annotated pools and keep them in sync.

#### Controller Design

```go
// internal/controller/poolsync_controller.go
type PoolSyncReconciler struct {
    client.Client
    BackendGVKs []schema.GroupVersionKind
}

func (r *PoolSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Fetch the pool from the discovered backend set
    // 2. Check for dynamic-prefix.io/name annotation
    // 3. Look up the referenced DynamicPrefix
    // 4. Resolve address-range, subnet, or raw-prefix configuration
    // 5. Update backend-specific pool fields idempotently
}

// Watch multiple pool types
func (r *PoolSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
  For(primaryBackendObject).
  Watches(additionalBackendObjects...).
        Watches(&v1alpha1.DynamicPrefix{}, handler.EnqueueRequestsFromMapFunc(r.findPoolsForPrefix)).
        Complete(r)
}
```

#### Pool Handlers

```go
// internal/controller/poolsync_backends.go
type poolBackend interface {
    name() string
    gvk() schema.GroupVersionKind
    update(ctx context.Context, r *PoolSyncReconciler, pool *unstructured.Unstructured, configs []poolConfiguration, managedPrefixes []netip.Prefix) (bool, error)
}
```

Implemented backends currently include Cilium `CiliumLoadBalancerIPPool`, Cilium `CiliumCIDRGroup`, MetalLB `IPAddressPool`, and Calico `IPPool`.

### Phase 6: Graceful Transitions (Week 5)

**Objective**: Handle prefix changes without disruption.

Graceful transitions are implemented in controller status and reconciliation rather than a separate transition package:

1. `DynamicPrefixReconciler` adds the previous prefix to `status.history` when a new prefix is acquired.
2. `PoolSyncReconciler` renders current + historical configurations into supported pools, preserving unmanaged entries where possible.
3. `ServiceSyncReconciler` implements HA mode by assigning current + historical Service IPs while targeting DNS at the current prefix.
4. `maxPrefixHistory` controls when older history entries are pruned.
5. Kubernetes events announce transition start and completion/pruning.

### Phase 7: Observability (Week 6)

#### Metrics

```go
var (
    prefixReceived = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "dynamic_prefix_received_total",
            Help: "Total prefixes received",
        },
        []string{"name", "source"},
    )

    prefixChanges = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "dynamic_prefix_changes_total",
        },
        []string{"name"},
    )

    leaseExpiry = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "dynamic_prefix_lease_expiry_seconds",
        },
        []string{"name"},
    )

    poolsSynced = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "dynamic_prefix_pools_synced",
        },
        []string{"backend", "dynamic_prefix", "pool"},
    )
)
```

#### Events

- `PrefixReceived` - New prefix obtained
- `PrefixChanged` - Prefix changed
- `PoolUpdated` - Pool CIDR updated
- `TransitionStarted` / `TransitionCompleted`

### Phase 8: Deployment & Release (Week 7)

- Helm chart
- Kustomize configurations
- GitHub Actions for releases
- Container image publishing

## CRD Specification

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: dynamicprefixes.dynamic-prefix.io
spec:
  group: dynamic-prefix.io
  names:
    kind: DynamicPrefix
    listKind: DynamicPrefixList
    plural: dynamicprefixes
    singular: dynamicprefix
    shortNames: [dp, dprefix]
  scope: Cluster
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - name: Prefix
          type: string
          jsonPath: .status.currentPrefix
        - name: Source
          type: string
          jsonPath: .status.prefixSource
        - name: Age
          type: date
          jsonPath: .metadata.creationTimestamp
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              required: [acquisition]
              properties:
                acquisition:
                  type: object
                  x-kubernetes-validations:
                    - rule: has(self.dhcpv6pd) || has(self.routerAdvertisement)
                      message: at least one acquisition method must be configured
                  properties:
                    dhcpv6pd:
                      type: object
                      required: [interface]
                      properties:
                        interface:
                          type: string
                        requestedPrefixLength:
                          type: integer
                          minimum: 48
                          maximum: 64
                    routerAdvertisement:
                      type: object
                      properties:
                        interface:
                          type: string
                        enabled:
                          type: boolean
                addressRanges:
                  type: array
                  x-kubernetes-list-type: map
                  x-kubernetes-list-map-keys: [name]
                  items:
                    type: object
                    required: [name, start, end]
                    properties:
                      name:
                        type: string
                      start:
                        type: string
                      end:
                        type: string
                subnets:
                  type: array
                  x-kubernetes-list-type: map
                  x-kubernetes-list-map-keys: [name]
                  items:
                    type: object
                    required: [name, prefixLength]
                    properties:
                      name:
                        type: string
                      offset:
                        type: integer
                        minimum: 0
                      prefixLength:
                        type: integer
                        minimum: 48
                        maximum: 128
                transition:
                  type: object
                  properties:
                    mode:
                      type: string
                      enum: [simple, ha]
                      default: simple
                    maxPrefixHistory:
                      type: integer
                      default: 2
                      minimum: 1
                      maximum: 10
            status:
              type: object
              properties:
                currentPrefix:
                  type: string
                prefixSource:
                  type: string
                leaseExpiresAt:
                  type: string
                  format: date-time
                subnets:
                  type: array
                  items:
                    type: object
                    properties:
                      name:
                        type: string
                      cidr:
                        type: string
                conditions:
                  type: array
                  items:
                    type: object
                    properties:
                      type:
                        type: string
                      status:
                        type: string
                      lastTransitionTime:
                        type: string
                        format: date-time
                      reason:
                        type: string
                      message:
                        type: string
```

## RBAC

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: dynamic-prefix-operator
rules:
  # Own CRDs
  - apiGroups: ["dynamic-prefix.io"]
    resources: ["dynamicprefixes"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["dynamic-prefix.io"]
    resources: ["dynamicprefixes/status"]
    verbs: ["get", "update", "patch"]

  # Pool backend resources (update annotated pools)
  - apiGroups: ["cilium.io"]
    resources: ["ciliumloadbalancerippools", "ciliumcidrgroups"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["metallb.io"]
    resources: ["ipaddresspools"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["projectcalico.org"]
    resources: ["ippools"]
    verbs: ["get", "list", "watch", "update", "patch"]

  # Events
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]

  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

## Security

- Requires `CAP_NET_RAW` for DHCPv6/NDP sockets
- Host network for interface binding
- Non-root where possible
- Minimal RBAC permissions

## References

- [Kubebuilder](https://kubebuilder.io/)
- [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [insomniacslk/dhcp](https://github.com/insomniacslk/dhcp)
- [mdlayher/ndp](https://github.com/mdlayher/ndp)
- [1Password Operator](https://github.com/1Password/onepassword-operator) - Binding pattern inspiration
- [Cilium LB-IPAM](https://docs.cilium.io/en/stable/network/lb-ipam/)
