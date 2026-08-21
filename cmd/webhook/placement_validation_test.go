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

package main

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"knative.dev/operator/pkg/apis/operator/base"
	operatorv1beta1 "knative.dev/operator/pkg/apis/operator/v1beta1"
	operatorclient "knative.dev/operator/pkg/client/clientset/versioned"
	fakeoperatorclient "knative.dev/operator/pkg/client/clientset/versioned/fake"
)

func TestUniqueRemotePlacementValidation(t *testing.T) {
	ref := base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}
	otherRef := base.ClusterProfileReference{Namespace: "fleet", Name: "other"}
	deleting := metav1.Now()

	tests := []struct {
		name      string
		existing  []runtime.Object
		incoming  runtime.Object
		validator func(operatorclient.Interface) func(context.Context, *unstructured.Unstructured) error
		wantError string
	}{
		{
			name:      "Serving rejects the same placement",
			existing:  []runtime.Object{servingForPlacement("operators-a", "first", &ref, false)},
			incoming:  servingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeServingPlacement,
			wantError: "KnativeServing operators-a/first already targets ClusterProfile fleet/spoke",
		},
		{
			name: "Serving rejects a terminating owner until deletion completes",
			existing: []runtime.Object{func() *operatorv1beta1.KnativeServing {
				ks := servingForPlacement("operators-a", "first", &ref, false)
				ks.DeletionTimestamp = &deleting
				return ks
			}()},
			incoming:  servingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeServingPlacement,
			wantError: "already targets ClusterProfile fleet/spoke",
		},
		{
			name:      "Serving recognizes the deprecated reference",
			existing:  []runtime.Object{servingForPlacement("operators-a", "first", &ref, true)},
			incoming:  servingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeServingPlacement,
			wantError: "already targets ClusterProfile fleet/spoke",
		},
		{
			name:      "Serving allows another remote cluster",
			existing:  []runtime.Object{servingForPlacement("operators-a", "first", &otherRef, false)},
			incoming:  servingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeServingPlacement,
		},
		{
			name:      "Serving allows local placement",
			existing:  []runtime.Object{servingForPlacement("operators-a", "first", &ref, false)},
			incoming:  servingForPlacement("operators-b", "second", nil, false),
			validator: validateUniqueKnativeServingPlacement,
		},
		{
			name:      "Serving and Eventing may share a remote cluster",
			existing:  []runtime.Object{eventingForPlacement("operators-a", "eventing", &ref, false)},
			incoming:  servingForPlacement("operators-b", "serving", &ref, false),
			validator: validateUniqueKnativeServingPlacement,
		},
		{
			name:      "Eventing rejects the same placement",
			existing:  []runtime.Object{eventingForPlacement("operators-a", "first", &ref, false)},
			incoming:  eventingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeEventingPlacement,
			wantError: "KnativeEventing operators-a/first already targets ClusterProfile fleet/spoke",
		},
		{
			name:      "Eventing allows another remote cluster",
			existing:  []runtime.Object{eventingForPlacement("operators-a", "first", &otherRef, false)},
			incoming:  eventingForPlacement("operators-b", "second", &ref, false),
			validator: validateUniqueKnativeEventingPlacement,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakeoperatorclient.NewSimpleClientset(tt.existing...)
			err := tt.validator(client)(t.Context(), toUnstructured(t, tt.incoming))
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("validation error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("validation error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestUniqueRemotePlacementValidationFailsClosed(t *testing.T) {
	client := fakeoperatorclient.NewSimpleClientset()
	client.PrependReactor("list", "knativeservings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("injected")
	})
	ref := base.ClusterProfileReference{Namespace: "fleet", Name: "spoke"}
	err := validateUniqueKnativeServingPlacement(client)(
		t.Context(), toUnstructured(t, servingForPlacement("operators", "serving", &ref, false)))
	if err == nil || !strings.Contains(err.Error(), "list KnativeServings") {
		t.Fatalf("validation error = %v, want list failure", err)
	}
}

func servingForPlacement(
	namespace, name string,
	ref *base.ClusterProfileReference,
	legacy bool,
) *operatorv1beta1.KnativeServing {
	return &operatorv1beta1.KnativeServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: operatorv1beta1.KnativeServingSpec{
			CommonSpec: commonSpecForPlacement(namespace, ref, legacy),
		},
	}
}

func eventingForPlacement(
	namespace, name string,
	ref *base.ClusterProfileReference,
	legacy bool,
) *operatorv1beta1.KnativeEventing {
	return &operatorv1beta1.KnativeEventing{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: operatorv1beta1.KnativeEventingSpec{
			CommonSpec: commonSpecForPlacement(namespace, ref, legacy),
		},
	}
}

func commonSpecForPlacement(
	namespace string,
	ref *base.ClusterProfileReference,
	legacy bool,
) base.CommonSpec {
	if ref == nil {
		return base.CommonSpec{}
	}
	if legacy {
		copy := *ref
		return base.CommonSpec{ClusterProfileRef: &copy}
	}
	return base.CommonSpec{Placement: &base.ComponentPlacement{
		ClusterProfileRef: *ref,
		Namespace:         namespace,
	}}
}

func toUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("ToUnstructured(): %v", err)
	}
	return &unstructured.Unstructured{Object: content}
}
