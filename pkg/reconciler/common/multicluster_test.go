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

package common

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	mf "github.com/manifestival/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
	"knative.dev/pkg/controller"

	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
)

func TestResolveTargetCluster_NilRef(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
	}
	instance.Status.InitializeConditions()

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	origClient := manifest.Client

	var state ReconcileState
	stage := ResolveTargetCluster(nil, &state)
	if err := stage(context.Background(), &manifest, instance); err != nil {
		t.Fatalf("ResolveTargetCluster() = %v, want nil", err)
	}

	if manifest.Client != origClient {
		t.Fatal("manifest.Client changed unexpectedly")
	}

	if state.AnchorOwner != nil {
		t.Fatal("state.AnchorOwner is non-nil, want nil")
	}

	if state.IsRemote() {
		t.Fatal("state.IsRemote() = true, want false")
	}

	cond := instance.Status.GetCondition(base.TargetClusterResolved)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("TargetClusterResolved = %v, want True", cond)
	}
}

var testSpokeRef = base.ClusterProfileReference{Namespace: "fleet-system", Name: "spoke-tokyo"}

func testPlacement(namespace string, ref base.ClusterProfileReference) *base.ComponentPlacement {
	return &base.ComponentPlacement{
		ClusterProfileRef: ref,
		Namespace:         namespace,
	}
}

func anchorConfigMap(instance base.KComponent) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnchorName(instance),
			Namespace: InstallationNamespace(instance),
			UID:       "anchor-uid",
		},
	}
}

func anchorOwnedDeployment(anchor *corev1.ConfigMap, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: anchor.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(anchor, corev1.SchemeGroupVersion.WithKind("ConfigMap")),
			},
		},
	}
}

func TestPlacementAccessors(t *testing.T) {
	tests := []struct {
		name          string
		instance      base.KComponent
		wantNamespace string
		wantRef       *base.ClusterProfileReference
	}{{
		name: "local install",
		instance: &v1beta1.KnativeServing{
			ObjectMeta: metav1.ObjectMeta{Namespace: "hub-workloads"},
		},
		wantNamespace: "hub-workloads",
	}, {
		name: "placement selects the remote namespace",
		instance: &v1beta1.KnativeServing{
			ObjectMeta: metav1.ObjectMeta{Namespace: "hub-workloads"},
			Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
				Placement: testPlacement("knative-serving", testSpokeRef),
			}},
		},
		wantNamespace: "knative-serving",
		wantRef:       &testSpokeRef,
	}, {
		name: "deprecated clusterProfileRef installs into the management CR namespace",
		instance: &v1beta1.KnativeServing{
			ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-workloads"},
			Spec:       v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{ClusterProfileRef: &testSpokeRef}},
		},
		wantNamespace: "fleet-workloads",
		wantRef:       &testSpokeRef,
	}, {
		name: "deprecated clusterProfileRef wins over placement",
		instance: &v1beta1.KnativeServing{
			ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving"},
			Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
				ClusterProfileRef: &testSpokeRef,
				Placement: testPlacement("custom-serving", base.ClusterProfileReference{
					Namespace: "fleet-system",
					Name:      "spoke-osaka",
				}),
			}},
		},
		wantNamespace: "knative-serving",
		wantRef:       &testSpokeRef,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(ClusterProfileRef(tt.instance), tt.wantRef); diff != "" {
				t.Errorf("ClusterProfileRef() (-got, +want): %s", diff)
			}
			if got := InstallationNamespace(tt.instance); got != tt.wantNamespace {
				t.Errorf("InstallationNamespace() = %q, want %q", got, tt.wantNamespace)
			}
		})
	}
}

func TestValidatePlacement(t *testing.T) {
	tests := []struct {
		name         string
		spec         base.CommonSpec
		wantErrParts []string
	}{{
		name: "local install",
	}, {
		name: "placement alone may pick any namespace",
		spec: base.CommonSpec{Placement: testPlacement("custom-serving", testSpokeRef)},
	}, {
		name: "deprecated clusterProfileRef alone",
		spec: base.CommonSpec{ClusterProfileRef: &testSpokeRef},
	}, {
		name: "matching migration",
		spec: base.CommonSpec{
			ClusterProfileRef: &testSpokeRef,
			Placement:         testPlacement("knative-serving", testSpokeRef),
		},
	}, {
		name: "different namespace",
		spec: base.CommonSpec{
			ClusterProfileRef: &testSpokeRef,
			Placement:         testPlacement("custom-serving", testSpokeRef),
		},
		wantErrParts: []string{"must match metadata.namespace", "correct spec.placement"},
	}, {
		name: "different cluster",
		spec: base.CommonSpec{
			ClusterProfileRef: &testSpokeRef,
			Placement: testPlacement("knative-serving", base.ClusterProfileReference{
				Namespace: "fleet-system",
				Name:      "spoke-osaka",
			}),
		},
		wantErrParts: []string{"clusterProfileRef must match", "correct spec.placement"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving"},
				Spec:       v1beta1.KnativeServingSpec{CommonSpec: tt.spec},
			}
			err := ValidatePlacement(instance)
			if len(tt.wantErrParts) == 0 && err != nil {
				t.Fatalf("ValidatePlacement() = %v, want nil", err)
			}
			for _, want := range tt.wantErrParts {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("ValidatePlacement() = %v, want error containing %q", err, want)
				}
			}
		})
	}
}

func TestEnsureAnchorConfigMap_Create(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fleet-workloads",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				Placement: testPlacement("test-ns", testSpokeRef),
			},
		},
	}

	ctx := context.Background()
	anchor, err := EnsureAnchorConfigMap(ctx, kubeClient, instance)
	if err != nil {
		t.Fatalf("EnsureAnchorConfigMap() error: %v", err)
	}

	expectedName := "knativeserving-test-root-owner"
	if anchor.Name != expectedName {
		t.Fatalf("anchor.Name = %q, want %q", anchor.Name, expectedName)
	}
	if anchor.Namespace != "test-ns" {
		t.Fatalf("anchor.Namespace = %q, want %q", anchor.Namespace, "test-ns")
	}

	if got := anchor.Labels["app.kubernetes.io/managed-by"]; got != "knative-operator" {
		t.Fatalf("label managed-by = %q, want %q", got, "knative-operator")
	}
	if got := anchor.Labels["operator.knative.dev/cr-name"]; got != "test" {
		t.Fatalf("label cr-name = %q, want %q", got, "test")
	}

	if got := anchor.Annotations["operator.knative.dev/anchor"]; got != "true" {
		t.Fatalf("annotation anchor = %q, want %q", got, "true")
	}
	if got := anchor.Annotations["operator.knative.dev/protected"]; got != "true" {
		t.Fatalf("annotation protected = %q, want %q", got, "true")
	}

	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, "test-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "knative-operator" {
		t.Fatalf("namespace label managed-by = %q, want %q", got, "knative-operator")
	}
}

func TestEnsureAnchorConfigMap_AlreadyExists(t *testing.T) {
	existingAnchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "knativeserving-test-root-owner",
			Namespace: "test-ns",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "knative-operator",
				"operator.knative.dev/cr-name": "test",
			},
			Annotations: map[string]string{
				"operator.knative.dev/anchor": "true",
			},
		},
	}
	existingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(existingNS, existingAnchor)

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fleet-workloads",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				Placement: testPlacement("test-ns", testSpokeRef),
			},
		},
	}

	ctx := context.Background()
	anchor, err := EnsureAnchorConfigMap(ctx, kubeClient, instance)
	if err != nil {
		t.Fatalf("EnsureAnchorConfigMap() error: %v", err)
	}

	if anchor.Name != "knativeserving-test-root-owner" {
		t.Fatalf("anchor.Name = %q, want %q", anchor.Name, "knativeserving-test-root-owner")
	}
	if anchor.Namespace != "test-ns" {
		t.Fatalf("anchor.Namespace = %q, want %q", anchor.Namespace, "test-ns")
	}
}

func TestEnsureAnchorConfigMap_AdditiveMerge(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "knative-serving",
			Namespace: "knative-serving",
		},
	}
	oldAnchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AnchorName(instance),
			Namespace: "knative-serving",
			Labels: map[string]string{
				"old-label": "old-value",
			},
			Annotations: map[string]string{
				"old-annotation": "old-value",
			},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "knative-serving"},
	}
	kubeClient := fake.NewSimpleClientset(oldAnchor, ns)

	anchor, err := EnsureAnchorConfigMap(context.Background(), kubeClient, instance)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := anchor.Labels["app.kubernetes.io/managed-by"]; got != "knative-operator" {
		t.Errorf("managed-by label = %q, want %q", got, "knative-operator")
	}
	if got := anchor.Labels["operator.knative.dev/cr-name"]; got != "knative-serving" {
		t.Errorf("cr-name label = %q, want %q", got, "knative-serving")
	}
	if got := anchor.Labels["old-label"]; got != "old-value" {
		t.Errorf("old-label not preserved, got labels: %v", anchor.Labels)
	}

	if got := anchor.Annotations["operator.knative.dev/anchor"]; got != "true" {
		t.Errorf("anchor annotation = %q, want %q", got, "true")
	}
	if got := anchor.Annotations["old-annotation"]; got != "old-value" {
		t.Errorf("old-annotation not preserved, got annotations: %v", anchor.Annotations)
	}
}

func TestEnsureAnchorConfigMap_NameTooLong(t *testing.T) {
	longName := strings.Repeat("a", 250)
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      longName,
		},
	}

	kubeClient := fake.NewSimpleClientset()
	_, err := EnsureAnchorConfigMap(context.Background(), kubeClient, instance)
	if err == nil {
		t.Fatal("EnsureAnchorConfigMap() = nil, want error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Fatalf("error = %v, want substring %q", err, "exceeds maximum length")
	}
}

func TestDeleteAnchorConfigMap_Success(t *testing.T) {
	existingAnchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "knativeserving-test-root-owner",
			Namespace: "test-ns",
		},
	}
	kubeClient := fake.NewSimpleClientset(existingAnchor)

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fleet-workloads",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				Placement: testPlacement("test-ns", testSpokeRef),
			},
		},
	}

	ctx := context.Background()
	if err := DeleteAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("DeleteAnchorConfigMap() error: %v", err)
	}

	_, err := kubeClient.CoreV1().ConfigMaps("test-ns").Get(ctx, "knativeserving-test-root-owner", metav1.GetOptions{})
	if err == nil {
		t.Fatal("anchor ConfigMap still exists after deletion")
	}
}

func TestAdoptRemoteRuntimeResources_IsIdempotent(t *testing.T) {
	holder := "controller-7c5d9f-abcde_01234567"
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", testSpokeRef),
		}},
	}
	anchor := anchorConfigMap(instance)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "controller.00-of-01", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: lease.Name, Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(anchor, lease, service)
	manifest, err := mf.ManifestFrom(mf.Slice{
		*NamespacedResource("apps/v1", "Deployment", "test-ns", "controller"),
	})
	if err != nil {
		t.Fatalf("ManifestFrom() error: %v", err)
	}

	for run := 1; run <= 2; run++ {
		if err := adoptRemoteRuntimeResources(
			t.Context(), kubeClient, &manifest, instance, anchor); err != nil {
			t.Fatalf("adoptRemoteRuntimeResources() run %d error: %v", run, err)
		}
	}
	if got := updateActionCount(kubeClient.Actions()); got != 2 {
		t.Fatalf("update action count = %d, want 2 from the first run only", got)
	}
	gotLease, err := kubeClient.CoordinationV1().Leases("test-ns").Get(
		t.Context(), lease.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Lease) error: %v", err)
	}
	gotService, err := kubeClient.CoreV1().Services("test-ns").Get(
		t.Context(), service.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(Service) error: %v", err)
	}
	for description, object := range map[string]metav1.Object{
		"Lease": gotLease, "Service": gotService,
	} {
		controller := metav1.GetControllerOfNoCopy(object)
		if controller == nil || controller.UID != anchor.UID {
			t.Errorf("%s controller = %v, want anchor UID %q", description, controller, anchor.UID)
		}
	}
}

func TestSetAnchorControllerReference_DoesNotStealController(t *testing.T) {
	anchor := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "anchor", UID: "anchor-uid"}}
	controller := true
	otherController := metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "other",
		UID: "other-uid", Controller: &controller,
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		OwnerReferences: []metav1.OwnerReference{otherController},
	}}

	changed, conflict := setAnchorControllerReference(service, anchor)
	if changed {
		t.Fatal("setAnchorControllerReference() changed = true, want false")
	}
	if conflict == nil || conflict.UID != otherController.UID {
		t.Fatalf("setAnchorControllerReference() conflict = %v, want UID %q", conflict, otherController.UID)
	}
	if got := metav1.GetControllerOf(service); got == nil || got.UID != otherController.UID {
		t.Errorf("Service controller = %v, want original controller UID %q", got, otherController.UID)
	}
}

func TestAdoptRuntimeLease_RetriesConflict(t *testing.T) {
	anchor := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "anchor", UID: "anchor-uid"}}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "election", Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(lease)
	updateCalls := 0
	kubeClient.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				lease.Name, errors.New("injected conflict"))
		}
		return false, nil, nil
	})

	if err := adoptRuntimeLease(t.Context(), kubeClient, anchor, "test-ns", lease.Name); err != nil {
		t.Fatalf("adoptRuntimeLease() error: %v", err)
	}
	if updateCalls != 2 {
		t.Fatalf("Lease update calls = %d, want 2", updateCalls)
	}
}

func TestAdoptRuntimeService_ReportsUpdateError(t *testing.T) {
	anchor := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "anchor", UID: "anchor-uid"}}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "election", Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(service)
	kubeClient.PrependReactor("update", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected Service update failure")
	})

	err := adoptRuntimeService(t.Context(), kubeClient, anchor, "test-ns", service.Name)
	if err == nil || !strings.Contains(err.Error(), "injected Service update failure") {
		t.Fatalf("adoptRuntimeService() error = %v, want injected failure", err)
	}
}

func updateActionCount(actions []clienttesting.Action) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == "update" {
			count++
		}
	}
	return count
}

func TestDeleteAnchorConfigMap_Pending(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test-ns", Name: "test"},
	}
	anchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AnchorName(instance), Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(anchor)
	kubeClient.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	err := DeleteAnchorConfigMap(t.Context(), kubeClient, instance)
	if !errors.Is(err, errAnchorDeletionPending) {
		t.Fatalf("DeleteAnchorConfigMap() error = %v, want errAnchorDeletionPending", err)
	}
}

func TestFinalizeRemoteClusterIfNeeded_RequeuesWhileAnchorDeletionPending(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}),
		}},
	}
	anchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AnchorName(instance), Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(anchor)
	kubeClient.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	entryCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	provider := &ClusterProvider{entries: map[string]*clusterEntry{
		"fleet/spoke": {
			mfClient:   fakeMfClient{},
			kubeClient: kubeClient,
			ctx:        entryCtx,
			cancel:     cancel,
		},
	}}

	handled, err := FinalizeRemoteClusterIfNeeded(t.Context(), provider, nil, instance)
	if !handled {
		t.Fatal("FinalizeRemoteClusterIfNeeded() handled = false, want true")
	}
	if requeue, after := controller.IsRequeueKey(err); !requeue || after != remoteAnchorDeletionWait {
		t.Fatalf("FinalizeRemoteClusterIfNeeded() error = %v, want requeue after %v", err, remoteAnchorDeletionWait)
	}
}

func TestFinalizeRemoteClusterIfNeeded_RefreshesCacheMiss(t *testing.T) {
	stub := &stubAccess{}
	kubeClient := fake.NewSimpleClientset()
	factory := &stubClientFactory{kubeClient: kubeClient}
	provider := newTestProviderWithStubAccess(
		stub, readyClusterProfile("fleet", "worker"))
	provider.clientFactory = factory
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{
				Namespace: "fleet",
				Name:      "worker",
			}),
		}},
	}

	handled, err := FinalizeRemoteClusterIfNeeded(t.Context(), provider, nil, instance)
	if err != nil {
		t.Fatalf("FinalizeRemoteClusterIfNeeded() error: %v", err)
	}
	if !handled {
		t.Fatal("FinalizeRemoteClusterIfNeeded() handled = false, want true")
	}
	if got := stub.count(); got != 1 {
		t.Errorf("BuildConfigFromCP call count = %d, want 1", got)
	}
	if got := factory.mfCount.Load(); got != 1 {
		t.Errorf("NewMfClient call count = %d, want 1", got)
	}
	if got := factory.kubeCount.Load(); got != 1 {
		t.Errorf("NewKubeClient call count = %d, want 1", got)
	}
	if _, _, err := provider.Get(t.Context(), "fleet/worker"); err != nil {
		t.Errorf("refreshed entry was not cached: %v", err)
	}
}

func TestFinalizeRemoteClusterIfNeeded_DoesNotHideConcurrentCleanupError(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}),
		}},
	}
	anchor := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AnchorName(instance), Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(anchor)
	kubeClient.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected runtime cleanup failure")
	})
	kubeClient.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	entryCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	provider := &ClusterProvider{entries: map[string]*clusterEntry{
		"fleet/spoke": {
			mfClient:   fakeMfClient{},
			kubeClient: kubeClient,
			ctx:        entryCtx,
			cancel:     cancel,
		},
	}}

	handled, err := FinalizeRemoteClusterIfNeeded(t.Context(), provider, nil, instance)
	if !handled {
		t.Fatal("FinalizeRemoteClusterIfNeeded() handled = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "injected runtime cleanup failure") {
		t.Fatalf("FinalizeRemoteClusterIfNeeded() error = %v, want concurrent cleanup failure", err)
	}
	if requeue, _ := controller.IsRequeueKey(err); requeue {
		t.Fatalf("FinalizeRemoteClusterIfNeeded() error = %v, want regular error", err)
	}
}

func TestFinalizeRemoteCluster_PreservesAnchorForRuntimeCleanupRetry(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test-ns", Name: "test"},
	}
	holder := "controller-oldhash-abcde_01234567"
	anchor := anchorConfigMap(instance)
	deployment := anchorOwnedDeployment(anchor, "controller")
	deployment.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "controller"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controller-oldhash-abcde",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "controller"},
		},
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "controller.00-of-01", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	kubeClient := fake.NewSimpleClientset(anchor, deployment, pod, lease)
	failRuntimeCleanup := true
	kubeClient.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failRuntimeCleanup {
			return true, nil, errors.New("injected runtime cleanup failure")
		}
		return false, nil, nil
	})
	clients := &clusterEntry{mfClient: fakeMfClient{}, kubeClient: kubeClient}

	err := FinalizeRemoteCluster(context.Background(), clients, nil, instance)
	if err == nil || !strings.Contains(err.Error(), "injected runtime cleanup failure") {
		t.Fatalf("FinalizeRemoteCluster() error = %v, want runtime cleanup failure", err)
	}
	if _, err := kubeClient.CoreV1().ConfigMaps("test-ns").Get(
		context.Background(), AnchorName(instance), metav1.GetOptions{}); err != nil {
		t.Fatalf("anchor ConfigMap was deleted before runtime cleanup could be retried: %v", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(
		context.Background(), lease.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("runtime Lease was deleted despite the injected discovery failure: %v", err)
	}

	failRuntimeCleanup = false
	if err := FinalizeRemoteCluster(context.Background(), clients, nil, instance); err != nil {
		t.Fatalf("FinalizeRemoteCluster() retry error: %v", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(
		context.Background(), lease.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("runtime Lease Get() after retry error = %v, want NotFound", err)
	}
	if _, err := kubeClient.CoreV1().ConfigMaps("test-ns").Get(
		context.Background(), AnchorName(instance), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("anchor ConfigMap Get() after successful retry error = %v, want NotFound", err)
	}
}

func TestDeleteRemoteRuntimeResources(t *testing.T) {
	managedHolder := "controller-oldhash-abcde_01234567"
	unmanagedHolder := "unrelated-7c5d9f-abcde_01234567"
	managedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "controller.00-of-01",
			Namespace: "test-ns",
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &managedHolder},
	}
	unmanagedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-election", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &unmanagedHolder},
	}
	managedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: managedLease.Name, Namespace: "test-ns"},
	}
	unmanagedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: unmanagedLease.Name, Namespace: "test-ns"},
	}
	managedSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autoscaler-bucket-slice",
			Namespace: "test-ns",
			Labels:    map[string]string{discoveryv1.LabelServiceName: managedLease.Name},
		},
	}
	unmanagedSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-slice",
			Namespace: "test-ns",
			Labels:    map[string]string{discoveryv1.LabelServiceName: unmanagedLease.Name},
		},
	}
	kubeClient := fake.NewSimpleClientset(
		managedLease, unmanagedLease, managedService, unmanagedService, managedSlice, unmanagedSlice)

	manifest, err := mf.ManifestFrom(mf.Slice{*NamespacedResource("apps/v1", "Deployment", "test-ns", "controller")})
	if err != nil {
		t.Fatalf("ManifestFrom() error: %v", err)
	}
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}),
		}},
	}

	ctx := context.Background()
	if err := deleteRemoteRuntimeResources(ctx, kubeClient, &manifest, instance); err != nil {
		t.Fatalf("deleteRemoteRuntimeResources() error: %v", err)
	}

	leases := kubeClient.CoordinationV1().Leases("test-ns")
	services := kubeClient.CoreV1().Services("test-ns")
	slices := kubeClient.DiscoveryV1().EndpointSlices("test-ns")
	assertions := []struct {
		desc     string
		get      func() error
		wantGone bool
	}{
		{"managed Lease", func() error { _, err := leases.Get(ctx, managedLease.Name, metav1.GetOptions{}); return err }, true},
		{"managed Service", func() error { _, err := services.Get(ctx, managedService.Name, metav1.GetOptions{}); return err }, true},
		{"managed EndpointSlice", func() error { _, err := slices.Get(ctx, managedSlice.Name, metav1.GetOptions{}); return err }, true},
		{"unmanaged Lease", func() error { _, err := leases.Get(ctx, unmanagedLease.Name, metav1.GetOptions{}); return err }, false},
		{"unmanaged Service", func() error { _, err := services.Get(ctx, unmanagedService.Name, metav1.GetOptions{}); return err }, false},
		{"unmanaged EndpointSlice", func() error { _, err := slices.Get(ctx, unmanagedSlice.Name, metav1.GetOptions{}); return err }, false},
	}
	for _, a := range assertions {
		if err := a.get(); apierrors.IsNotFound(err) != a.wantGone {
			t.Errorf("%s Get() error = %v, wantGone %v", a.desc, err, a.wantGone)
		}
	}
}

func TestDeleteRemoteRuntimeResources_RetriesBeforeDeletingLease(t *testing.T) {
	holder := "autoscaler-7c5d9f-abcde_01234567"
	resourceName := "autoscaler-bucket-00-of-01"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "test-ns"},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "autoscaler-bucket-slice",
			Namespace: "test-ns",
			Labels:    map[string]string{discoveryv1.LabelServiceName: resourceName},
		},
	}
	kubeClient := fake.NewSimpleClientset(lease, service, slice)

	manifest, err := mf.ManifestFrom(mf.Slice{*NamespacedResource("apps/v1", "Deployment", "test-ns", "autoscaler")})
	if err != nil {
		t.Fatalf("ManifestFrom() error: %v", err)
	}
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeServingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}),
		}},
	}

	failSliceDelete := true
	kubeClient.PrependReactor("delete", "endpointslices", func(clienttesting.Action) (bool, runtime.Object, error) {
		if failSliceDelete {
			return true, nil, errors.New("injected EndpointSlice deletion failure")
		}
		return false, nil, nil
	})

	err = deleteRemoteRuntimeResources(context.Background(), kubeClient, &manifest, instance)
	if err == nil || !strings.Contains(err.Error(), "injected EndpointSlice deletion failure") {
		t.Fatalf("deleteRemoteRuntimeResources() error = %v, want injected failure", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(context.Background(), resourceName, metav1.GetOptions{}); err != nil {
		t.Fatalf("Lease was deleted before dependent cleanup succeeded: %v", err)
	}

	failSliceDelete = false
	if err := deleteRemoteRuntimeResources(context.Background(), kubeClient, &manifest, instance); err != nil {
		t.Fatalf("deleteRemoteRuntimeResources() retry error: %v", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(context.Background(), resourceName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Lease Get() after retry error = %v, want NotFound", err)
	}
	if _, err := kubeClient.DiscoveryV1().EndpointSlices("test-ns").Get(context.Background(), slice.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("EndpointSlice Get() after retry error = %v, want NotFound", err)
	}
}

func TestDeleteRemoteRuntimeResources_UsesAnchorOwnedLeaseWithEmptyHolder(t *testing.T) {
	instance := &v1beta1.KnativeEventing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeEventingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", testSpokeRef),
		}},
	}
	anchor := anchorConfigMap(instance)
	ownedLease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name:      "inmemorychannel-dispatcher.00-of-01",
		Namespace: "test-ns",
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(anchor, corev1.SchemeGroupVersion.WithKind("ConfigMap")),
		},
	}}
	unmanagedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "unmanaged.00-of-01", Namespace: "test-ns"},
	}
	ownedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: ownedLease.Name, Namespace: "test-ns"},
	}
	unmanagedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: unmanagedLease.Name, Namespace: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(
		anchor, ownedLease, unmanagedLease, ownedService, unmanagedService)

	names, err := remoteRuntimeResourceNames(t.Context(), kubeClient, nil, instance)
	if err != nil {
		t.Fatalf("remoteRuntimeResourceNames() error: %v", err)
	}
	if diff := cmp.Diff([]string{ownedLease.Name}, names); diff != "" {
		t.Fatalf("remoteRuntimeResourceNames() (-want, +got):\n%s", diff)
	}
	if err := deleteRemoteRuntimeResources(t.Context(), kubeClient, nil, instance); err != nil {
		t.Fatalf("deleteRemoteRuntimeResources() error: %v", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(
		t.Context(), ownedLease.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("owned Lease Get() error = %v, want NotFound", err)
	}
	if _, err := kubeClient.CoreV1().Services("test-ns").Get(
		t.Context(), ownedService.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("owned Service Get() error = %v, want NotFound", err)
	}
	if _, err := kubeClient.CoordinationV1().Leases("test-ns").Get(
		t.Context(), unmanagedLease.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("unmanaged Lease Get() error = %v, want nil", err)
	}
	if _, err := kubeClient.CoreV1().Services("test-ns").Get(
		t.Context(), unmanagedService.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("unmanaged Service Get() error = %v, want nil", err)
	}
}

// Without an installed manifest the anchor's Deployments identify the Leases, and the
// holder Pod may already have been replaced by one from a newer ReplicaSet.
func TestRemoteRuntimeResourceNames_FallsBackToAnchorOwnedDeployments(t *testing.T) {
	staleHolder := "imc-dispatcher-oldhash-abcde_01234567"
	unmanagedHolder := "external-controller-oldhash-abcde_01234567"
	instance := &v1beta1.KnativeEventing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub-ns", Name: "test"},
		Spec: v1beta1.KnativeEventingSpec{CommonSpec: base.CommonSpec{
			Placement: testPlacement("test-ns", base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}),
		}},
	}
	anchor := anchorConfigMap(instance)
	unmanagedDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "external-controller", Namespace: "test-ns"},
	}
	managedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "inmemorychannel-dispatcher.00-of-01", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &staleHolder},
	}
	largeBucketLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "inmemorychannel-dispatcher.100-of-100", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &staleHolder},
	}
	unmanagedLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "external-controller.00-of-01", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &unmanagedHolder},
	}
	prefixOnlyLease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "imc-dispatcher-metrics", Namespace: "test-ns"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &staleHolder},
	}
	kubeClient := fake.NewSimpleClientset(anchor, anchorOwnedDeployment(anchor, "imc-dispatcher"),
		unmanagedDeployment, managedLease, largeBucketLease, unmanagedLease, prefixOnlyLease)

	names, err := remoteRuntimeResourceNames(context.Background(), kubeClient, nil, instance)
	if err != nil {
		t.Fatalf("remoteRuntimeResourceNames() error: %v", err)
	}
	got := make(map[string]bool, len(names))
	for _, name := range names {
		got[name] = true
	}
	if len(names) != 2 || !got[managedLease.Name] || !got[largeBucketLease.Name] {
		t.Fatalf("remoteRuntimeResourceNames() = %v, want [%s %s]", names, managedLease.Name, largeBucketLease.Name)
	}
}

func TestConfigEqual(t *testing.T) {
	cfg := &rest.Config{
		Host:            "https://example.com",
		BearerToken:     "token",
		BearerTokenFile: "/path/to/token",
		Username:        "user",
		Password:        "pass",
	}
	same := &rest.Config{
		Host:            "https://example.com",
		BearerToken:     "token",
		BearerTokenFile: "/path/to/token",
		Username:        "user",
		Password:        "pass",
	}
	if !configEqual(cfg, same) {
		t.Fatal("configEqual() = false, want true")
	}

	if configEqual(cfg, &rest.Config{Host: "https://other.com", BearerToken: "token"}) {
		t.Fatal("configEqual(different Host) = true, want false")
	}

	if configEqual(cfg, &rest.Config{Host: "https://example.com", BearerToken: "other-token"}) {
		t.Fatal("configEqual(different BearerToken) = true, want false")
	}

	if configEqual(cfg, &rest.Config{
		Host:            "https://example.com",
		TLSClientConfig: rest.TLSClientConfig{CertFile: "/path/to/cert"},
	}) {
		t.Fatal("configEqual(different TLSClientConfig) = true, want false")
	}
}

func TestSameClusterProfile(t *testing.T) {
	tests := []struct {
		name string
		a, b *base.ClusterProfileReference
		want bool
	}{
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
		{
			name: "a nil",
			a:    nil,
			b:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			want: false,
		},
		{
			name: "same",
			a:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			b:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			want: true,
		},
		{
			name: "different",
			a:    &base.ClusterProfileReference{Namespace: "ns", Name: "name1"},
			b:    &base.ClusterProfileReference{Namespace: "ns", Name: "name2"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SameClusterProfile(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("SameClusterProfile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClusterProvider_ClosedShortCircuit(t *testing.T) {
	ctx := t.Context()

	p := &ClusterProvider{
		entries:       make(map[string]*clusterEntry),
		access:        NoOpClusterProfileAccess{},
		controllerCtx: ctx,
	}

	p.CloseAll()

	_, reason, err := p.GetOrRefresh(ctx, "fleet", "spoke1")
	if !errors.Is(err, errClusterProviderClosed) {
		t.Fatalf("GetOrRefresh after CloseAll = %v, want errClusterProviderClosed", err)
	}
	if reason != base.ReasonClusterProviderClosed {
		t.Fatalf("GetOrRefresh reason = %q, want %q", reason, base.ReasonClusterProviderClosed)
	}

	reason, err = p.Refresh(ctx, "fleet", "spoke1")
	if !errors.Is(err, errClusterProviderClosed) {
		t.Fatalf("Refresh after CloseAll = %v, want errClusterProviderClosed", err)
	}
	if reason != base.ReasonClusterProviderClosed {
		t.Fatalf("Refresh reason = %q, want %q", reason, base.ReasonClusterProviderClosed)
	}
}

func TestEnsureAnchorConfigMap_NamespaceLabels(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				NamespaceConfiguration: &base.NamespaceConfiguration{
					Labels: map[string]string{
						"team":                         "platform",
						"app.kubernetes.io/managed-by": "should-be-overwritten",
					},
					Annotations: map[string]string{
						"docs": "https://example.com/knative",
					},
				},
			},
		},
	}

	kubeClient := fake.NewSimpleClientset()

	ctx := context.Background()
	if _, err := EnsureAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("EnsureAnchorConfigMap() error: %v", err)
	}

	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, "test-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := ns.Labels["team"]; got != "platform" {
		t.Errorf("namespace label team = %q, want %q", got, "platform")
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "knative-operator" {
		t.Errorf("namespace label managed-by = %q, want %q (operator must overwrite CR-provided value)",
			got, "knative-operator")
	}
	if got := ns.Annotations["docs"]; got != "https://example.com/knative" {
		t.Errorf("namespace annotation docs = %q, want %q", got, "https://example.com/knative")
	}
}

func TestEnsureAnchorConfigMap_NamespaceLabels_ExistingUnchanged(t *testing.T) {
	existingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			Labels: map[string]string{
				"preserved-label": "original",
			},
			Annotations: map[string]string{
				"preserved-annotation": "original",
			},
		},
	}
	kubeClient := fake.NewSimpleClientset(existingNS)

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				NamespaceConfiguration: &base.NamespaceConfiguration{
					Labels: map[string]string{
						"team": "platform",
					},
					Annotations: map[string]string{
						"docs": "https://example.com/knative",
					},
				},
			},
		},
	}

	ctx := context.Background()
	if _, err := EnsureAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("EnsureAnchorConfigMap() error: %v", err)
	}

	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, "test-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := ns.Labels["preserved-label"]; got != "original" {
		t.Errorf("namespace label preserved-label = %q, want %q", got, "original")
	}
	if _, ok := ns.Labels["team"]; ok {
		t.Errorf("namespace should not have acquired CR label team=platform on existing namespace, got labels: %v",
			ns.Labels)
	}
	if got := ns.Annotations["preserved-annotation"]; got != "original" {
		t.Errorf("namespace annotation preserved-annotation = %q, want %q", got, "original")
	}
	if _, ok := ns.Annotations["docs"]; ok {
		t.Errorf("namespace should not have acquired CR annotation docs on existing namespace, got annotations: %v",
			ns.Annotations)
	}
}

func TestShouldFinalizeClusterScoped(t *testing.T) {
	ref := &base.ClusterProfileReference{Namespace: "fleet", Name: "spoke1"}

	tests := []struct {
		name       string
		components []base.KComponent
		original   base.KComponent
		want       bool
	}{
		{
			name:       "no other components",
			components: []base.KComponent{},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
			},
			want: true,
		},
		{
			name: "another alive component with same cluster profile",
			components: []base.KComponent{
				&v1beta1.KnativeServing{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks-other"},
					Spec: v1beta1.KnativeServingSpec{
						CommonSpec: base.CommonSpec{Placement: testPlacement("knative-serving", *ref)},
					},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
				Spec: v1beta1.KnativeServingSpec{
					CommonSpec: base.CommonSpec{Placement: testPlacement("knative-serving", *ref)},
				},
			},
			want: false,
		},
		{
			name: "legacy and placement use the same cluster profile",
			components: []base.KComponent{
				&v1beta1.KnativeServing{
					ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving", Name: "ks-other"},
					Spec: v1beta1.KnativeServingSpec{
						CommonSpec: base.CommonSpec{ClusterProfileRef: ref},
					},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "fleet-workloads", Name: "ks"},
				Spec: v1beta1.KnativeServingSpec{
					CommonSpec: base.CommonSpec{Placement: testPlacement("knative-serving", *ref)},
				},
			},
			want: false,
		},
		{
			name: "another alive component with different cluster profile",
			components: []base.KComponent{
				&v1beta1.KnativeServing{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks-other"},
					Spec: v1beta1.KnativeServingSpec{
						CommonSpec: base.CommonSpec{Placement: testPlacement("knative-serving", base.ClusterProfileReference{
							Namespace: "fleet", Name: "spoke2",
						})},
					},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
				Spec: v1beta1.KnativeServingSpec{
					CommonSpec: base.CommonSpec{Placement: testPlacement("knative-serving", *ref)},
				},
			},
			want: true,
		},
		{
			name: "both local (nil refs), another alive",
			components: []base.KComponent{
				&v1beta1.KnativeServing{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks-other"},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldFinalizeClusterScoped(tt.components, tt.original)
			if got != tt.want {
				t.Fatalf("ShouldFinalizeClusterScoped() = %v, want %v", got, tt.want)
			}
		})
	}
}

// stubClientFactory returns predetermined clients without network I/O.
type stubClientFactory struct {
	mfErr      error
	kubeErr    error
	kubeClient kubernetes.Interface
	mfCount    atomic.Int32
	kubeCount  atomic.Int32
}

func (s *stubClientFactory) NewMfClient(*rest.Config) (mf.Client, error) {
	s.mfCount.Add(1)
	if s.mfErr != nil {
		return nil, s.mfErr
	}
	return fakeMfClient{}, nil
}

func (s *stubClientFactory) NewKubeClient(*rest.Config) (kubernetes.Interface, error) {
	s.kubeCount.Add(1)
	if s.kubeErr != nil {
		return nil, s.kubeErr
	}
	if s.kubeClient != nil {
		return s.kubeClient, nil
	}
	return fake.NewSimpleClientset(), nil
}

// fakeMfClient is a no-op manifestival client for tests that don't exercise mf I/O.
type fakeMfClient struct{}

func (fakeMfClient) Create(_ *unstructured.Unstructured, _ ...mf.ApplyOption) error { return nil }
func (fakeMfClient) Update(_ *unstructured.Unstructured, _ ...mf.ApplyOption) error { return nil }
func (fakeMfClient) Delete(_ *unstructured.Unstructured, _ ...mf.DeleteOption) error {
	return nil
}

func (fakeMfClient) Get(_ *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, nil
}

var _ mf.Client = fakeMfClient{}

// blockingAccess holds BuildConfigFromCP open until release() is closed; used for dedup testing.
type blockingAccess struct {
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	count int
	seen  bool
}

func (b *blockingAccess) BuildConfigFromCP(cp *clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
	b.mu.Lock()
	b.count++
	first := !b.seen
	b.seen = true
	b.mu.Unlock()
	if first {
		close(b.entered)
	}
	host := cp.Annotations["test-host"]
	if host == "" {
		host = "https://blocked.example.com"
	}
	<-b.release
	return &rest.Config{Host: host}, nil
}

func (b *blockingAccess) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

func TestDoRefresh_Concurrency_SameKeyDeduped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	access := &blockingAccess{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	factory := &stubClientFactory{}
	provider := newTestProviderWithStubAccess(&stubAccess{}, readyClusterProfile("fleet", "worker"))
	provider.access = access
	provider.clientFactory = factory

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)

	go func() {
		defer wg.Done()
		_, err := provider.Refresh(ctx, "fleet", "worker")
		errs[0] = err
	}()

	select {
	case <-access.entered:
	case <-time.After(5 * time.Second):
		close(access.release)
		t.Fatal("leader goroutine did not enter BuildConfigFromCP within 5s")
	}

	followerReady := make(chan struct{})
	go func() {
		defer wg.Done()
		close(followerReady)
		_, err := provider.Refresh(ctx, "fleet", "worker")
		errs[1] = err
	}()
	<-followerReady

	// A second in-flight call would still be blocked in BuildConfigFromCP; on dedup calls() stays at 1.
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancelDeadline)
	for deadlineCtx.Err() == nil {
		if access.calls() != 1 {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadlineCtx.Done():
		}
	}
	if got := access.calls(); got != 1 {
		close(access.release)
		wg.Wait()
		t.Fatalf("BuildConfigFromCP calls before release = %d, want 1", got)
	}

	close(access.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Refresh() = %v, want nil", i, err)
		}
	}
	if got := access.calls(); got != 1 {
		t.Fatalf("BuildConfigFromCP final calls = %d, want 1", got)
	}
	if got := factory.mfCount.Load(); got != 1 {
		t.Fatalf("NewMfClient calls = %d, want 1", got)
	}
	if got := factory.kubeCount.Load(); got != 1 {
		t.Fatalf("NewKubeClient calls = %d, want 1", got)
	}
}

func TestDoRefresh_DiscardsClientInvalidatedDuringBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	oldCP := readyClusterProfile("fleet", "worker")
	oldCP.Annotations = map[string]string{"test-host": "https://old.example.com"}
	access := &blockingAccess{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(access.release) }) }
	t.Cleanup(release)

	factory := &stubClientFactory{}
	provider := newTestProviderWithStubAccess(&stubAccess{}, oldCP)
	provider.access = access
	provider.clientFactory = factory

	type refreshResult struct {
		reason string
		err    error
	}
	result := make(chan refreshResult, 1)
	go func() {
		reason, err := provider.Refresh(ctx, "fleet", "worker")
		result <- refreshResult{reason: reason, err: err}
	}()

	select {
	case <-access.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh did not reach BuildConfigFromCP")
	}

	newCP := oldCP.DeepCopy()
	newCP.Annotations["test-host"] = "https://new.example.com"
	newCP.Status.AccessProviders = []clusterinventoryv1alpha1.AccessProvider{{Name: "updated"}}
	if _, err := provider.ciClient.ApisV1alpha1().ClusterProfiles("fleet").Update(
		ctx, newCP, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update ClusterProfile: %v", err)
	}
	provider.handleUpdate(oldCP, newCP)
	release()

	var got refreshResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh did not finish after BuildConfigFromCP was released")
	}
	if !errors.Is(got.err, errClusterProfileChanged) {
		t.Fatalf("Refresh() error = %v, want errClusterProfileChanged", got.err)
	}
	if got.reason != base.ReasonClusterProfileUnavailable {
		t.Fatalf("Refresh() reason = %q, want %q", got.reason, base.ReasonClusterProfileUnavailable)
	}
	if _, _, err := provider.Get(ctx, "fleet/worker"); !errors.Is(err, errClusterNotResolved) {
		t.Fatalf("Get() after invalidated refresh = %v, want errClusterNotResolved", err)
	}

	entry, reason, err := provider.GetOrRefresh(ctx, "fleet", "worker")
	if err != nil {
		t.Fatalf("GetOrRefresh() after invalidation = %v (reason %q)", err, reason)
	}
	if gotHost := entry.RestConfig().Host; gotHost != "https://new.example.com" {
		t.Fatalf("refreshed Host = %q, want https://new.example.com", gotHost)
	}
	if gotCalls := access.calls(); gotCalls != 2 {
		t.Fatalf("BuildConfigFromCP calls = %d, want 2", gotCalls)
	}
	if gotClients := factory.kubeCount.Load(); gotClients != 2 {
		t.Fatalf("NewKubeClient calls = %d, want 2", gotClients)
	}
}

func TestDoRefresh_Concurrency_DifferentKeysIndependent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	factory := &stubClientFactory{}
	provider := newTestProviderWithStubAccess(
		&stubAccess{},
		readyClusterProfile("fleet", "worker-a"),
		readyClusterProfile("fleet", "worker-b"),
	)
	provider.clientFactory = factory

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)
	names := []string{"worker-a", "worker-b"}
	for i, name := range names {
		go func() {
			defer wg.Done()
			_, err := provider.Refresh(ctx, "fleet", name)
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d (%s): Refresh() = %v, want nil", i, names[i], err)
		}
	}
	if got := factory.mfCount.Load(); got != 2 {
		t.Fatalf("NewMfClient calls = %d, want 2", got)
	}
	if got := factory.kubeCount.Load(); got != 2 {
		t.Fatalf("NewKubeClient calls = %d, want 2", got)
	}
}

func TestEnsureAnchorConfigMap_NamespaceExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	existingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "existing-ns"}}
	kubeClient := fake.NewSimpleClientset(existingNS)

	var nsCreated atomic.Bool
	kubeClient.PrependReactor("create", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		nsCreated.Store(true)
		return false, nil, nil
	})

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "existing-ns", Name: "test"},
	}
	if _, err := EnsureAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("EnsureAnchorConfigMap() = %v, want nil", err)
	}
	if nsCreated.Load() {
		t.Fatal("Namespaces().Create was invoked for an existing namespace")
	}
	if _, err := kubeClient.CoreV1().ConfigMaps("existing-ns").
		Get(ctx, "knativeserving-test-root-owner", metav1.GetOptions{}); err != nil {
		t.Fatalf("anchor ConfigMap was not created: %v", err)
	}
}

func TestDoRefresh_ClientCreationFailure_Manifestival(t *testing.T) {
	provider := newTestProviderWithStubAccess(&stubAccess{}, readyClusterProfile("fleet", "worker"))
	mfErr := errors.New("mf boom")
	factory := &stubClientFactory{mfErr: mfErr}
	provider.clientFactory = factory

	reason, err := provider.Refresh(context.Background(), "fleet", "worker")
	if err == nil {
		t.Fatal("Refresh() = nil, want error")
	}
	if reason != base.ReasonRemoteClientCreationFailed {
		t.Errorf("reason = %q, want %q", reason, base.ReasonRemoteClientCreationFailed)
	}
	if !errors.Is(err, mfErr) {
		t.Errorf("error chain does not wrap mfErr: %v", err)
	}
	if !strings.Contains(err.Error(), "manifestival") {
		t.Errorf("error message = %q, want it to mention %q", err.Error(), "manifestival")
	}
	if got := factory.mfCount.Load(); got != 1 {
		t.Errorf("NewMfClient calls = %d, want 1", got)
	}
	if got := factory.kubeCount.Load(); got != 0 {
		t.Errorf("NewKubeClient calls = %d, want 0 (should short-circuit before kube client)", got)
	}
	if got := len(provider.entries); got != 0 {
		t.Errorf("provider.entries size = %d, want 0 (failure must not cache)", got)
	}
}

func TestDoRefresh_ClientCreationFailure_Kube(t *testing.T) {
	provider := newTestProviderWithStubAccess(&stubAccess{}, readyClusterProfile("fleet", "worker"))
	kubeErr := errors.New("kube boom")
	factory := &stubClientFactory{kubeErr: kubeErr}
	provider.clientFactory = factory

	reason, err := provider.Refresh(context.Background(), "fleet", "worker")
	if err == nil {
		t.Fatal("Refresh() = nil, want error")
	}
	if reason != base.ReasonRemoteClientCreationFailed {
		t.Errorf("reason = %q, want %q", reason, base.ReasonRemoteClientCreationFailed)
	}
	if !errors.Is(err, kubeErr) {
		t.Errorf("error chain does not wrap kubeErr: %v", err)
	}
	if !strings.Contains(err.Error(), "kube") {
		t.Errorf("error message = %q, want it to mention %q", err.Error(), "kube")
	}
	if got := factory.mfCount.Load(); got != 1 {
		t.Errorf("NewMfClient calls = %d, want 1", got)
	}
	if got := factory.kubeCount.Load(); got != 1 {
		t.Errorf("NewKubeClient calls = %d, want 1", got)
	}
	if got := len(provider.entries); got != 0 {
		t.Errorf("provider.entries size = %d, want 0 (failure must not cache)", got)
	}
}
func TestClusterProvider_ClosedShortCircuit_Concurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	access := &blockingAccess{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	factory := &stubClientFactory{}
	provider := newTestProviderWithStubAccess(&stubAccess{}, readyClusterProfile("fleet", "worker"))
	provider.access = access
	provider.clientFactory = factory

	var wg sync.WaitGroup
	wg.Add(1)
	var (
		gotReason string
		gotErr    error
	)
	go func() {
		defer wg.Done()
		gotReason, gotErr = provider.Refresh(ctx, "fleet", "worker")
	}()

	select {
	case <-access.entered:
	case <-time.After(5 * time.Second):
		close(access.release)
		wg.Wait()
		t.Fatal("Refresh did not reach BuildConfigFromCP within 5s")
	}

	provider.CloseAll()
	close(access.release)
	wg.Wait()

	if gotErr == nil {
		t.Fatal("Refresh() = nil, want error after CloseAll")
	}
	if !errors.Is(gotErr, errClusterProviderClosed) {
		t.Errorf("error = %v, want wrapping errClusterProviderClosed", gotErr)
	}
	if gotReason != base.ReasonClusterProviderClosed {
		t.Errorf("reason = %q, want %q", gotReason, base.ReasonClusterProviderClosed)
	}
	if got := len(provider.entries); got != 0 {
		t.Errorf("provider.entries size = %d, want 0 (closed provider must not retain entries)", got)
	}
}
