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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"knative.dev/operator/pkg/apis/operator/base"
	operatorv1beta1 "knative.dev/operator/pkg/apis/operator/v1beta1"
	operatorclient "knative.dev/operator/pkg/client/clientset/versioned"
	"knative.dev/pkg/webhook"
	"knative.dev/pkg/webhook/resourcesemantics/validation"
)

func placementValidationCallbacks(client operatorclient.Interface) map[schema.GroupVersionKind]validation.Callback {
	return map[schema.GroupVersionKind]validation.Callback{
		operatorv1beta1.SchemeGroupVersion.WithKind("KnativeServing"): validation.NewCallback(
			validateUniqueKnativeServingPlacement(client), webhook.Create),
		operatorv1beta1.SchemeGroupVersion.WithKind("KnativeEventing"): validation.NewCallback(
			validateUniqueKnativeEventingPlacement(client), webhook.Create),
	}
}

func validateUniqueKnativeServingPlacement(client operatorclient.Interface) func(context.Context, *unstructured.Unstructured) error {
	return func(ctx context.Context, obj *unstructured.Unstructured) error {
		incoming := &operatorv1beta1.KnativeServing{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, incoming); err != nil {
			return fmt.Errorf("decode KnativeServing: %w", err)
		}
		list, err := client.OperatorV1beta1().KnativeServings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list KnativeServings: %w", err)
		}
		existing := make([]base.KComponent, len(list.Items))
		for i := range list.Items {
			existing[i] = &list.Items[i]
		}
		return validateUniqueRemotePlacement(incoming, existing, "KnativeServing")
	}
}

func validateUniqueKnativeEventingPlacement(client operatorclient.Interface) func(context.Context, *unstructured.Unstructured) error {
	return func(ctx context.Context, obj *unstructured.Unstructured) error {
		incoming := &operatorv1beta1.KnativeEventing{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, incoming); err != nil {
			return fmt.Errorf("decode KnativeEventing: %w", err)
		}
		list, err := client.OperatorV1beta1().KnativeEventings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list KnativeEventings: %w", err)
		}
		existing := make([]base.KComponent, len(list.Items))
		for i := range list.Items {
			existing[i] = &list.Items[i]
		}
		return validateUniqueRemotePlacement(incoming, existing, "KnativeEventing")
	}
}

func validateUniqueRemotePlacement(incoming base.KComponent, existing []base.KComponent, kind string) error {
	ref := effectiveClusterProfileRef(incoming.GetSpec())
	if ref == nil {
		return nil
	}
	for _, component := range existing {
		if component.GetNamespace() == incoming.GetNamespace() && component.GetName() == incoming.GetName() {
			continue
		}
		if sameClusterProfile(ref, effectiveClusterProfileRef(component.GetSpec())) {
			return fmt.Errorf(
				"%s %s/%s already targets ClusterProfile %s/%s; only one %s may target a remote cluster",
				kind, component.GetNamespace(), component.GetName(), ref.Namespace, ref.Name, kind)
		}
	}
	return nil
}

func effectiveClusterProfileRef(spec base.KComponentSpec) *base.ClusterProfileReference {
	if ref := spec.GetClusterProfileRef(); ref != nil {
		return ref
	}
	if placement := spec.GetPlacement(); placement != nil {
		ref := placement.ClusterProfileRef
		return &ref
	}
	return nil
}

func sameClusterProfile(a, b *base.ClusterProfileReference) bool {
	return a != nil && b != nil && a.Namespace == b.Namespace && a.Name == b.Name
}
