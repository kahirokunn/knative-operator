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
	"net/http"
	"strings"
	"sync"
	"testing"

	mf "github.com/manifestival/manifestival"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
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
		t.Fatalf("Expected no error for nil ClusterProfileRef, got: %v", err)
	}

	if manifest.Client != origClient {
		t.Fatal("Expected manifest.Client to remain unchanged when ClusterProfileRef is nil")
	}

	if state.AnchorOwner != nil {
		t.Fatal("Expected state.AnchorOwner to remain nil when ClusterProfileRef is nil")
	}

	if state.IsRemote() {
		t.Fatal("Expected state.IsRemote() to be false when ClusterProfileRef is nil")
	}

	cond := instance.Status.GetCondition(base.TargetClusterResolved)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("Expected TargetClusterResolved to be True for nil ClusterProfileRef, got: %v", cond)
	}
}

func TestResolveTargetCluster_ClusterNotResolved(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				ClusterProfileRef: &base.ClusterProfileReference{
					Name:      "test-cluster",
					Namespace: "fleet-system",
				},
			},
		},
	}
	instance.Status.InitializeConditions()

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	// With GetOrRefresh, a cache miss now triggers a Refresh, which calls
	// the cluster-inventory API to look up the ClusterProfile. Seed an empty
	// fake client so the lookup fails with NotFound and the reconciler
	// bubbles the error up as TargetClusterNotResolved.
	provider := newTestProviderWithStubAccess(&stubAccess{})

	var state ReconcileState
	stage := ResolveTargetCluster(provider, &state)
	err = stage(context.Background(), &manifest, instance)
	if err == nil {
		t.Fatal("Expected error when cluster is not in cache, got nil")
	}
	if !strings.Contains(err.Error(), "failed to resolve target cluster") {
		t.Fatalf("Expected target cluster resolution error, got: %v", err)
	}
	assertTargetClusterNotResolved(t, &instance.Status,
		"ClusterProfileNotFound", "failed to get ClusterProfile")
}

func TestAnchorName(t *testing.T) {
	tests := []struct {
		name     string
		instance base.KComponent
		want     string
	}{
		{
			name: "KnativeServing",
			instance: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "knative-serving",
					Name:      "knative-serving",
				},
			},
			want: "knativeserving-knative-serving-root-owner",
		},
		{
			name: "KnativeEventing",
			instance: &v1beta1.KnativeEventing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "knative-eventing",
					Name:      "my-eventing",
				},
			},
			want: "knativeeventing-my-eventing-root-owner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnchorName(tt.instance)
			if got != tt.want {
				t.Fatalf("AnchorName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureAnchorConfigMap_Create(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
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

	if anchor.Labels["app.kubernetes.io/managed-by"] != "knative-operator" {
		t.Fatalf("Expected label app.kubernetes.io/managed-by=knative-operator, got %q",
			anchor.Labels["app.kubernetes.io/managed-by"])
	}
	if anchor.Labels["operator.knative.dev/cr-name"] != "test" {
		t.Fatalf("Expected label operator.knative.dev/cr-name=test, got %q",
			anchor.Labels["operator.knative.dev/cr-name"])
	}

	if anchor.Annotations["operator.knative.dev/anchor"] != "true" {
		t.Fatalf("Expected annotation operator.knative.dev/anchor=true, got %q",
			anchor.Annotations["operator.knative.dev/anchor"])
	}

	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, "test-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected namespace test-ns to exist, got error: %v", err)
	}
	if ns.Name != "test-ns" {
		t.Fatalf("namespace.Name = %q, want %q", ns.Name, "test-ns")
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
				"operator.knative.dev/anchor":  "true",
				"operator.knative.dev/warning": "Deleting this ConfigMap will trigger garbage collection of all managed namespace-scoped resources",
			},
		},
	}
	existingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(existingNS, existingAnchor)

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
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

func TestEnsureAnchorConfigMap_NamespaceAlreadyExists(t *testing.T) {
	existingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ns"},
	}
	kubeClient := fake.NewSimpleClientset(existingNS)

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
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

	nsList, err := kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list namespaces: %v", err)
	}
	count := 0
	for _, ns := range nsList.Items {
		if ns.Name == "test-ns" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Expected 1 namespace named test-ns, got %d", count)
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
			Namespace: "test-ns",
			Name:      "test",
		},
	}

	ctx := context.Background()
	if err := DeleteAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("DeleteAnchorConfigMap() error: %v", err)
	}

	_, err := kubeClient.CoreV1().ConfigMaps("test-ns").Get(ctx, "knativeserving-test-root-owner", metav1.GetOptions{})
	if err == nil {
		t.Fatal("Expected anchor ConfigMap to be deleted, but it still exists")
	}
}

func TestDeleteAnchorConfigMap_NotFound(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
		},
	}

	ctx := context.Background()
	if err := DeleteAnchorConfigMap(ctx, kubeClient, instance); err != nil {
		t.Fatalf("Expected no error for deleting non-existent anchor, got: %v", err)
	}
}

func newTestClusterEntry(host string) *clusterEntry {
	ctx, cancel := context.WithCancel(context.Background())
	return &clusterEntry{
		restConfig: &rest.Config{Host: host},
		cancel:     cancel,
		ctx:        ctx,
	}
}

func TestClusterProvider_Remove(t *testing.T) {
	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name": newTestClusterEntry("https://example.com"),
		},
	}

	provider.Remove("ns/name")
	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache after Remove, got %d entries", len(provider.entries))
	}
}

func TestClusterProvider_Remove_NonExistent(t *testing.T) {
	provider := &ClusterProvider{
		entries: make(map[string]*clusterEntry),
	}

	provider.Remove("ns/does-not-exist")
	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache, got %d entries", len(provider.entries))
	}
}

func TestClusterProvider_Remove_OnlyTargetKey(t *testing.T) {
	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name1": newTestClusterEntry("https://cluster1.example.com"),
			"ns/name2": newTestClusterEntry("https://cluster2.example.com"),
		},
	}

	provider.Remove("ns/name1")
	if len(provider.entries) != 1 {
		t.Errorf("expected 1 entry after Remove, got %d", len(provider.entries))
	}
	if _, ok := provider.entries["ns/name2"]; !ok {
		t.Error("expected entry ns/name2 to remain after removing ns/name1")
	}
	if _, ok := provider.entries["ns/name1"]; ok {
		t.Error("expected entry ns/name1 to be removed after Remove")
	}
}

func TestClusterProvider_ConcurrentAccess(t *testing.T) {
	provider := &ClusterProvider{
		entries: make(map[string]*clusterEntry),
	}
	provider.entries["ns/name"] = newTestClusterEntry("https://example.com")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			provider.Remove("ns/name")
		}()
	}
	wg.Wait()

	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache after concurrent Remove, got %d entries", len(provider.entries))
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

	// Expected labels should be present
	if anchor.Labels["app.kubernetes.io/managed-by"] != "knative-operator" {
		t.Errorf("expected managed-by label, got labels: %v", anchor.Labels)
	}
	if anchor.Labels["operator.knative.dev/cr-name"] != "knative-serving" {
		t.Errorf("expected cr-name label, got labels: %v", anchor.Labels)
	}
	// Old labels should be preserved (additive merge)
	if anchor.Labels["old-label"] != "old-value" {
		t.Errorf("expected old-label to be preserved with additive merge, got labels: %v", anchor.Labels)
	}

	// Expected annotations should be present
	if anchor.Annotations["operator.knative.dev/anchor"] != "true" {
		t.Errorf("expected anchor annotation, got annotations: %v", anchor.Annotations)
	}
	if anchor.Annotations["operator.knative.dev/warning"] == "" {
		t.Errorf("expected warning annotation, got annotations: %v", anchor.Annotations)
	}
	// Old annotations should be preserved (additive merge)
	if anchor.Annotations["old-annotation"] != "old-value" {
		t.Errorf("expected old-annotation to be preserved with additive merge, got annotations: %v", anchor.Annotations)
	}
}

func TestClusterProvider_Get_CacheMiss(t *testing.T) {
	provider := &ClusterProvider{
		entries: make(map[string]*clusterEntry),
	}

	_, _, err := provider.Get(context.Background(), "ns/name")
	if err == nil {
		t.Fatal("Expected error for cache miss, got nil")
	}
	if !errors.Is(err, ErrClusterNotResolved) {
		t.Fatalf("Expected ErrClusterNotResolved, got: %v", err)
	}
}

func TestClusterProvider_Get_Stale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel to make the entry stale

	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name": {
				restConfig: &rest.Config{Host: "https://example.com"},
				cancel:     cancel,
				ctx:        ctx,
			},
		},
	}

	_, _, err := provider.Get(context.Background(), "ns/name")
	if err == nil {
		t.Fatal("Expected error for stale entry, got nil")
	}
	if !errors.Is(err, ErrClusterStale) {
		t.Fatalf("Expected ErrClusterStale, got: %v", err)
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
			name: "b nil",
			a:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			b:    nil,
			want: false,
		},
		{
			name: "same",
			a:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			b:    &base.ClusterProfileReference{Namespace: "ns", Name: "name"},
			want: true,
		},
		{
			name: "different name",
			a:    &base.ClusterProfileReference{Namespace: "ns", Name: "name1"},
			b:    &base.ClusterProfileReference{Namespace: "ns", Name: "name2"},
			want: false,
		},
		{
			name: "different namespace",
			a:    &base.ClusterProfileReference{Namespace: "ns1", Name: "name"},
			b:    &base.ClusterProfileReference{Namespace: "ns2", Name: "name"},
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

func TestConfigEqual(t *testing.T) {
	base := &rest.Config{
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
	if !configEqual(base, same) {
		t.Fatal("Expected configEqual to return true for identical configs")
	}

	diffHost := &rest.Config{Host: "https://other.com", BearerToken: "token"}
	if configEqual(base, diffHost) {
		t.Fatal("Expected configEqual to return false for different Host")
	}

	diffToken := &rest.Config{Host: "https://example.com", BearerToken: "other-token"}
	if configEqual(base, diffToken) {
		t.Fatal("Expected configEqual to return false for different BearerToken")
	}
}

func TestClusterProvider_Get_Success(t *testing.T) {
	entry := newTestClusterEntry("https://example.com")
	entry.kubeClient = fake.NewSimpleClientset()

	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name": entry,
		},
	}

	clients, _, err := provider.Get(context.Background(), "ns/name")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if clients.RestConfig().Host != "https://example.com" {
		t.Fatalf("RestConfig().Host = %q, want %q", clients.RestConfig().Host, "https://example.com")
	}
	if clients.KubeClient() == nil {
		t.Fatal("Expected KubeClient() to be non-nil")
	}
	// MfClient was not set, so it should be nil
	if clients.MfClient() != nil {
		t.Fatal("Expected MfClient() to be nil when not set")
	}
}

func TestClusterProvider_Remove_ClosesContext(t *testing.T) {
	entry := newTestClusterEntry("https://example.com")

	if entry.ctx.Err() != nil {
		t.Fatal("Expected context to be alive before Remove")
	}

	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name": entry,
		},
	}

	provider.Remove("ns/name")

	if entry.ctx.Err() == nil {
		t.Fatal("Expected context to be canceled after Remove")
	}
}

func TestConfigEqual_TLSChange(t *testing.T) {
	a := &rest.Config{
		Host: "https://example.com",
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: "/path/to/cert",
		},
	}
	b := &rest.Config{
		Host: "https://example.com",
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: "/path/to/other-cert",
		},
	}
	if configEqual(a, b) {
		t.Fatal("Expected configEqual to return false for different TLSClientConfig")
	}
}

func TestConfigEqual_ExecProviderChange(t *testing.T) {
	a := &rest.Config{
		Host: "https://example.com",
		ExecProvider: &clientcmdapi.ExecConfig{
			Command: "aws",
		},
	}
	b := &rest.Config{
		Host: "https://example.com",
		ExecProvider: &clientcmdapi.ExecConfig{
			Command: "gcloud",
		},
	}
	if configEqual(a, b) {
		t.Fatal("Expected configEqual to return false for different ExecProvider")
	}
}

func TestConfigEqual_AuthProviderChange(t *testing.T) {
	a := &rest.Config{
		Host: "https://example.com",
		AuthProvider: &clientcmdapi.AuthProviderConfig{
			Name: "gcp",
		},
	}
	b := &rest.Config{
		Host: "https://example.com",
		AuthProvider: &clientcmdapi.AuthProviderConfig{
			Name: "azure",
		},
	}
	if configEqual(a, b) {
		t.Fatal("Expected configEqual to return false for different AuthProvider")
	}
}

func TestConfigEqual_ImpersonateChange(t *testing.T) {
	a := &rest.Config{
		Host: "https://example.com",
		Impersonate: rest.ImpersonationConfig{
			UserName: "admin",
		},
	}
	b := &rest.Config{
		Host: "https://example.com",
		Impersonate: rest.ImpersonationConfig{
			UserName: "developer",
		},
	}
	if configEqual(a, b) {
		t.Fatal("Expected configEqual to return false for different Impersonate")
	}
}

func TestConfigEqual_WrapTransportIgnored(t *testing.T) {
	a := &rest.Config{
		Host:          "https://example.com",
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper { return rt },
	}
	b := &rest.Config{
		Host:          "https://example.com",
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper { return rt },
	}
	if !configEqual(a, b) {
		t.Fatal("Expected configEqual to return true when configs differ only in WrapTransport")
	}

	c := &rest.Config{
		Host:          "https://example.com",
		WrapTransport: nil,
	}
	if !configEqual(a, c) {
		t.Fatal("Expected configEqual to return true when one has WrapTransport and other does not")
	}
}

func TestFinalizeRemoteCluster_AnchorNotFound(t *testing.T) {
	entry := newTestClusterEntry("https://example.com")
	entry.kubeClient = fake.NewSimpleClientset()

	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
		},
	}

	ctx := context.Background()
	err := FinalizeRemoteCluster(ctx, entry, nil, instance)
	if err != nil {
		t.Fatalf("Expected no error when anchor ConfigMap does not exist, got: %v", err)
	}
}

func TestReconcileState_IsRemote(t *testing.T) {
	state := &ReconcileState{}
	if state.IsRemote() {
		t.Fatal("Expected IsRemote() to be false when RemoteClients is nil")
	}

	entry := newTestClusterEntry("https://example.com")
	entry.kubeClient = fake.NewSimpleClientset()
	state.RemoteClients = entry
	if !state.IsRemote() {
		t.Fatal("Expected IsRemote() to be true when RemoteClients is set")
	}
}

func TestResolveTargetCluster_NilProvider(t *testing.T) {
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
		Spec: v1beta1.KnativeServingSpec{
			CommonSpec: base.CommonSpec{
				ClusterProfileRef: &base.ClusterProfileReference{
					Name:      "test-cluster",
					Namespace: "fleet-system",
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
	stage := ResolveTargetCluster(nil, &state)
	err = stage(context.Background(), &manifest, instance)
	if err == nil {
		t.Fatal("Expected error when provider is nil and ClusterProfileRef is set, got nil")
	}
	if !strings.Contains(err.Error(), "cluster provider not configured") {
		t.Fatalf("Expected 'cluster provider not configured' error, got: %v", err)
	}

	cond := instance.Status.GetCondition(base.TargetClusterResolved)
	if cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("Expected TargetClusterResolved to be False when provider is nil, got: %v", cond)
	}
	assertTargetClusterNotResolved(t, &instance.Status,
		"ClusterProviderNotConfigured", "cluster provider not configured")
}

func TestClusterEntry_Close_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := &clusterEntry{
		restConfig: &rest.Config{Host: "https://example.com"},
		kubeClient: fake.NewSimpleClientset(),
		cancel:     cancel,
		ctx:        ctx,
	}

	entry.Close()
	if entry.ctx.Err() == nil {
		t.Fatal("Expected context to be canceled after first Close")
	}

	// Second Close should not panic due to sync.Once.
	entry.Close()
	entry.Close()
}

func TestClusterProvider_CloseAll(t *testing.T) {
	entry1 := newTestClusterEntry("https://cluster1.example.com")
	entry2 := newTestClusterEntry("https://cluster2.example.com")

	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name1": entry1,
			"ns/name2": entry2,
		},
	}

	provider.CloseAll()

	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache after CloseAll, got %d entries", len(provider.entries))
	}
	if entry1.ctx.Err() == nil {
		t.Fatal("Expected entry1 context to be canceled after CloseAll")
	}
	if entry2.ctx.Err() == nil {
		t.Fatal("Expected entry2 context to be canceled after CloseAll")
	}
}

func TestClusterProvider_CloseAll_Idempotent(t *testing.T) {
	entry := newTestClusterEntry("https://example.com")

	provider := &ClusterProvider{
		entries: map[string]*clusterEntry{
			"ns/name": entry,
		},
	}

	provider.CloseAll()
	// Second call should not panic.
	provider.CloseAll()

	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache after CloseAll, got %d entries", len(provider.entries))
	}
}

func TestAnchorName_LengthValidation(t *testing.T) {
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
		t.Fatal("Expected error for anchor name exceeding max length, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Fatalf("Expected 'exceeds maximum length' error, got: %v", err)
	}
}

func TestClusterProvider_ConcurrentClose(t *testing.T) {
	provider := &ClusterProvider{
		entries: make(map[string]*clusterEntry),
	}
	entry := newTestClusterEntry("https://example.com")
	entry.kubeClient = fake.NewSimpleClientset()
	provider.entries["ns/name"] = entry

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			provider.Remove("ns/name")
		}()
	}
	wg.Wait()

	if len(provider.entries) != 0 {
		t.Errorf("expected empty cache after concurrent Remove, got %d entries", len(provider.entries))
	}
}

func TestEnsureAnchorConfigMap_NamespaceHasManagedByLabel(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test",
		},
	}

	ctx := context.Background()
	_, err := EnsureAnchorConfigMap(ctx, kubeClient, instance)
	if err != nil {
		t.Fatalf("EnsureAnchorConfigMap() error: %v", err)
	}

	ns, err := kubeClient.CoreV1().Namespaces().Get(ctx, "test-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected namespace test-ns to exist, got error: %v", err)
	}
	if ns.Labels["app.kubernetes.io/managed-by"] != "knative-operator" {
		t.Fatalf("Expected namespace to have managed-by label, got labels: %v", ns.Labels)
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
						CommonSpec: base.CommonSpec{ClusterProfileRef: ref},
					},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
				Spec: v1beta1.KnativeServingSpec{
					CommonSpec: base.CommonSpec{ClusterProfileRef: ref},
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
						CommonSpec: base.CommonSpec{ClusterProfileRef: &base.ClusterProfileReference{
							Namespace: "fleet", Name: "spoke2",
						}},
					},
				},
			},
			original: &v1beta1.KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ks"},
				Spec: v1beta1.KnativeServingSpec{
					CommonSpec: base.CommonSpec{ClusterProfileRef: ref},
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

func TestResolveTargetCluster_NilRef_MarkResolved(t *testing.T) {
	instance := &v1beta1.KnativeEventing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
	}
	instance.Status.InitializeConditions()

	manifest, _ := mf.ManifestFrom(mf.Slice{})
	var state ReconcileState
	stage := ResolveTargetCluster(nil, &state)
	if err := stage(context.Background(), &manifest, instance); err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	cond := instance.Status.GetCondition(base.TargetClusterResolved)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("Expected TargetClusterResolved to be True for KnativeEventing with nil ref, got: %v", cond)
	}
}
