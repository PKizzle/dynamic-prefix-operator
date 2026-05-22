# dynamic-prefix-operator

A Helm chart for deploying the Dynamic Prefix Operator - a Kubernetes operator that manages dynamic IPv6 prefix delegation.

## Prerequisites

- Kubernetes 1.26+
- Helm 3.0+
- At least one supported pool backend CRD: Cilium, MetalLB, or Calico

## Installation

```bash
# Add the repository
helm repo add dynamic-prefix-operator https://pkizzle.github.io/dynamic-prefix-operator
helm repo update

# Install the chart
helm install dynamic-prefix-operator dynamic-prefix-operator/dynamic-prefix-operator

# Or install from OCI
helm install dynamic-prefix-operator oci://ghcr.io/pkizzle/dynamic-prefix-operator/helm/dynamic-prefix-operator

# Or install from local directory
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator
```

## Configuration

### Watch Configuration

Control which resources the operator watches and manages:

```yaml
watch:
  # Namespaces to watch (empty = all namespaces)
  namespaces: []

  # CiliumLoadBalancerIPPool settings
  ciliumLoadBalancerIPPool:
    enabled: true
    labelSelector:
      app.kubernetes.io/managed-by: dynamic-prefix-operator
    annotationSelector: {}

  # CiliumCIDRGroup settings
  ciliumCIDRGroup:
    enabled: true
    labelSelector: {}
    annotationSelector: {}

  # Ingress settings (for future use)
  ingress:
    enabled: false
    ingressClassName: nginx
    labelSelector:
      dynamic-prefix.io/enabled: "true"

  # Service settings (for future use)
  service:
    enabled: false
    types:
      - LoadBalancer
    labelSelector: {}
```

### Common Configuration Examples

#### Watch only specific namespaces

```bash
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator \
  --set 'watch.namespaces={production,staging}'
```

#### Filter pools by label

```bash
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator \
  --set 'watch.ciliumLoadBalancerIPPool.labelSelector.environment=production'
```

#### Enable Prometheus monitoring

```bash
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator \
  --set serviceMonitor.enabled=true
```

#### Limit the Service informer cache

For large clusters, label HA-managed Services and restrict the Service informer
cache to those opt-in Services:

```bash
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator \
  --set 'config.serviceSync.cacheLabelSelector=dynamic-prefix.io/name'
```

Services still use the `dynamic-prefix.io/name` annotation for configuration;
the matching label is only used to narrow the informer cache.

#### High availability setup

Leader election is enabled by default, but the chart keeps `replicaCount: 1`
for low-footprint installs. Set `replicaCount` to at least `2` if you want a
warm standby replica that can take over automatically.

Non-leader replicas intentionally still expose health probes and metrics while
they wait for the lease; controllers become active only on the elected leader.

```bash
helm install dynamic-prefix-operator ./charts/dynamic-prefix-operator \
  --set replicaCount=2 \
  --set podDisruptionBudget.enabled=true \
  --set config.leaderElection.enabled=true
```

## Parameters

### General

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas (`2+` enables warm-standby HA) | `1` |
| `image.repository` | Image repository | `ghcr.io/pkizzle/dynamic-prefix-operator` |
| `image.tag` | Image tag | Chart appVersion |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |

### Watch Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `watch.namespaces` | Namespaces to watch | `[]` (all) |
| `watch.ciliumLoadBalancerIPPool.enabled` | Watch CiliumLoadBalancerIPPool | `true` |
| `watch.ciliumLoadBalancerIPPool.labelSelector` | Label selector for pools | `{}` |
| `watch.ciliumCIDRGroup.enabled` | Watch CiliumCIDRGroup | `true` |
| `watch.ingress.enabled` | Watch Ingress resources | `false` |
| `watch.ingress.ingressClassName` | Filter by ingress class | `""` |
| `watch.service.enabled` | Watch Service resources | `false` |
| `watch.service.types` | Service types to watch | `[LoadBalancer]` |

### Operator Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.logLevel` | Log level | `info` |
| `config.leaderElection.enabled` | Enable leader election for multi-replica deployments | `true` |
| `config.metrics.enabled` | Enable metrics endpoint | `true` |
| `config.serviceSync.cacheLabelSelector` | Optional label selector for the Service informer cache | `""` |

### Monitoring

| Parameter | Description | Default |
|-----------|-------------|---------|
| `serviceMonitor.enabled` | Create ServiceMonitor | `false` |
| `serviceMonitor.interval` | Scrape interval | `30s` |
| `networkPolicy.enabled` | Create NetworkPolicy | `false` |
| `podDisruptionBudget.enabled` | Create PDB | `false` |

### Security

| Parameter | Description | Default |
|-----------|-------------|---------|
| `rbac.create` | Create RBAC resources | `true` |
| `serviceAccount.create` | Create ServiceAccount | `true` |
| `podSecurityContext.runAsNonRoot` | Run as non-root | `true` |

## Usage

After installation, create a DynamicPrefix resource:

```yaml
apiVersion: dynamic-prefix.io/v1alpha1
kind: DynamicPrefix
metadata:
  name: home-ipv6
spec:
  acquisition:
    dhcpv6pd:
      interface: eth0
  subnets:
    - name: loadbalancers
      offset: 0
      prefixLength: 64
```

Then annotate resources you want the operator to manage:

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: ipv6-pool
  annotations:
    dynamic-prefix.io/name: home-ipv6
    dynamic-prefix.io/subnet: loadbalancers
spec:
  blocks: []  # Managed by operator
```

## Upgrading

```bash
helm upgrade dynamic-prefix-operator dynamic-prefix-operator/dynamic-prefix-operator
```

## Uninstalling

```bash
helm uninstall dynamic-prefix-operator
```

Note: CRDs are not removed by default. To remove them:

```bash
kubectl delete crd dynamicprefixes.dynamic-prefix.io
```

## License

Apache License 2.0
