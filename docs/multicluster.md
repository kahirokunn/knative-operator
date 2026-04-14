# Multi-Cluster Deployment

## Hub and Spoke Architecture

The Knative Operator supports deploying Knative Serving and Eventing components
to remote ("spoke") clusters from a single management ("hub") cluster.

- The operator runs on the **hub cluster**. Each `KnativeServing` or
  `KnativeEventing` CR that carries a `spec.clusterProfileRef` causes the
  operator to reconcile the component on the referenced spoke cluster instead
  of the local cluster.
- The hub must have **network access to the spoke API servers**. The connection
  details are resolved through the Cluster Inventory API (`ClusterProfile`).
  If direct hub→spoke connectivity is not available (e.g. spokes behind NAT or
  in private networks), the direction can be reversed by using
  [OCM Cluster Proxy](https://open-cluster-management.io/docs/getting-started/integration/cluster-proxy/),
  which tunnels hub requests through an agent running on the spoke.

## Usage: `spec.clusterProfileRef`

Set `spec.clusterProfileRef` on a `KnativeServing` or `KnativeEventing` CR to
target a remote cluster. The field references a `ClusterProfile` resource from
the [Cluster Inventory API](https://github.com/kubernetes-sigs/cluster-inventory-api)
(`sigs.k8s.io/cluster-inventory-api`).

```yaml
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: knative-serving
  namespace: knative-serving
spec:
  clusterProfileRef:
    name: spoke-cluster-1
    namespace: fleet-system
```

When `clusterProfileRef` is set:

1. The operator resolves the `ClusterProfile` and builds a `rest.Config` for the
   spoke cluster using the configured access provider.
2. All manifest operations (apply, delete) target the spoke cluster.
3. Status conditions include `TargetClusterResolved` to indicate whether the
   remote cluster was successfully reached.

When `clusterProfileRef` is omitted, the operator behaves as before and
reconciles on the local cluster.

### Operator flags

The `--clusterprofile-provider-file` flag must be set on the operator binary,
pointing to an access provider config JSON file (as defined by
`sigs.k8s.io/cluster-inventory-api/pkg/access`). Without this flag, any CR with
a `clusterProfileRef` will fail to reconcile.

#### Helm chart

Enable multi-cluster in `values.yaml`:

```yaml
knative_operator:
  multicluster:
    enabled: true
    accessProvidersConfig:
      providers:
        - name: token-secretreader
          execConfig:
            apiVersion: client.authentication.k8s.io/v1
            command: /access-plugins/token-secretreader/kubeconfig-secretreader-plugin
            provideClusterInfo: true
    plugins:
      - name: token-secretreader
        image: ghcr.io/example/plugin:v1.0.0
        mountPath: /access-plugins/token-secretreader
```

When `multicluster.enabled` is `true`, the chart creates a `ConfigMap` named
`clusterprofile-provider-file` and mounts it at
`/etc/cluster-inventory/config.json`. Each entry in `plugins` is mounted as a
Kubernetes `image` volume so the access plugin binary is available inside the
operator pod.

## Anchor ConfigMap

When deploying to a remote cluster, the operator creates an **anchor ConfigMap**
on that cluster. It serves as an `OwnerReference` target for namespace-scoped
resources so that Kubernetes garbage collection keeps the remote resources in
sync with the operator's intent.

- Name format: `{kind}-{cr-name}-root-owner`
  (e.g. `knativeserving-knative-serving-root-owner`)
- Created in the CR's namespace on the remote cluster.
- Labels: `app.kubernetes.io/managed-by: knative-operator`,
  `operator.knative.dev/cr-name: <name>`
- Annotation: `operator.knative.dev/anchor: "true"`

> **Warning:** Manually deleting the anchor ConfigMap triggers Kubernetes garbage
> collection of all managed namespace-scoped resources on that remote cluster.

On CR deletion the operator explicitly deletes the anchor ConfigMap and
cluster-scoped resources via the `FinalizeRemoteCluster` function.

## Architecture

The operator uses the `knative.dev/pkg` informer framework with a custom
`ClusterProvider` type that resolves `ClusterProfile` references into cached
client sets:

```
ClusterProvider.Get(ctx, "namespace/name") -> RemoteClusterClients
                                                |- MfClient()    (manifestival)
                                                |- KubeClient()  (kubernetes.Interface)
                                                |- RestConfig()  (*rest.Config)
```

A `ClusterProfile` informer watches for changes and calls
`ClusterProvider.Refresh` to keep the cache up to date. The
`ResolveTargetCluster` reconciliation stage swaps the manifest's `Client` to the
remote one and provisions the anchor ConfigMap.
