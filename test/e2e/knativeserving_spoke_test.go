//go:build e2e
// +build e2e

/*
Copyright 2024 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
	"knative.dev/operator/test"
	"knative.dev/operator/test/client"
	"knative.dev/operator/test/resources"
)

const (
	defaultSpokeClusterProfileName      = "spoke"
	defaultSpokeClusterProfileNamespace = "default"

	spokeWaitInterval = 5 * time.Second
	// spokeReadyTimeout bounds how long we wait for spoke deployments to
	// become Available after the hub CR has reported TargetClusterResolved.
	spokeReadyTimeout = 5 * time.Minute
	// spokeGoneTimeout bounds how long we wait for spoke deployments to be
	// removed after deleting the hub CR.
	spokeGoneTimeout = 3 * time.Minute
	// hubResolveTimeout bounds how long we wait for the hub CR to flip
	// TargetClusterResolved before assuming the operator is wedged.
	hubResolveTimeout = 60 * time.Second
)

// spokeClusterProfileRefName returns the ClusterProfile name to use in the
// KnativeServing CR, allowing the e2e harness to override it via env var.
func spokeClusterProfileRefName() string {
	if v := os.Getenv("SPOKE_CLUSTER_NAME"); v != "" {
		return v
	}
	return defaultSpokeClusterProfileName
}

// spokeClusterProfileRefNamespace returns the ClusterProfile namespace.
func spokeClusterProfileRefNamespace() string {
	if v := os.Getenv("SPOKE_CLUSTER_NAMESPACE"); v != "" {
		return v
	}
	return defaultSpokeClusterProfileNamespace
}

// TestKnativeServingSpokeDeployment verifies that a KnativeServing CR with a
// clusterProfileRef causes the operator to deploy Knative Serving onto the
// spoke cluster instead of the hub cluster.
//
// Preconditions (set up by the e2e harness):
//   - SPOKE_KUBECONFIG points at a kubeconfig usable from inside the hub
//     operator pod (e.g. kind get kubeconfig --internal --name spoke).
//   - A ClusterProfile named "spoke" exists in the "default" namespace on the
//     hub cluster, referencing the spoke apiserver.
func TestKnativeServingSpokeDeployment(t *testing.T) {
	hub := client.Setup(t)
	spoke := client.SetupSpoke(t)

	names := test.ResourceNames{
		KnativeServing: test.OperatorName,
		Namespace:      test.ServingOperatorNamespace,
	}

	// The reconciler is expected to create the target namespace on the spoke,
	// but creating it idempotently up-front avoids a race window during the
	// first reconcile and keeps the test focused on the swap path.
	ensureSpokeNamespace(t, spoke, names.Namespace)

	test.CleanupOnInterrupt(func() { test.TearDown(hub, names) })
	defer test.TearDown(hub, names)

	// Create the KnativeServing CR on the hub with a clusterProfileRef so
	// ResolveTargetCluster swaps the manifest client to point at the spoke.
	if err := createKnativeServingWithSpokeRef(hub, names); err != nil {
		t.Fatalf("Failed to create KnativeServing %q on hub: %v", names.KnativeServing, err)
	}

	t.Run("hub-cr-ready", func(t *testing.T) {
		// Hub-side READY implies TargetClusterResolved=True (it is part of
		// the static ConditionSet for KnativeServing).
		resources.AssertKSOperatorCRReadyStatus(t, hub, names)
	})

	t.Run("spoke-deployments-ready", func(t *testing.T) {
		waitForSpokeDeploymentsReady(t, context.TODO(), hub, spoke, names)
	})

	t.Run("delete-and-cleanup-spoke", func(t *testing.T) {
		// KSOperatorCRDelete verifies cluster-scoped cleanup against the
		// hub manifest, but in this flow those resources live on the spoke.
		if err := deleteHubKnativeServing(hub, names); err != nil {
			t.Fatalf("Failed to delete hub KnativeServing %q: %v", names.KnativeServing, err)
		}
		if err := waitForSpokeDeploymentsGone(t, spoke, names.Namespace); err != nil {
			t.Fatalf("Spoke deployments still present after deletion in namespace %q: %v",
				names.Namespace, err)
		}
	})
}

// createKnativeServingWithSpokeRef creates a KnativeServing CR carrying a
// clusterProfileRef pointing at the spoke ClusterProfile fixture.
//
// EnsureKnativeServingExists does not accept a ClusterProfileReference, so
// we construct the CR directly. Gateway API ingress is enabled because it
// is the most CRD-light option that works on a freshly provisioned spoke
// cluster (Istio would require the full Istio control plane, Kourier would
// require its own operator). The spoke cluster is assumed to already have
// the upstream gateway-api CRDs installed.
func createKnativeServingWithSpokeRef(clients *test.Clients, names test.ResourceNames) error {
	ks := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.KnativeServing,
			Namespace: names.Namespace,
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				ClusterProfileRef: &base.ClusterProfileReference{
					Name:      spokeClusterProfileRefName(),
					Namespace: spokeClusterProfileRefNamespace(),
				},
				Config: map[string]map[string]string{
					"network": {
						"ingress-class": "gateway-api.ingress.networking.knative.dev",
					},
				},
			},
			Ingress: &v1beta1.IngressConfigs{
				Istio:      base.IstioIngressConfiguration{Enabled: false},
				GatewayAPI: base.GatewayAPIIngressConfiguration{Enabled: true},
			},
		},
	}
	_, err := clients.KnativeServing().Create(context.TODO(), ks, metav1.CreateOptions{})
	if apierrs.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// deleteHubKnativeServing deletes the hub KnativeServing CR and waits for
// the finalizer (which unwinds the spoke-side install) to drain.
func deleteHubKnativeServing(clients *test.Clients, names test.ResourceNames) error {
	if err := clients.KnativeServing().Delete(context.TODO(), names.KnativeServing, metav1.DeleteOptions{}); err != nil {
		if apierrs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return wait.PollUntilContextTimeout(context.TODO(), spokeWaitInterval, spokeGoneTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := clients.KnativeServing().Get(ctx, names.KnativeServing, metav1.GetOptions{})
			if apierrs.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
}

func ensureSpokeNamespace(t *testing.T, clients *test.Clients, namespace string) {
	t.Helper()
	_, err := clients.KubeClient.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrs.IsAlreadyExists(err) {
		t.Fatalf("Failed to ensure spoke namespace %q: %v", namespace, err)
	}
}

// waitForSpokeDeploymentsReady blocks until every Deployment in the spoke
// namespace reports Available=True. Before polling the spoke it asserts that
// the hub-side KnativeServing CR has flipped TargetClusterResolved=True so we
// fail fast (within hubResolveTimeout) when the operator cannot reach the
// spoke at all instead of silently waiting for spokeReadyTimeout. While
// polling, progress is logged whenever the observed deployment count or the
// number of ready deployments changes, and on timeout the last observed
// deployment list is dumped to make CI failures debuggable.
func waitForSpokeDeploymentsReady(t *testing.T, ctx context.Context, hub *test.Clients, spoke *test.Clients, names test.ResourceNames) {
	t.Helper()

	// Phase 1: hub CR must report TargetClusterResolved=True.
	t.Logf("Waiting up to %s for hub KnativeServing %s/%s to report %s=True",
		hubResolveTimeout, names.Namespace, names.KnativeServing, base.TargetClusterResolved)

	var lastResolveStatus string
	resolveErr := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, hubResolveTimeout, true,
		func(ctx context.Context) (bool, error) {
			ks, err := hub.KnativeServing().Get(ctx, names.KnativeServing, metav1.GetOptions{})
			if err != nil {
				if apierrs.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			cond := ks.Status.GetCondition(base.TargetClusterResolved)
			if cond == nil {
				if lastResolveStatus != "Unknown(missing)" {
					t.Logf("hub CR %s condition not yet set", base.TargetClusterResolved)
					lastResolveStatus = "Unknown(missing)"
				}
				return false, nil
			}
			status := fmt.Sprintf("%s/%s/%s", cond.Status, cond.Reason, cond.Message)
			if status != lastResolveStatus {
				t.Logf("hub CR %s=%s reason=%q message=%q",
					base.TargetClusterResolved, cond.Status, cond.Reason, cond.Message)
				lastResolveStatus = status
			}
			switch cond.Status {
			case corev1.ConditionTrue:
				return true, nil
			case corev1.ConditionFalse:
				// Hard fail: operator could not resolve the spoke.
				return false, fmt.Errorf("hub CR %s=False reason=%q message=%q",
					base.TargetClusterResolved, cond.Reason, cond.Message)
			default:
				return false, nil
			}
		})
	if resolveErr != nil {
		t.Fatalf("hub KnativeServing %s/%s did not reach %s=True: %v",
			names.Namespace, names.KnativeServing, base.TargetClusterResolved, resolveErr)
	}

	// Phase 2: poll spoke Deployments until all Available=True.
	t.Logf("Waiting up to %s for all Deployments in spoke namespace %q to become Available",
		spokeReadyTimeout, names.Namespace)

	var (
		lastTotal    = -1
		lastReady    = -1
		lastObserved []appsv1.Deployment
	)
	pollErr := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			dpList, err := spoke.KubeClient.AppsV1().Deployments(names.Namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			lastObserved = dpList.Items
			total := len(dpList.Items)
			ready := 0
			for _, d := range dpList.Items {
				if isDeploymentAvailable(&d) {
					ready++
				}
			}
			if total != lastTotal || ready != lastReady {
				t.Logf("spoke ns %q: %d/%d Deployments Available", names.Namespace, ready, total)
				lastTotal = total
				lastReady = ready
			}
			if total == 0 {
				return false, nil
			}
			return ready == total, nil
		})
	if pollErr != nil {
		t.Logf("Spoke deployments did not become ready in namespace %q. Last observed state:",
			names.Namespace)
		dumpDeployments(t, lastObserved)
		t.Fatalf("Spoke deployments did not become ready in namespace %q: %v",
			names.Namespace, pollErr)
	}
}

func waitForSpokeDeploymentsGone(t *testing.T, clients *test.Clients, namespace string) error {
	t.Helper()
	t.Logf("Waiting up to %s for all Deployments in spoke namespace %q to disappear",
		spokeGoneTimeout, namespace)

	lastCount := -1
	return wait.PollUntilContextTimeout(context.TODO(), spokeWaitInterval, spokeGoneTimeout, true,
		func(ctx context.Context) (bool, error) {
			dpList, err := clients.KubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				if apierrs.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			if len(dpList.Items) != lastCount {
				t.Logf("spoke ns %q: %d Deployments remaining", namespace, len(dpList.Items))
				lastCount = len(dpList.Items)
			}
			return len(dpList.Items) == 0, nil
		})
}

func isDeploymentAvailable(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func dumpDeployments(t *testing.T, items []appsv1.Deployment) {
	t.Helper()
	if len(items) == 0 {
		t.Logf("  (no deployments observed)")
		return
	}
	names := make([]string, 0, len(items))
	byName := make(map[string]appsv1.Deployment, len(items))
	for _, d := range items {
		names = append(names, d.Name)
		byName[d.Name] = d
	}
	sort.Strings(names)
	for _, n := range names {
		d := byName[n]
		conds := make([]string, 0, len(d.Status.Conditions))
		for _, c := range d.Status.Conditions {
			conds = append(conds, fmt.Sprintf("%s=%s(%s)", c.Type, c.Status, c.Reason))
		}
		t.Logf("  - %s: replicas=%d/%d ready=%d available=%d updated=%d conditions=[%s]",
			n,
			d.Status.ReadyReplicas, d.Status.Replicas,
			d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas,
			strings.Join(conds, ","))
	}
}
