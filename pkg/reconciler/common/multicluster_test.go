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
	"strings"
	"testing"

	mf "github.com/manifestival/manifestival"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/operator/pkg/apis/operator/v1beta1"
)

func TestResolveTargetCluster_NilRef(t *testing.T) {
	// ClusterProfileRef is nil => the stage should be a no-op.
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
	}

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	origClient := manifest.Client

	stage := ResolveTargetCluster(nil) // localConfig is irrelevant for nil ref
	if err := stage(context.Background(), &manifest, instance); err != nil {
		t.Fatalf("Expected no error for nil ClusterProfileRef, got: %v", err)
	}

	if manifest.Client != origClient {
		t.Fatal("Expected manifest.Client to remain unchanged when ClusterProfileRef is nil")
	}
}

func TestResolveTargetCluster_MissingCredentialConfig(t *testing.T) {
	// Ensure the package variable is empty for this test.
	old := CredentialProvidersConfigPath
	CredentialProvidersConfigPath = ""
	defer func() { CredentialProvidersConfigPath = old }()

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

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	stage := ResolveTargetCluster(nil)
	err = stage(context.Background(), &manifest, instance)
	if err == nil {
		t.Fatal("Expected error when CredentialProvidersConfigPath is empty, got nil")
	}
	if !strings.Contains(err.Error(), "credential-providers-config") {
		t.Fatalf("Expected error message to contain 'credential-providers-config', got: %v", err)
	}
}

func TestResolveTargetClusterForManifest_NilRef(t *testing.T) {
	// ResolveTargetClusterForManifest with nil ClusterProfileRef => no-op.
	instance := &v1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test",
		},
	}

	manifest, err := mf.ManifestFrom(mf.Slice{})
	if err != nil {
		t.Fatalf("Failed to create manifest: %v", err)
	}

	origClient := manifest.Client

	if err := ResolveTargetClusterForManifest(context.Background(), nil, &manifest, instance); err != nil {
		t.Fatalf("Expected no error for nil ClusterProfileRef, got: %v", err)
	}

	if manifest.Client != origClient {
		t.Fatal("Expected manifest.Client to remain unchanged when ClusterProfileRef is nil")
	}
}
