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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/pkg/apis"
)

func TestValidatePlacementMigration(t *testing.T) {
	legacyRef := &base.ClusterProfileReference{Namespace: "fleet-system", Name: "spoke-tokyo"}
	otherRef := base.ClusterProfileReference{Namespace: "fleet-system", Name: "spoke-osaka"}

	factories := []struct {
		name      string
		namespace string
		new       func(*base.ClusterProfileReference, *base.ComponentPlacement) apis.Validatable
	}{
		{name: "Serving", namespace: "knative-serving", new: func(ref *base.ClusterProfileReference, placement *base.ComponentPlacement) apis.Validatable {
			return &KnativeServing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "knative-serving"},
				Spec: KnativeServingSpec{CommonSpec: base.CommonSpec{
					ClusterProfileRef: ref,
					Placement:         placement,
				}},
			}
		}},
		{name: "Eventing", namespace: "knative-eventing", new: func(ref *base.ClusterProfileReference, placement *base.ComponentPlacement) apis.Validatable {
			return &KnativeEventing{
				ObjectMeta: metav1.ObjectMeta{Namespace: "knative-eventing"},
				Spec: KnativeEventingSpec{CommonSpec: base.CommonSpec{
					ClusterProfileRef: ref,
					Placement:         placement,
				}},
			}
		}},
	}

	for _, factory := range factories {
		matchingPlacement := placementForValidation(factory.namespace, *legacyRef)
		otherLegacyRef := &base.ClusterProfileReference{Namespace: otherRef.Namespace, Name: otherRef.Name}
		tests := []struct {
			name         string
			isUpdate     bool
			oldLegacyRef *base.ClusterProfileReference
			oldPlacement *base.ComponentPlacement
			legacyRef    *base.ClusterProfileReference
			placement    *base.ComponentPlacement
			deleting     bool
			wantErrPart  string
		}{
			{name: "legacy only", legacyRef: legacyRef},
			{name: "placement only", placement: placementForValidation("custom-installation", *legacyRef)},
			{name: "local update remains local", isUpdate: true},
			{
				name:        "local update cannot add legacy reference",
				isUpdate:    true,
				legacyRef:   legacyRef,
				wantErrPart: "remote placement cannot be added",
			},
			{
				name:        "local update cannot add placement",
				isUpdate:    true,
				placement:   matchingPlacement,
				wantErrPart: "remote placement cannot be added",
			},
			{name: "placement unchanged after migration", oldPlacement: matchingPlacement, placement: matchingPlacement},
			{
				name:         "legacy reference cannot be added after migration",
				oldPlacement: matchingPlacement,
				legacyRef:    legacyRef,
				placement:    matchingPlacement,
				wantErrPart:  "spec.clusterProfileRef",
			},
			{
				name:         "placement cluster is immutable after migration",
				oldPlacement: matchingPlacement,
				placement:    placementForValidation(factory.namespace, otherRef),
				wantErrPart:  "spec.placement.clusterProfileRef",
			},
			{
				name:         "placement namespace is immutable after migration",
				oldPlacement: matchingPlacement,
				placement:    placementForValidation("custom-installation", *legacyRef),
				wantErrPart:  "spec.placement.namespace",
			},
			{
				name:         "placement cannot be removed after migration",
				oldPlacement: matchingPlacement,
				wantErrPart:  "spec.placement",
			},
			{name: "matching migration", legacyRef: legacyRef, placement: matchingPlacement},
			{
				name:         "matching placement can be added during migration",
				oldLegacyRef: legacyRef,
				legacyRef:    legacyRef,
				placement:    matchingPlacement,
			},
			{
				name:         "placement cannot be removed during migration",
				oldLegacyRef: legacyRef,
				oldPlacement: matchingPlacement,
				legacyRef:    legacyRef,
				wantErrPart:  "spec.placement",
			},
			{
				name:         "legacy reference is immutable",
				oldLegacyRef: legacyRef,
				legacyRef:    otherLegacyRef,
				placement:    placementForValidation(factory.namespace, otherRef),
				wantErrPart:  "spec.clusterProfileRef",
			},
			{
				name:        "migration uses another cluster",
				legacyRef:   legacyRef,
				placement:   placementForValidation(factory.namespace, otherRef),
				wantErrPart: "spec.placement.clusterProfileRef",
			},
			{
				name:        "migration uses another namespace",
				legacyRef:   legacyRef,
				placement:   placementForValidation("custom-installation", *legacyRef),
				wantErrPart: "spec.placement.namespace",
			},
			{
				name:         "matching legacy removal",
				oldLegacyRef: legacyRef,
				oldPlacement: matchingPlacement,
				placement:    matchingPlacement,
			},
			{
				name:         "direct migration swap",
				oldLegacyRef: legacyRef,
				placement:    matchingPlacement,
				wantErrPart:  "prior update",
			},
			{
				name:         "legacy removal without placement",
				oldLegacyRef: legacyRef,
				oldPlacement: matchingPlacement,
				wantErrPart:  "spec.placement",
			},
			{
				name:         "legacy removal activates another cluster",
				oldLegacyRef: legacyRef,
				oldPlacement: matchingPlacement,
				placement:    placementForValidation(factory.namespace, otherRef),
				wantErrPart:  "spec.placement.clusterProfileRef",
			},
			{
				name:         "legacy removal activates another namespace",
				oldLegacyRef: legacyRef,
				oldPlacement: matchingPlacement,
				placement:    placementForValidation("custom-installation", *legacyRef),
				wantErrPart:  "spec.placement.namespace",
			},
			{
				name:      "delete bypasses migration validation",
				legacyRef: legacyRef,
				placement: placementForValidation("custom-installation", otherRef),
				deleting:  true,
			},
		}

		for _, tt := range tests {
			t.Run(factory.name+"/"+tt.name, func(t *testing.T) {
				ctx := context.Background()
				if tt.isUpdate || tt.oldLegacyRef != nil || tt.oldPlacement != nil {
					ctx = apis.WithinUpdate(ctx, factory.new(tt.oldLegacyRef, tt.oldPlacement))
				}
				if tt.deleting {
					ctx = apis.WithinDelete(ctx)
				}
				err := factory.new(tt.legacyRef, tt.placement).Validate(ctx)
				if tt.wantErrPart == "" && err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				if tt.wantErrPart != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrPart)) {
					t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErrPart)
				}
			})
		}
	}
}

func placementForValidation(namespace string, ref base.ClusterProfileReference) *base.ComponentPlacement {
	return &base.ComponentPlacement{ClusterProfileRef: ref, Namespace: namespace}
}
