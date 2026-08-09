# Customizing generated resources with patches

Cluster administrators can use `spec.patches` on a `KnativeServing` or
`KnativeEventing` resource when an existing typed override does not cover the
generated Kubernetes resource they need to change. Patches run after the
Operator's built-in transformations, so the patch contains the final decision
for every field it changes.

## Patch target

Each patch must identify exactly one generated resource by API version, kind,
and name. Set `namespace` only when resources in more than one namespace have
the same identity. Reconciliation fails if the target matches zero or multiple
resources; this makes a renamed or removed target visible during an upgrade.

```yaml
spec:
  patches:
  - target:
      apiVersion: autoscaling/v2
      kind: HorizontalPodAutoscaler
      name: webhook
    patch:
      type: strategic
      content: |
        spec:
          minReplicas: 2
          maxReplicas: 10
```

Patches apply to all resources assembled for the component, including Serving
or Eventing core resources, enabled Ingress or Source resources, extension
resources, and `additionalManifests`.

| Patch type | Use it for | Content format |
| --- | --- | --- |
| `strategic` | Kubernetes resources with named lists, such as Deployment containers | Strategic Merge Patch in YAML or JSON |
| `merge` | Maps and scalar fields, including custom resources | RFC 7386 Merge Patch in YAML or JSON |
| `json` | Exact add, replace, or remove operations | RFC 6902 JSON Patch in YAML or JSON |

Strategic Merge Patch requires a Kubernetes type registered in the Operator's
scheme. Use `merge` or `json` for an unregistered custom resource. A patch may
not change the target's API version, kind, name, or namespace.

## Letting KEDA manage replicas

Knative 1.23 includes the following HorizontalPodAutoscalers. An ingress or
broker HPA is present only when its corresponding optional component is
enabled.

| Custom resource | Workload | HPA name |
| --- | --- | --- |
| `KnativeServing` | `activator` | `activator` |
| `KnativeServing` | `webhook` | `webhook` |
| `KnativeServing` with Kourier | `3scale-kourier-gateway` | `3scale-kourier-gateway` |
| `KnativeEventing` | `eventing-webhook` | `eventing-webhook` |
| `KnativeEventing` with MTChannelBasedBroker | `mt-broker-ingress` | `broker-ingress-hpa` |
| `KnativeEventing` with MTChannelBasedBroker | `mt-broker-filter` | `broker-filter-hpa` |

For example, the following `KnativeServing` configuration removes the bundled
`activator` HPA and removes `spec.replicas` from the Deployment manifest. KEDA
can then manage the Deployment through a separately installed `ScaledObject`.

```yaml
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: knative-serving
  namespace: knative-serving
spec:
  patches:
  - target:
      apiVersion: autoscaling/v2
      kind: HorizontalPodAutoscaler
      name: activator
    patch:
      type: strategic
      content: |
        $patch: delete
  - target:
      apiVersion: apps/v1
      kind: Deployment
      name: activator
    patch:
      type: strategic
      content: |
        spec:
          replicas: null
```

The root-level `$patch: delete` directive means the exact HPA identity must
remain absent. Configure the external autoscaler to use another HPA name;
KEDA's default generated HPA name does this. Removing the delete patch causes
the bundled HPA to be created again on the next reconciliation.

When `replicas: null` is present from the first installation, the Operator
never records that field in its applied Deployment manifest. If the patch is
added to an existing installation where the Operator previously set replicas,
the field is released during the transition and Kubernetes or the external
autoscaler may briefly supply a replacement value.

The Operator never automatically deletes CustomResourceDefinitions. A
root-level `$patch: delete` targeting a CRD is rejected.
