//go:build e2e && multicluster
// +build e2e,multicluster

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

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"knative.dev/operator/test"
	"knative.dev/operator/test/client"
)

func TestMulticlusterPlacementAdmissionTransitions(t *testing.T) {
	hub := client.Setup(t)
	assertPlacementValidationWebhookConfigured(t, hub)

	tests := []struct {
		name      string
		kind      string
		namespace string
		resource  string
	}{
		{name: "Serving", kind: "KnativeServing", namespace: test.ServingOperatorNamespace, resource: "knativeservings"},
		{name: "Eventing", kind: "KnativeEventing", namespace: test.EventingOperatorNamespace, resource: "knativeeventings"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			resources := hub.Dynamic.Resource(schema.GroupVersionResource{
				Group: "operator.knative.dev", Version: "v1beta1", Resource: tt.resource,
			}).Namespace(tt.namespace)
			name := fmt.Sprintf("placement-admission-%d", time.Now().UnixNano())
			defer cleanupPlacementAdmissionResource(t, resources, name)

			legacyRef := map[string]any{"name": "missing-spoke", "namespace": "default"}
			obj := placementAdmissionResource(tt.kind, tt.namespace, name, map[string]any{
				"clusterProfileRef": legacyRef,
			})
			if _, err := resources.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create legacy %s: %v", tt.kind, err)
			}

			assertIncompletePlacementRejected(t, resources, tt.kind, tt.namespace)

			placement := map[string]any{
				"clusterProfileRef": legacyRef,
				"namespace":         tt.namespace,
			}
			_, err := updatePlacementAdmissionResource(ctx, resources, name, true, func(obj *unstructured.Unstructured) error {
				if err := unstructured.SetNestedMap(obj.Object, placement, "spec", "placement"); err != nil {
					return err
				}
				unstructured.RemoveNestedField(obj.Object, "spec", "clusterProfileRef")
				return nil
			})
			requireAdmissionRejection(t, err,
				"clusterProfileRef cannot be removed until spec.placement targets the same ClusterProfile in a prior update")

			_, err = updatePlacementAdmissionResource(ctx, resources, name, true, func(obj *unstructured.Unstructured) error {
				return unstructured.SetNestedField(obj.Object, "different-spoke", "spec", "clusterProfileRef", "name")
			})
			requireAdmissionRejection(t, err, "spec.clusterProfileRef is immutable")

			if _, err := updatePlacementAdmissionResource(ctx, resources, name, false, func(obj *unstructured.Unstructured) error {
				return unstructured.SetNestedMap(obj.Object, placement, "spec", "placement")
			}); err != nil {
				t.Fatalf("Add matching placement: %v", err)
			}
			if _, err := updatePlacementAdmissionResource(ctx, resources, name, false, func(obj *unstructured.Unstructured) error {
				unstructured.RemoveNestedField(obj.Object, "spec", "clusterProfileRef")
				return nil
			}); err != nil {
				t.Fatalf("Remove deprecated clusterProfileRef after staging placement: %v", err)
			}

			_, err = updatePlacementAdmissionResource(ctx, resources, name, true, func(obj *unstructured.Unstructured) error {
				return unstructured.SetNestedField(obj.Object, "different-namespace", "spec", "placement", "namespace")
			})
			requireAdmissionRejection(t, err, "spec.placement is immutable")

			_, err = updatePlacementAdmissionResource(ctx, resources, name, true, func(obj *unstructured.Unstructured) error {
				unstructured.RemoveNestedField(obj.Object, "spec", "placement")
				return nil
			})
			requireAdmissionRejection(t, err, "remote placement cannot be added or removed after creation")

			duplicateName := name + "-duplicate"
			duplicate := placementAdmissionResource(tt.kind, tt.namespace, duplicateName, map[string]any{
				"placement": placement,
			})
			_, err = resources.Create(ctx, duplicate, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			requireDuplicatePlacementRejection(t, err, tt.kind, "default/missing-spoke")
		})
	}
}

func assertPlacementValidationWebhookConfigured(t *testing.T, clients *test.Clients) {
	t.Helper()
	var lastProblem string
	err := wait.PollUntilContextTimeout(t.Context(), 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			configuration, err := clients.KubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
				ctx, "validation.webhook.operator.knative.dev", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				lastProblem = err.Error()
				return false, nil
			}
			if err != nil {
				return false, err
			}
			for _, wh := range configuration.Webhooks {
				if wh.Name != "validation.webhook.operator.knative.dev" {
					continue
				}
				if wh.ClientConfig.Service == nil || wh.ClientConfig.Service.Path == nil ||
					*wh.ClientConfig.Service.Path != "/resource-validation" {
					lastProblem = fmt.Sprintf("service path = %v, want /resource-validation", wh.ClientConfig.Service)
					return false, nil
				}
				if len(wh.ClientConfig.CABundle) == 0 {
					lastProblem = "CA bundle is empty"
					return false, nil
				}
				if !placementRuleConfigured(wh.Rules) {
					lastProblem = fmt.Sprintf("rules do not cover Serving and Eventing CREATE/UPDATE: %#v", wh.Rules)
					return false, nil
				}
				return true, nil
			}
			lastProblem = "webhook entry not found"
			return false, nil
		})
	if err != nil {
		t.Fatalf("Placement validation webhook did not become ready: %v (last state: %s)", err, lastProblem)
	}
}

func placementRuleConfigured(rules []admissionregistrationv1.RuleWithOperations) bool {
	servingCovered := false
	eventingCovered := false
	for _, rule := range rules {
		if !containsAdmissionOperation(rule.Operations, admissionregistrationv1.Create) ||
			!containsAdmissionOperation(rule.Operations, admissionregistrationv1.Update) ||
			!containsString(rule.Rule.APIGroups, "operator.knative.dev") ||
			!containsString(rule.Rule.APIVersions, "v1beta1") {
			continue
		}
		servingCovered = servingCovered || containsString(rule.Rule.Resources, "knativeservings")
		eventingCovered = eventingCovered || containsString(rule.Rule.Resources, "knativeeventings")
	}
	return servingCovered && eventingCovered
}

func containsAdmissionOperation(values []admissionregistrationv1.OperationType, want admissionregistrationv1.OperationType) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertIncompletePlacementRejected(
	t *testing.T,
	resources dynamic.ResourceInterface,
	kind, namespace string,
) {
	t.Helper()
	name := fmt.Sprintf("placement-incomplete-%d", time.Now().UnixNano())
	obj := placementAdmissionResource(kind, namespace, name, map[string]any{
		"placement": map[string]any{
			"clusterProfileRef": map[string]any{"name": "schema-test", "namespace": "default"},
		},
	})
	_, err := resources.Create(t.Context(), obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	requireAdmissionRejection(t, err, "spec.placement.namespace")
}

func placementAdmissionResource(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.knative.dev/v1beta1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}}
}

func updatePlacementAdmissionResource(
	ctx context.Context,
	resources dynamic.ResourceInterface,
	name string,
	dryRun bool,
	mutate func(*unstructured.Unstructured) error,
) (*unstructured.Unstructured, error) {
	var updated *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := resources.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := mutate(current); err != nil {
			return err
		}
		opts := metav1.UpdateOptions{}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		updated, err = resources.Update(ctx, current, opts)
		return err
	})
	return updated, err
}

func requireAdmissionRejection(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("request succeeded, want admission rejection containing %q", message)
	}
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), message) {
		t.Fatalf("request error = %v, want Invalid containing %q", err, message)
	}
}

func requireDuplicatePlacementRejection(t *testing.T, err error, kind, clusterProfile string) {
	t.Helper()
	if err == nil {
		t.Fatal("duplicate placement create succeeded, want admission rejection")
	}
	if !apierrors.IsBadRequest(err) ||
		!strings.Contains(err.Error(), kind) ||
		!strings.Contains(err.Error(), "already targets ClusterProfile "+clusterProfile) {
		t.Fatalf("duplicate placement error = %v, want BadRequest identifying %s and %s", err, kind, clusterProfile)
	}
}

func cleanupPlacementAdmissionResource(t *testing.T, resources dynamic.ResourceInterface, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		obj, err := resources.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if len(obj.GetFinalizers()) > 0 {
			obj.SetFinalizers(nil)
			if _, err := resources.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
				if apierrors.IsConflict(err) {
					return false, nil
				}
				return false, err
			}
		}
		if err := resources.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		t.Errorf("Clean up admission test resource %q: %v", name, err)
	}
}
