//go:build e2e && multicluster
// +build e2e,multicluster

/*
Copyright 2025 The Knative Authors

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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
	"knative.dev/operator/pkg/reconciler/common"
	"knative.dev/operator/test"
	"knative.dev/operator/test/client"
	"knative.dev/operator/test/resources"
	"knative.dev/pkg/apis"

	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

const (
	defaultSpokeClusterProfileName      = "spoke"
	defaultSpokeClusterProfileNamespace = "default"
	spokeServingInstallNamespace        = "knative-serving"
	spokeEventingInstallNamespace       = "knative-eventing"
	hubOperatorNamespace                = "knative-operator"
	hubOperatorDeployment               = "knative-operator"

	spokeWaitInterval = 5 * time.Second
	spokeReadyTimeout = 5 * time.Minute
	spokeGoneTimeout  = 3 * time.Minute
	hubResolveTimeout = 60 * time.Second
)

func spokeClusterProfileRefName() string {
	if v := os.Getenv("SPOKE_CLUSTER_NAME"); v != "" {
		return v
	}
	return defaultSpokeClusterProfileName
}

func spokeClusterProfileRefNamespace() string {
	if v := os.Getenv("SPOKE_CLUSTER_NAMESPACE"); v != "" {
		return v
	}
	return defaultSpokeClusterProfileNamespace
}

func TestMulticlusterKnativeServingSpokeDeployment(t *testing.T) {
	ctx := t.Context()

	hub := client.Setup(t)
	spoke := client.SetupSpoke(t)

	names := test.ResourceNames{
		KnativeServing: test.OperatorName,
		Namespace:      test.ServingOperatorNamespace,
	}

	ensureSpokeNamespace(ctx, t, spoke, spokeServingInstallNamespace)

	test.CleanupOnInterrupt(func() { test.TearDown(hub, names) })
	defer test.TearDown(hub, names)

	if err := createKnativeServingWithPlacement(ctx, hub, names); err != nil {
		t.Fatalf("Failed to create KnativeServing %q on hub: %v", names.KnativeServing, err)
	}

	t.Run("hub-cr-ready", func(t *testing.T) {
		resources.AssertKSOperatorCRReadyStatus(t, hub, names)
		waitForTargetClusterCondition(t.Context(), t, servingTargetCluster(hub, names), corev1.ConditionTrue, "")
	})

	t.Run("spoke-deployments-ready", func(t *testing.T) {
		waitForSpokeDeploymentsReady(t.Context(), t, hub, spoke, names)
	})

	t.Run("spoke-runtime-resources-owned-by-anchor", func(t *testing.T) {
		waitForSpokeRuntimeResourcesOwned(t.Context(), t, spoke, newSpokeKnativeServing(names))
	})

	t.Run("cluster-profile-update-refreshes-client", func(t *testing.T) {
		assertClusterProfileReadinessRefreshesClient(t.Context(), t, hub, names)
	})

	t.Run("delete-after-operator-restart-and-cleanup-spoke", func(t *testing.T) {
		ctx := t.Context()
		scale, err := hub.KubeClient.AppsV1().Deployments(hubOperatorNamespace).GetScale(
			ctx, hubOperatorDeployment, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get hub operator scale: %v", err)
		}
		originalReplicas := scale.Spec.Replicas
		operatorRestored := false
		t.Cleanup(func() {
			if operatorRestored {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), spokeReadyTimeout)
			defer cancel()
			if err := setAndWaitHubOperatorReplicas(cleanupCtx, hub, originalReplicas); err != nil {
				t.Errorf("Failed to restore hub operator to %d replicas: %v", originalReplicas, err)
			}
		})

		if err := setAndWaitHubOperatorReplicas(ctx, hub, 0); err != nil {
			t.Fatalf("Failed to scale down hub operator: %v", err)
		}
		if err := hub.KnativeServing().Delete(ctx, names.KnativeServing, metav1.DeleteOptions{}); err != nil && !apierrs.IsNotFound(err) {
			t.Fatalf("Failed to delete hub KnativeServing %q: %v", names.KnativeServing, err)
		}
		if err := setAndWaitHubOperatorReplicas(ctx, hub, originalReplicas); err != nil {
			t.Fatalf("Failed to restart hub operator: %v", err)
		}
		operatorRestored = true
		if err := waitForHubKnativeServingGone(ctx, hub, names); err != nil {
			t.Fatalf("Hub KnativeServing %q still present after operator restart: %v",
				names.KnativeServing, err)
		}
		if err := waitForSpokeDeploymentsGone(ctx, t, spoke, spokeServingInstallNamespace); err != nil {
			t.Fatalf("Spoke deployments still present after deletion in namespace %q: %v",
				spokeServingInstallNamespace, err)
		}
		waitForSpokeRuntimeResourcesGone(ctx, t, spoke, spokeServingInstallNamespace)
		assertAnchorConfigMapGone(ctx, t, spoke, newSpokeKnativeServing(names))
	})
}

func assertClusterProfileReadinessRefreshesClient(ctx context.Context, t *testing.T, hub *test.Clients, names test.ResourceNames) {
	t.Helper()
	tc := servingTargetCluster(hub, names)
	profiles := hub.ClusterInventory.ApisV1alpha1().ClusterProfiles(spokeClusterProfileRefNamespace())
	profile, err := profiles.Get(ctx, spokeClusterProfileRefName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get ClusterProfile: %v", err)
	}
	originalStatus := *profile.Status.DeepCopy()

	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), hubResolveTimeout)
		defer cancel()
		current, err := profiles.Get(restoreCtx, spokeClusterProfileRefName(), metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get ClusterProfile for status restore: %v", err)
			return
		}
		current.Status = originalStatus
		if _, err := profiles.UpdateStatus(restoreCtx, current, metav1.UpdateOptions{}); err != nil {
			t.Errorf("Failed to restore ClusterProfile status: %v", err)
			return
		}
		waitForTargetClusterCondition(restoreCtx, t, tc, corev1.ConditionTrue, "")
	}()

	apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:    clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy,
		Status:  metav1.ConditionFalse,
		Reason:  "E2EReadinessTransition",
		Message: "temporarily unavailable to verify client cache invalidation",
	})
	if _, err := profiles.UpdateStatus(ctx, profile, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Failed to mark ClusterProfile not ready: %v", err)
	}

	waitForTargetClusterCondition(ctx, t, tc, corev1.ConditionFalse, base.ReasonClusterProfileNotReady)
}

// targetCluster reads the TargetClusterResolved condition off a hub CR, so the assertions
// below are shared between KnativeServing and KnativeEventing.
type targetCluster struct {
	describe string
	get      func(context.Context) (*apis.Condition, error)
}

func servingTargetCluster(hub *test.Clients, names test.ResourceNames) targetCluster {
	return targetCluster{
		describe: fmt.Sprintf("KnativeServing %s/%s", names.Namespace, names.KnativeServing),
		get: func(ctx context.Context) (*apis.Condition, error) {
			ks, err := hub.KnativeServing().Get(ctx, names.KnativeServing, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return ks.Status.GetCondition(base.TargetClusterResolved), nil
		},
	}
}

func eventingTargetCluster(hub *test.Clients, names test.ResourceNames) targetCluster {
	return targetCluster{
		describe: fmt.Sprintf("KnativeEventing %s/%s", names.Namespace, names.KnativeEventing),
		get: func(ctx context.Context) (*apis.Condition, error) {
			ke, err := hub.KnativeEventing().Get(ctx, names.KnativeEventing, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return ke.Status.GetCondition(base.TargetClusterResolved), nil
		},
	}
}

// waitForTargetClusterCondition polls the hub CR until its TargetClusterResolved
// condition reports wantStatus, and wantReason when that is non-empty.
func waitForTargetClusterCondition(
	ctx context.Context,
	t *testing.T,
	tc targetCluster,
	wantStatus corev1.ConditionStatus,
	wantReason string,
) {
	t.Helper()
	t.Logf("Waiting up to %s for hub %s to report %s=%s",
		hubResolveTimeout, tc.describe, base.TargetClusterResolved, wantStatus)

	last := ""
	err := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, hubResolveTimeout, true,
		func(ctx context.Context) (bool, error) {
			cond, err := tc.get(ctx)
			if err != nil {
				if apierrs.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			state := "missing"
			if cond != nil {
				state = fmt.Sprintf("status=%s reason=%q message=%q", cond.Status, cond.Reason, cond.Message)
			}
			if state != last {
				t.Logf("hub %s %s: %s", tc.describe, base.TargetClusterResolved, state)
				last = state
			}
			return cond != nil && cond.Status == wantStatus &&
				(wantReason == "" || cond.Reason == wantReason), nil
		})
	if err != nil {
		t.Fatalf("hub %s did not reach %s=%s reason=%q (last: %s): %v",
			tc.describe, base.TargetClusterResolved, wantStatus, wantReason, last, err)
	}
}

func newSpokeKnativeServing(names test.ResourceNames) *v1beta1.KnativeServing {
	return &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.KnativeServing,
			Namespace: names.Namespace,
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				Placement: &base.ComponentPlacement{
					ClusterProfileRef: base.ClusterProfileReference{
						Name:      spokeClusterProfileRefName(),
						Namespace: spokeClusterProfileRefNamespace(),
					},
					Namespace: spokeServingInstallNamespace,
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
}

func createKnativeServingWithPlacement(ctx context.Context, clients *test.Clients, names test.ResourceNames) error {
	_, err := clients.KnativeServing().Create(ctx, newSpokeKnativeServing(names), metav1.CreateOptions{})
	if apierrs.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func deleteHubKnativeServing(ctx context.Context, clients *test.Clients, names test.ResourceNames) error {
	if err := clients.KnativeServing().Delete(ctx, names.KnativeServing, metav1.DeleteOptions{}); err != nil {
		if apierrs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return waitForHubKnativeServingGone(ctx, clients, names)
}

func waitForHubKnativeServingGone(ctx context.Context, clients *test.Clients, names test.ResourceNames) error {
	return wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeGoneTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := clients.KnativeServing().Get(ctx, names.KnativeServing, metav1.GetOptions{})
			if apierrs.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
}

func setAndWaitHubOperatorReplicas(ctx context.Context, hub *test.Clients, replicas int32) error {
	deployments := hub.KubeClient.AppsV1().Deployments(hubOperatorNamespace)
	scale, err := deployments.GetScale(ctx, hubOperatorDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Deployment scale: %w", err)
	}
	scale.Spec.Replicas = replicas
	if _, err := deployments.UpdateScale(
		ctx, hubOperatorDeployment, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update Deployment scale to %d: %w", replicas, err)
	}

	return wait.PollUntilContextTimeout(ctx, time.Second, spokeReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			deployment, err := deployments.Get(ctx, hubOperatorDeployment, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if replicas == 0 {
				pods, err := hub.KubeClient.CoreV1().Pods(hubOperatorNamespace).List(
					ctx, metav1.ListOptions{LabelSelector: "name=knative-operator"})
				if err != nil {
					return false, err
				}
				return deployment.Status.Replicas == 0 && len(pods.Items) == 0, nil
			}
			return deployment.Status.ObservedGeneration >= deployment.Generation &&
				deployment.Status.UpdatedReplicas == replicas &&
				deployment.Status.AvailableReplicas == replicas, nil
		})
}

func ensureSpokeNamespace(ctx context.Context, t *testing.T, clients *test.Clients, namespace string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := clients.KubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrs.IsAlreadyExists(err) {
		t.Fatalf("Failed to ensure spoke namespace %q: %v", namespace, err)
	}
}

func waitForSpokeDeploymentsReady(ctx context.Context, t *testing.T, hub *test.Clients, spoke *test.Clients, names test.ResourceNames) {
	t.Helper()
	waitForTargetClusterCondition(ctx, t, servingTargetCluster(hub, names), corev1.ConditionTrue, "")
	waitForSpokeDeploymentsAvailable(ctx, t, spoke, spokeServingInstallNamespace)
}

func waitForSpokeDeploymentsAvailable(ctx context.Context, t *testing.T, spoke *test.Clients, namespace string) {
	t.Helper()
	t.Logf("Waiting up to %s for all Deployments in spoke namespace %q to become Available",
		spokeReadyTimeout, namespace)

	var (
		lastTotal    = -1
		lastReady    = -1
		lastObserved []appsv1.Deployment
	)
	pollErr := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			dpList, err := spoke.KubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			lastObserved = dpList.Items
			total := len(dpList.Items)
			ready := 0
			for i := range dpList.Items {
				if available, _ := resources.IsDeploymentAvailable(&dpList.Items[i]); available {
					ready++
				}
			}
			if total != lastTotal || ready != lastReady {
				t.Logf("spoke ns %q: %d/%d Deployments Available", namespace, ready, total)
				lastTotal = total
				lastReady = ready
			}
			if total == 0 {
				return false, nil
			}
			return ready == total, nil
		})
	if pollErr != nil {
		t.Logf("Spoke deployments did not become ready in namespace %q. Last observed state:", namespace)
		dumpDeployments(t, lastObserved)
		t.Fatalf("Spoke deployments did not become ready in namespace %q: %v", namespace, pollErr)
	}
}

func waitForSpokeDeploymentsGone(ctx context.Context, t *testing.T, clients *test.Clients, namespace string) error {
	t.Helper()
	t.Logf("Waiting up to %s for all Deployments in spoke namespace %q to disappear",
		spokeGoneTimeout, namespace)

	lastCount := -1
	return wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeGoneTimeout, true,
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

func waitForSpokeRuntimeResourcesGone(ctx context.Context, t *testing.T, spoke *test.Clients, namespace string) {
	t.Helper()
	t.Logf("Waiting up to %s for Services, EndpointSlices, and Leases in spoke namespace %q to disappear",
		spokeGoneTimeout, namespace)

	lastState := ""
	err := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeGoneTimeout, true,
		func(ctx context.Context) (bool, error) {
			services, err := spoke.KubeClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			slices, err := spoke.KubeClient.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			leases, err := spoke.KubeClient.CoordinationV1().Leases(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			state := fmt.Sprintf("services=%d endpointSlices=%d leases=%d",
				len(services.Items), len(slices.Items), len(leases.Items))
			if state != lastState {
				t.Logf("spoke ns %q: %s remaining", namespace, state)
				lastState = state
			}
			return len(services.Items) == 0 && len(slices.Items) == 0 && len(leases.Items) == 0, nil
		})
	if err != nil {
		t.Fatalf("Spoke namespace %q still has runtime resources after cleanup (%s): %v", namespace, lastState, err)
	}
}

func waitForSpokeRuntimeResourcesOwned(
	ctx context.Context,
	t *testing.T,
	spoke *test.Clients,
	instance base.KComponent,
) {
	t.Helper()
	namespace, anchorName := common.InstallationNamespace(instance), common.AnchorName(instance)
	lastState := ""
	err := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			anchor, err := spoke.KubeClient.CoreV1().ConfigMaps(namespace).Get(
				ctx, anchorName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			leases, err := spoke.KubeClient.CoordinationV1().Leases(namespace).List(
				ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			ownedLeases := 0
			unownedServices := 0
			for i := range leases.Items {
				lease := &leases.Items[i]
				if !metav1.IsControlledBy(lease, anchor) {
					continue
				}
				ownedLeases++
				service, err := spoke.KubeClient.CoreV1().Services(namespace).Get(
					ctx, lease.Name, metav1.GetOptions{})
				if apierrs.IsNotFound(err) {
					continue
				}
				if err != nil {
					return false, err
				}
				if !metav1.IsControlledBy(service, anchor) {
					unownedServices++
				}
			}
			state := fmt.Sprintf("ownedLeases=%d unownedSameNameServices=%d",
				ownedLeases, unownedServices)
			if state != lastState {
				t.Logf("spoke ns %q runtime ownership: %s", namespace, state)
				lastState = state
			}
			return ownedLeases > 0 && unownedServices == 0, nil
		})
	if err != nil {
		t.Fatalf("Spoke namespace %q runtime resources were not adopted by anchor %q (%s): %v",
			namespace, anchorName, lastState, err)
	}
}

func assertAnchorConfigMapGone(ctx context.Context, t *testing.T, spoke *test.Clients, instance base.KComponent) {
	t.Helper()
	namespace, anchorName := common.InstallationNamespace(instance), common.AnchorName(instance)
	err := wait.PollUntilContextTimeout(ctx, spokeWaitInterval, spokeGoneTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := spoke.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, anchorName, metav1.GetOptions{})
			if apierrs.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	if err != nil {
		t.Fatalf("Anchor ConfigMap %q still exists in spoke namespace %q: %v", anchorName, namespace, err)
	}
}
