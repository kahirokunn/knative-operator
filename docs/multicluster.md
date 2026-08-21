# Multi-Cluster Deployment

The operator can deploy Knative Serving and Eventing to remote clusters from a
single hub cluster. Platform administrators use `spec.placement` to choose both
the spoke cluster and the namespace where Knative is installed.

> **Warning: Alpha API.** Multi-cluster placement can change without backward
> compatibility. Do not use it for production workloads yet.

The hub needs network access to each spoke API server. Connection details are
resolved through the Cluster Inventory API (`ClusterProfile`).

> Note: if direct connectivity is not available, reverse the direction with
> [OCM Cluster Proxy](https://open-cluster-management.io/docs/getting-started/integration/cluster-proxy/).

## Prerequisites

- **Kubernetes 1.35+** on the hub cluster (image volumes must be available).
- The **Cluster Inventory API** CRD (`ClusterProfile`) installed on the hub.
- Network connectivity from the operator pod to each spoke API server.

## Usage

Create the management CR on the hub and set `spec.placement` to the spoke
destination. The hub identity and spoke installation namespace can be
different:

```yaml
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: serving-spoke-tokyo
  namespace: fleet-workloads
spec:
  placement:
    clusterProfileRef:
      name: spoke-tokyo
      namespace: fleet-system
    namespace: knative-serving
```

| Field | Cluster | What it identifies | Example |
|-------|---------|--------------------|---------|
| `metadata.namespace` / `metadata.name` | Hub | The management CR watched by the operator | `fleet-workloads/serving-spoke-tokyo` |
| `spec.placement.clusterProfileRef` | Hub | The `ClusterProfile` used to connect to the spoke | `fleet-system/spoke-tokyo` |
| `spec.placement.namespace` | Spoke | The namespace receiving Knative resources and the anchor ConfigMap | `knative-serving` |

`placement.clusterProfileRef.name`, `placement.clusterProfileRef.namespace`,
and `placement.namespace` are all required. For a resource that uses only
`spec.placement`, the whole `placement` object is immutable: delete and recreate
the management CR to change its destination. During a legacy migration, correct
`placement` while `spec.clusterProfileRef` remains set.

Only one `KnativeServing` and one `KnativeEventing` may target a given remote
`ClusterProfile`. A second resource of the same kind is rejected even when it
selects a different installation namespace, because the upstream installation
also owns fixed-name cluster-scoped resources. Serving and Eventing may target
the same remote cluster together.

When neither `spec.placement` nor the deprecated `spec.clusterProfileRef` is
set, the installation is local and uses the management CR's
`metadata.namespace`. `spec.placement` always means a remote installation; it
is not an interface for installing into another namespace on the hub.

The operator does not create a `KnativeServing` or `KnativeEventing` CR on the
spoke. It renders the release manifests from the hub CR and applies them
directly to `spec.placement.namespace`, so the management CR name does not
rename the upstream Knative resources.

The operator resolves the `ClusterProfile`, builds a `rest.Config` via the
configured access provider, and applies manifests on the spoke. A
`TargetClusterResolved` status condition tracks whether the remote cluster was
reached.

`--clusterprofile-provider-file` must point to an access provider config JSON
file (`sigs.k8s.io/cluster-inventory-api/pkg/access`); without it, any remote CR
will fail to reconcile.

## Compatibility with the old Alpha field

The top-level `spec.clusterProfileRef` field is deprecated but remains supported
for two minor releases. Use `spec.placement` for new resources.

The old field did not have a separate target namespace. A legacy resource such
as this installs Serving into `knative-serving` on `spoke-tokyo`, because its
hub namespace is also `knative-serving`:

```yaml
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: knative-serving
  namespace: knative-serving
spec:
  clusterProfileRef:
    name: spoke-tokyo
    namespace: fleet-system
```

The operator translates that legacy configuration in memory as follows. It
does not rewrite the resource spec.

| Legacy value | Effective placement value |
|--------------|---------------------------|
| `spec.clusterProfileRef` | `spec.placement.clusterProfileRef` |
| `metadata.namespace` | `spec.placement.namespace` |

### Migrate an existing resource

Migration uses two updates so the resource can never lose its remote-cluster
selector. First, add a placement that describes the same destination while
keeping the old field:

```bash
kubectl patch knativeserving knative-serving \
  --namespace knative-serving \
  --type merge \
  --patch '{"spec":{"placement":{"clusterProfileRef":{"name":"spoke-tokyo","namespace":"fleet-system"},"namespace":"knative-serving"}}}'
```

The two cluster references must match, and `placement.namespace` must equal
the management CR's current `metadata.namespace`. Admission rejects either
mismatch before the operator changes any spoke resources. Correct a rejected
placement while `spec.clusterProfileRef` is still present; placement becomes
immutable after the deprecated field is removed.

Wait for the operator to accept and resolve the placement:

```bash
target_generation="$(kubectl get knativeserving knative-serving \
  --namespace knative-serving \
  --output jsonpath='{.metadata.generation}')"
kubectl wait knativeserving/knative-serving \
  --namespace knative-serving \
  --for="jsonpath={.status.observedGeneration}=${target_generation}" \
  --timeout=60s
kubectl wait knativeserving/knative-serving \
  --namespace knative-serving \
  --for=condition=TargetClusterResolved=True \
  --timeout=60s
```

Then remove the deprecated field:

```bash
kubectl patch knativeserving knative-serving \
  --namespace knative-serving \
  --type merge \
  --patch '{"spec":{"clusterProfileRef":null}}'
```

Use the same sequence with `knativeeventing` for Eventing. Validation rejects
an atomic replacement of `clusterProfileRef` with `placement`, removing
`placement` after it has been added, or adding the deprecated field to an
existing resource.

This in-place migration preserves the existing hub namespace and spoke
namespace. To move the management CR to `fleet-workloads` while keeping the
spoke installation in `knative-serving`, delete the old CR, wait for remote
cleanup, and create the new placement-based CR shown in [Usage](#usage).

## Helm chart

The chart supports one Knative Operator release per Kubernetes cluster. It owns
cluster-scoped CRDs, RBAC objects, and webhook configurations, so do not install
the chart into multiple namespaces in the same cluster.

Enable multi-cluster in `values.yaml`:

```yaml
knative_operator:
  multicluster:
    enabled: true
    accessProvidersConfig:
      providers:
        - name: secretreader
          execConfig:
            apiVersion: client.authentication.k8s.io/v1
            command: /access-plugins/secretreader/bin/secretreader-plugin
            interactiveMode: Never
            provideClusterInfo: true
        - name: kubeconfig-secretreader
          execConfig:
            apiVersion: client.authentication.k8s.io/v1
            command: /access-plugins/kubeconfig-secretreader/bin/kubeconfig-secretreader-plugin
            interactiveMode: Never
            provideClusterInfo: true
    plugins:
      - name: secretreader
        image: registry.k8s.io/cluster-inventory-api/secretreader:v0.1.3
        mountPath: /access-plugins/secretreader
      - name: kubeconfig-secretreader
        image: registry.k8s.io/cluster-inventory-api/kubeconfig-secretreader:v0.1.3
        mountPath: /access-plugins/kubeconfig-secretreader
```

The chart creates a `ConfigMap` with the provider config and mounts each
plugin as a Kubernetes image volume inside the operator pod. Each
`plugins[].mountPath` must be absolute, and each `execConfig.command` must
point under a plugin mount path, not at the mount directory itself.

## Namespace configuration

`spec.namespace.labels` and `spec.namespace.annotations` are applied to the
effective installation namespace. For the example above, that is the
`knative-serving` namespace on `spoke-tokyo`, including when the namespace
already exists.

## Anchor ConfigMap

For remote deployments, the operator creates an anchor ConfigMap
(`{kind}-{cr-name}-root-owner`) on the spoke. Namespace-scoped resources use
it as their `OwnerReference`, so deleting the anchor triggers GC of all owned
resources. Cluster-scoped resources are not owned by the anchor and are
cleaned up by `FinalizeRemoteCluster` when the hub CR is deleted.

The anchor carries an `operator.knative.dev/protected=true` annotation and a
description annotation warning against manual deletion. To uninstall safely,
delete the corresponding CR on the hub.

After the installed Deployments become available, the operator adds the anchor
as controller owner of their runtime-created leader-election Leases and any
same-named Services. It never replaces a different controller owner. Before
removing the anchor, the finalizer also explicitly deletes matching Services,
EndpointSlices, and Leases as a fallback for resources created before ownership
was adopted. This allows cleanup to find an adopted Lease even after its holder
identity has been cleared.

In addition to applying the release manifests, the resolved spoke credentials
must allow `get`, `list`, `update`, and `delete` on Leases; `get`, `update`, and
`delete` on Services; and `list` and `delete` on EndpointSlices in the effective
installation namespace.

## Remote deployments poll interval

While spoke deployments roll out, the operator requeues the CR to re-check
readiness. The interval is controlled by `--remote-deployments-poll-interval`
(default `10s`); values below `1s` fall back to the default. The effective
value is logged at operator startup.

Larger values reduce reconcile traffic on hubs managing many spokes, at the
cost of slower observability of readiness transitions:

| Spoke count | Recommended interval |
|-------------|----------------------|
| < 10 | `10s` (default) |
| 10-100 | `30s` |
| > 100 | `60s` |

### Setting the interval

```yaml
knative_operator:
  multicluster:
    enabled: true
    remoteDeploymentsPollInterval: 30s
```

## Troubleshooting

Check the status condition on the CR:

```bash
kubectl get knativeserving -n <ns> <name> -o jsonpath='{.status.conditions[?(@.type=="TargetClusterResolved")]}'
```

Common reasons for `TargetClusterResolved=False`:

- **InvalidPlacement**: `spec.clusterProfileRef` and `spec.placement` are both
  present during migration, but `spec.placement.namespace` is not the
  management CR's own namespace.
- **ClusterProfileNotFound**: the referenced `ClusterProfile` does not exist.
  Check `spec.placement.clusterProfileRef`, or the deprecated
  `spec.clusterProfileRef` on a legacy CR.
- **ClusterProfileNotReady**: `ClusterProfile` exists but is unhealthy.
  Inspect `kubectl get clusterprofile -n <ns> <name> -o yaml`.
- **ClusterProfileUnavailable**: the hub API request failed, or the
  `ClusterProfile` changed while its clients were being refreshed. The latter
  is transient and the queued reconcile builds clients from the new profile.
- **AccessProviderFailed**: the configured access provider could not build a
  client configuration, including when no provider matches the
  `ClusterProfile`. Check the provider configuration and operator logs.
- **AccessProviderNotConfigured**: the reconciler was started without a cluster
  provider. This indicates an operator wiring error; the standard deployment
  always configures one.
- **MulticlusterDisabled**: `--clusterprofile-provider-file` is not set on the
  operator Deployment. Set the flag and mount a provider configuration file.
- **RemoteClientCreationFailed**: the returned `rest.Config` could not be used
  to construct Kubernetes clients. Remote network failures after construction
  are reported by the install operation instead.
- **RemoteClusterStale**: the cached client context was already cancelled. A
  normal `ClusterProfile` update evicts the entry and the next reconcile builds
  a new client.
- **ClusterProviderClosed**: operator is shutting down; the next leader
  re-reconciles and recovers.

If spoke deployments are not coming up, confirm `TargetClusterResolved=True`,
check the operator logs on the hub, and inspect the spoke cluster directly with
`kubectl --kubeconfig=<spoke> get deployments -n <ns>`.
