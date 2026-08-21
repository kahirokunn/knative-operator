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

package v1beta1

import (
	"context"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/pkg/apis"
)

// SetDefaults implements apis.Defaultable.
func (*KnativeServing) SetDefaults(context.Context) {}

// SetDefaults implements apis.Defaultable.
func (*KnativeEventing) SetDefaults(context.Context) {}

// SupportedVerbs limits placement admission validation to creates and updates.
func (*KnativeServing) SupportedVerbs() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}
}

// SupportedVerbs limits placement admission validation to creates and updates.
func (*KnativeEventing) SupportedVerbs() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
	}
}

// SupportedSubResources limits placement admission validation to spec updates.
func (*KnativeServing) SupportedSubResources() []string { return []string{""} }

// SupportedSubResources limits placement admission validation to spec updates.
func (*KnativeEventing) SupportedSubResources() []string { return []string{""} }

// Validate implements apis.Validatable.
func (ks *KnativeServing) Validate(ctx context.Context) *apis.FieldError {
	if apis.IsInDelete(ctx) {
		return nil
	}
	var oldLegacyRef *base.ClusterProfileReference
	var oldPlacement *base.ComponentPlacement
	isUpdate := false
	if old, ok := apis.GetBaseline(ctx).(*KnativeServing); ok {
		isUpdate = true
		oldLegacyRef = old.Spec.GetClusterProfileRef()
		oldPlacement = old.Spec.Placement
	}
	return validatePlacementMigration(
		ks.Namespace, ks.Spec.GetClusterProfileRef(), ks.Spec.Placement, isUpdate, oldLegacyRef, oldPlacement)
}

// Validate implements apis.Validatable.
func (ke *KnativeEventing) Validate(ctx context.Context) *apis.FieldError {
	if apis.IsInDelete(ctx) {
		return nil
	}
	var oldLegacyRef *base.ClusterProfileReference
	var oldPlacement *base.ComponentPlacement
	isUpdate := false
	if old, ok := apis.GetBaseline(ctx).(*KnativeEventing); ok {
		isUpdate = true
		oldLegacyRef = old.Spec.GetClusterProfileRef()
		oldPlacement = old.Spec.Placement
	}
	return validatePlacementMigration(
		ke.Namespace, ke.Spec.GetClusterProfileRef(), ke.Spec.Placement, isUpdate, oldLegacyRef, oldPlacement)
}

func validatePlacementMigration(
	managementNamespace string,
	legacyRef *base.ClusterProfileReference,
	placement *base.ComponentPlacement,
	isUpdate bool,
	oldLegacyRef *base.ClusterProfileReference,
	oldPlacement *base.ComponentPlacement,
) *apis.FieldError {
	if isUpdate {
		hasRemotePlacement := legacyRef != nil || placement != nil
		hadRemotePlacement := oldLegacyRef != nil || oldPlacement != nil
		if hasRemotePlacement != hadRemotePlacement {
			return apis.ErrGeneric(
				"remote placement cannot be added or removed after creation",
				"spec.clusterProfileRef", "spec.placement")
		}
		if legacyRef != nil {
			if oldLegacyRef == nil {
				return apis.ErrGeneric(
					"deprecated spec.clusterProfileRef cannot be added after creation",
					"spec.clusterProfileRef")
			}
			if *legacyRef != *oldLegacyRef {
				return apis.ErrInvalidValue(
					clusterProfileReferenceString(*legacyRef),
					"spec.clusterProfileRef",
					fmt.Sprintf("must remain %q", clusterProfileReferenceString(*oldLegacyRef)))
			}
		}
	}

	if oldPlacement != nil {
		if placement == nil {
			return apis.ErrMissingField("spec.placement")
		}
		if legacyRef == nil {
			var errs *apis.FieldError
			if placement.ClusterProfileRef != oldPlacement.ClusterProfileRef {
				errs = errs.Also(apis.ErrInvalidValue(
					clusterProfileReferenceString(placement.ClusterProfileRef),
					"spec.placement.clusterProfileRef",
					fmt.Sprintf("must remain %q after deprecated spec.clusterProfileRef is removed",
						clusterProfileReferenceString(oldPlacement.ClusterProfileRef))))
			}
			if placement.Namespace != oldPlacement.Namespace {
				errs = errs.Also(apis.ErrInvalidValue(
					placement.Namespace,
					"spec.placement.namespace",
					fmt.Sprintf("must remain %q after deprecated spec.clusterProfileRef is removed",
						oldPlacement.Namespace)))
			}
			if errs != nil {
				return errs
			}
		}
	}

	requiredLegacyRef := legacyRef
	removingLegacyRef := requiredLegacyRef == nil && oldLegacyRef != nil
	if removingLegacyRef {
		if oldPlacement == nil {
			return apis.ErrGeneric(
				"spec.placement must be added in a prior update before removing deprecated spec.clusterProfileRef",
				"spec.clusterProfileRef")
		}
		requiredLegacyRef = oldLegacyRef
	}
	if requiredLegacyRef == nil {
		return nil
	}
	if placement == nil {
		if removingLegacyRef {
			return apis.ErrMissingField("spec.placement")
		}
		return nil
	}

	var errs *apis.FieldError
	if placement.ClusterProfileRef != *requiredLegacyRef {
		errs = errs.Also(apis.ErrInvalidValue(
			clusterProfileReferenceString(placement.ClusterProfileRef),
			"spec.placement.clusterProfileRef",
			fmt.Sprintf("must match deprecated spec.clusterProfileRef %q",
				clusterProfileReferenceString(*requiredLegacyRef))))
	}
	if placement.Namespace != managementNamespace {
		errs = errs.Also(apis.ErrInvalidValue(
			placement.Namespace,
			"spec.placement.namespace",
			fmt.Sprintf("must match metadata.namespace %q while deprecated spec.clusterProfileRef is present or being removed",
				managementNamespace)))
	}
	return errs
}

func clusterProfileReferenceString(ref base.ClusterProfileReference) string {
	return ref.Namespace + "/" + ref.Name
}
