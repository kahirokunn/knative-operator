/*
Copyright 2026 The Knative Authors

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
	"testing"
	"time"

	mf "github.com/manifestival/manifestival"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
	"knative.dev/pkg/apis"

	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	fakeciclient "sigs.k8s.io/cluster-inventory-api/client/clientset/versioned/fake"
)

// stubAccess is an in-memory ClusterProfileAccess for unit tests. It counts
// calls and runs an arbitrary buildFn so tests can simulate success, failure
// and concurrent refresh scenarios.
type stubAccess struct {
	mu         sync.Mutex
	buildCount int
	buildFn    func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error)
}

func (s *stubAccess) BuildConfigFromCP(cp *clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
	s.mu.Lock()
	s.buildCount++
	s.mu.Unlock()
	if s.buildFn == nil {
		return &rest.Config{Host: "https://stub.example.com"}, nil
	}
	return s.buildFn(cp)
}

func (s *stubAccess) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildCount
}

// readyClusterProfile returns a ClusterProfile whose ControlPlaneHealthy
// condition is True, which is what isClusterProfileReady requires.
func readyClusterProfile(namespace, name string) *clusterinventoryv1alpha1.ClusterProfile {
	return &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Status: clusterinventoryv1alpha1.ClusterProfileStatus{
			Conditions: []metav1.Condition{
				{
					Type:               clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
					Message:            "Test ClusterProfile is ready",
				},
			},
		},
	}
}

// newTestProviderWithStubAccess constructs a ClusterProvider suitable for unit
// tests that exercise Refresh / GetOrRefresh. The provided ClusterProfiles are
// seeded into the fake cluster-inventory client.
func newTestProviderWithStubAccess(stub *stubAccess, cps ...*clusterinventoryv1alpha1.ClusterProfile) *ClusterProvider {
	objs := make([]runtime.Object, 0, len(cps))
	for _, cp := range cps {
		objs = append(objs, cp)
	}
	ci := fakeciclient.NewSimpleClientset(objs...)
	return &ClusterProvider{
		entries:       map[string]*clusterEntry{},
		access:        stub,
		ciClient:      ci,
		controllerCtx: context.Background(),
		remoteTimeout: DefaultRemoteClusterTimeout,
	}
}

func TestNoOpClusterProfileAccess_ReturnsDisabledError(t *testing.T) {
	noop := NoOpClusterProfileAccess{}
	cp := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "worker"},
	}
	cfg, err := noop.BuildConfigFromCP(cp)
	if cfg != nil {
		t.Fatalf("Expected nil rest.Config, got %+v", cfg)
	}
	if !errors.Is(err, ErrMulticlusterDisabled) {
		t.Fatalf("Expected ErrMulticlusterDisabled, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--clusterprofile-provider-file") {
		t.Fatalf("Expected error message to mention the flag, got: %v", err)
	}
}

func TestClusterProvider_Refresh_AccessFailure(t *testing.T) {
	stub := &stubAccess{
		buildFn: func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
			return nil, errors.New("simulated exec-plugin failure")
		},
	}
	provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))

	if _, err := provider.Refresh(context.Background(), "fleet", "worker"); err == nil {
		t.Fatal("Expected Refresh to return error when access provider fails, got nil")
	}
	if stub.count() != 1 {
		t.Fatalf("Expected BuildConfigFromCP to be called exactly once, got %d", stub.count())
	}
	// cache must remain empty on failure
	if _, _, err := provider.Get(context.Background(), "fleet/worker"); !errors.Is(err, ErrClusterNotResolved) {
		t.Fatalf("Expected cache miss after failed refresh, got: %v", err)
	}
}

func TestClusterProvider_Refresh_NoOpAccessReturnsDisabledError(t *testing.T) {
	ci := fakeciclient.NewSimpleClientset(readyClusterProfile("fleet", "worker"))
	provider := &ClusterProvider{
		entries:       map[string]*clusterEntry{},
		access:        NoOpClusterProfileAccess{},
		ciClient:      ci,
		controllerCtx: context.Background(),
		remoteTimeout: DefaultRemoteClusterTimeout,
	}
	_, err := provider.Refresh(context.Background(), "fleet", "worker")
	if !errors.Is(err, ErrMulticlusterDisabled) {
		t.Fatalf("Expected Refresh to wrap ErrMulticlusterDisabled, got: %v", err)
	}
}

func TestClusterProvider_GetOrRefresh_CacheHit(t *testing.T) {
	stub := &stubAccess{} // default buildFn returns success — must NOT be called
	provider := newTestProviderWithStubAccess(stub)

	entry := newTestClusterEntry("https://cached.example.com")
	provider.entries["fleet/worker"] = entry

	got, _, err := provider.GetOrRefresh(context.Background(), "fleet", "worker")
	if err != nil {
		t.Fatalf("GetOrRefresh() error = %v, want nil", err)
	}
	if got.RestConfig().Host != "https://cached.example.com" {
		t.Fatalf("Got unexpected entry (Host=%q)", got.RestConfig().Host)
	}
	if stub.count() != 0 {
		t.Fatalf("Cache hit must not invoke BuildConfigFromCP, got %d calls", stub.count())
	}
}

func TestClusterProvider_GetOrRefresh_CacheMiss_RefreshSucceeds(t *testing.T) {
	stub := &stubAccess{
		buildFn: func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
			return &rest.Config{Host: "https://fresh.example.com"}, nil
		},
	}
	provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))

	got, _, err := provider.GetOrRefresh(context.Background(), "fleet", "worker")
	if err != nil {
		t.Fatalf("GetOrRefresh() error = %v, want nil", err)
	}
	if got == nil || got.RestConfig() == nil {
		t.Fatalf("Got nil entry or nil RestConfig")
	}
	if got.RestConfig().Host != "https://fresh.example.com" {
		t.Fatalf("Host = %q, want https://fresh.example.com", got.RestConfig().Host)
	}
	if stub.count() != 1 {
		t.Fatalf("Expected 1 BuildConfigFromCP call, got %d", stub.count())
	}

	// Second call should hit cache.
	if _, _, err := provider.GetOrRefresh(context.Background(), "fleet", "worker"); err != nil {
		t.Fatalf("second GetOrRefresh() error = %v", err)
	}
	if stub.count() != 1 {
		t.Fatalf("Second call should hit cache; BuildConfigFromCP called %d times", stub.count())
	}
}

func TestClusterProvider_GetOrRefresh_CacheMiss_RefreshFails(t *testing.T) {
	stub := &stubAccess{
		buildFn: func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
			return nil, errors.New("simulated access failure")
		},
	}
	provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))

	_, _, err := provider.GetOrRefresh(context.Background(), "fleet", "worker")
	if err == nil {
		t.Fatal("Expected GetOrRefresh to return error when Refresh fails")
	}
	if stub.count() != 1 {
		t.Fatalf("Expected 1 BuildConfigFromCP call, got %d", stub.count())
	}

	// On a second call the provider should try to refresh again (no caching of errors).
	_, _, err = provider.GetOrRefresh(context.Background(), "fleet", "worker")
	if err == nil {
		t.Fatal("Expected second GetOrRefresh to return error")
	}
	if stub.count() != 2 {
		t.Fatalf("Expected retry to call BuildConfigFromCP a second time, got %d", stub.count())
	}
}

func TestClusterProvider_GetOrRefresh_Singleflight(t *testing.T) {
	start := make(chan struct{})
	stub := &stubAccess{
		buildFn: func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
			<-start
			time.Sleep(50 * time.Millisecond)
			return &rest.Config{Host: "https://singleflight.example.com"}, nil
		},
	}
	provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := provider.GetOrRefresh(context.Background(), "fleet", "worker")
			errs <- err
		}()
	}
	// Give all goroutines time to enter Refresh before releasing buildFn.
	time.Sleep(20 * time.Millisecond)
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("GetOrRefresh() returned error: %v", err)
		}
	}
	// singleflight should collapse concurrent refreshes. Allow a small slack
	// (>=1 but small) in case the goroutines are scheduled so that some
	// callers observe the cached entry after the first refresh completes.
	if got := stub.count(); got == 0 || got > 2 {
		t.Fatalf("Expected singleflight to collapse refreshes to 1-2 calls, got %d", got)
	}
}

func TestHandleClusterProfileEvent_DoesNotCallRefresh(t *testing.T) {
	stub := &stubAccess{}
	provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))

	// Snapshot the cache state before the event.
	provider.mu.RLock()
	before := len(provider.entries)
	provider.mu.RUnlock()

	var notified int
	provider.RegisterListener(ClusterProfileListener{
		ListCRs: func(ns, name string) []types.NamespacedName {
			if ns != "fleet" || name != "worker" {
				t.Errorf("Listener called with unexpected key %s/%s", ns, name)
			}
			notified++
			return []types.NamespacedName{{Namespace: "knative-serving", Name: "default"}}
		},
		EnqueueKey: func(types.NamespacedName) {},
	})

	provider.handleClusterProfileEvent("fleet", "worker")

	if stub.count() != 0 {
		t.Fatalf("Handler must NOT call BuildConfigFromCP; got %d calls", stub.count())
	}
	provider.mu.RLock()
	after := len(provider.entries)
	provider.mu.RUnlock()
	if before != after {
		t.Fatalf("Handler must not mutate cache; before=%d after=%d", before, after)
	}
	if notified != 1 {
		t.Fatalf("Expected listener to be invoked exactly once, got %d", notified)
	}
}

// TestResolveTargetCluster_NoOpAccess verifies that when multi-cluster is
// disabled (provider file empty) but a CR specifies clusterProfileRef, the
// reconciler surfaces ErrMulticlusterDisabled via MarkTargetClusterNotResolved
// rather than panicking or crash-looping. (Plan §3.3.4 graceful path.)
func TestResolveTargetCluster_NoOpAccess(t *testing.T) {
	provider := &ClusterProvider{
		entries:       map[string]*clusterEntry{},
		access:        NoOpClusterProfileAccess{},
		ciClient:      fakeciclient.NewSimpleClientset(readyClusterProfile("fleet", "worker")),
		controllerCtx: context.Background(),
		remoteTimeout: DefaultRemoteClusterTimeout,
	}

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving", Name: "default"},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				ClusterProfileRef: &base.ClusterProfileReference{
					Namespace: "fleet",
					Name:      "worker",
				},
			},
		},
	}
	instance.Status.InitializeConditions()

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	var state ReconcileState
	stage := ResolveTargetCluster(provider, &state)
	if err := stage(context.Background(), &manifest, instance); err == nil {
		t.Fatal("Expected error when multi-cluster is disabled")
	} else if !errors.Is(err, ErrMulticlusterDisabled) {
		t.Fatalf("Expected wrapped ErrMulticlusterDisabled, got: %v", err)
	}

	cond := instance.Status.GetCondition(base.TargetClusterResolved)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("Expected TargetClusterResolved=False, got: %v", cond)
	}
	if !strings.Contains(cond.Message, "multi-cluster support is disabled") {
		t.Fatalf("Expected condition message to mention disable reason, got: %q", cond.Message)
	}
	assertTargetClusterNotResolved(t, &instance.Status,
		"MulticlusterDisabled", "multi-cluster support is disabled")
}

// conditionReader is satisfied by both KnativeServingStatus and
// KnativeEventingStatus, whose GetCondition methods are declared on the
// concrete lifecycle types rather than on base.KComponentStatus.
type conditionReader interface {
	GetCondition(apis.ConditionType) *apis.Condition
}

// assertTargetClusterNotResolved verifies that both TargetClusterResolved
// and InstallSucceeded are False, with the expected reason/message on
// TargetClusterResolved and the stable "TargetClusterUnavailable" reason
// on InstallSucceeded. Use this in every test that exercises the error
// path so a future change can't silently drop one of the two conditions.
func assertTargetClusterNotResolved(t *testing.T, status conditionReader, wantReason, wantMsgContains string) {
	t.Helper()
	tc := status.GetCondition(base.TargetClusterResolved)
	if tc == nil || tc.Status != corev1.ConditionFalse {
		t.Fatalf("TargetClusterResolved: want False, got %v", tc)
	}
	if tc.Reason != wantReason {
		t.Fatalf("TargetClusterResolved.Reason: want %q, got %q", wantReason, tc.Reason)
	}
	if wantMsgContains != "" && !strings.Contains(tc.Message, wantMsgContains) {
		t.Fatalf("TargetClusterResolved.Message: want substring %q, got %q", wantMsgContains, tc.Message)
	}
	is := status.GetCondition(base.InstallSucceeded)
	if is == nil || is.Status != corev1.ConditionFalse {
		t.Fatalf("InstallSucceeded: want False, got %v", is)
	}
	if is.Reason != "TargetClusterUnavailable" {
		t.Fatalf("InstallSucceeded.Reason: want %q, got %q", "TargetClusterUnavailable", is.Reason)
	}
}

// TestResolveTargetCluster_ReasonPropagation verifies that each failure
// mode in the resolve path surfaces a distinct machine-readable reason
// on TargetClusterResolved, while InstallSucceeded always carries the
// fixed "TargetClusterUnavailable" reason. See plan §5.2.
func TestResolveTargetCluster_ReasonPropagation(t *testing.T) {
	newInstance := func() *v1beta1.KnativeServing {
		inst := &v1beta1.KnativeServing{
			ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving", Name: "default"},
			Spec: v1beta1.KnativeServingSpec{
				CommonSpec: base.CommonSpec{
					ClusterProfileRef: &base.ClusterProfileReference{
						Namespace: "fleet",
						Name:      "worker",
					},
				},
			},
		}
		inst.Status.InitializeConditions()
		return inst
	}
	runResolve := func(t *testing.T, provider *ClusterProvider, inst *v1beta1.KnativeServing) {
		t.Helper()
		manifest, err := mf.ManifestFrom(mf.Slice{})
		if err != nil {
			t.Fatalf("Failed to create manifest: %v", err)
		}
		var state ReconcileState
		stage := ResolveTargetCluster(provider, &state)
		if err := stage(context.Background(), &manifest, inst); err == nil {
			t.Fatal("Expected error, got nil")
		}
	}

	t.Run("NilProvider", func(t *testing.T) {
		inst := newInstance()
		runResolve(t, nil, inst)
		assertTargetClusterNotResolved(t, &inst.Status,
			"ClusterProviderNotConfigured", "cluster provider not configured")
	})

	t.Run("Disabled", func(t *testing.T) {
		provider := &ClusterProvider{
			entries:       map[string]*clusterEntry{},
			access:        NoOpClusterProfileAccess{},
			ciClient:      fakeciclient.NewSimpleClientset(readyClusterProfile("fleet", "worker")),
			controllerCtx: context.Background(),
			remoteTimeout: DefaultRemoteClusterTimeout,
		}
		inst := newInstance()
		runResolve(t, provider, inst)
		assertTargetClusterNotResolved(t, &inst.Status,
			"MulticlusterDisabled", "multi-cluster support is disabled")
	})

	t.Run("ProfileNotFound", func(t *testing.T) {
		provider := newTestProviderWithStubAccess(&stubAccess{})
		inst := newInstance()
		runResolve(t, provider, inst)
		assertTargetClusterNotResolved(t, &inst.Status,
			"ClusterProfileNotFound", "failed to get ClusterProfile")
	})

	t.Run("ProfileNotReady", func(t *testing.T) {
		notReady := &clusterinventoryv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "worker"},
			Status: clusterinventoryv1alpha1.ClusterProfileStatus{
				Conditions: []metav1.Condition{
					{
						Type:               clusterinventoryv1alpha1.ClusterConditionControlPlaneHealthy,
						Status:             metav1.ConditionFalse,
						LastTransitionTime: metav1.Now(),
						Reason:             "Unhealthy",
						Message:            "control plane unhealthy",
					},
				},
			},
		}
		provider := newTestProviderWithStubAccess(&stubAccess{}, notReady)
		inst := newInstance()
		runResolve(t, provider, inst)
		assertTargetClusterNotResolved(t, &inst.Status,
			"ClusterProfileNotReady", "is not ready")
	})

	t.Run("AccessFailed", func(t *testing.T) {
		stub := &stubAccess{
			buildFn: func(*clusterinventoryv1alpha1.ClusterProfile) (*rest.Config, error) {
				return nil, errors.New("simulated exec-plugin failure")
			},
		}
		provider := newTestProviderWithStubAccess(stub, readyClusterProfile("fleet", "worker"))
		inst := newInstance()
		runResolve(t, provider, inst)
		assertTargetClusterNotResolved(t, &inst.Status,
			"AccessProviderFailed", "failed to build config from ClusterProfile")
	})

	t.Run("CacheColdFromFinalizer", func(t *testing.T) {
		provider := &ClusterProvider{
			entries:       map[string]*clusterEntry{},
			controllerCtx: context.Background(),
			remoteTimeout: DefaultRemoteClusterTimeout,
		}
		_, reason, err := provider.Get(context.Background(), "fleet/worker")
		if err == nil {
			t.Fatal("Expected error from Get on empty cache, got nil")
		}
		if !errors.Is(err, ErrClusterNotResolved) {
			t.Fatalf("Expected ErrClusterNotResolved, got: %v", err)
		}
		if reason != "ClusterProfileUnavailable" {
			t.Fatalf("reason = %q, want %q", reason, "ClusterProfileUnavailable")
		}
	})

	t.Run("RemoteStale", func(t *testing.T) {
		entryCtx, entryCancel := context.WithCancel(context.Background())
		entry := &clusterEntry{
			restConfig: &rest.Config{Host: "https://stale.example.com"},
			cancel:     entryCancel,
			ctx:        entryCtx,
		}
		provider := &ClusterProvider{
			entries:       map[string]*clusterEntry{"fleet/worker": entry},
			controllerCtx: context.Background(),
			remoteTimeout: DefaultRemoteClusterTimeout,
		}
		entryCancel() // mark stale
		_, reason, err := provider.Get(context.Background(), "fleet/worker")
		if err == nil {
			t.Fatal("Expected error from Get on stale entry, got nil")
		}
		if !errors.Is(err, ErrClusterStale) {
			t.Fatalf("Expected ErrClusterStale, got: %v", err)
		}
		if reason != "RemoteClusterStale" {
			t.Fatalf("reason = %q, want %q", reason, "RemoteClusterStale")
		}
	})
}
