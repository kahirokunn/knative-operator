/*
Copyright 2022 The Knative Authors

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/operator/pkg/apis/operator/base"
	duckv1 "knative.dev/pkg/apis/duck/v1"
)

var (
	_ base.KComponent     = (*KnativeServing)(nil)
	_ base.KComponentSpec = (*KnativeServingSpec)(nil)
)

// KnativeServing is the Schema for the knativeservings API
// +genclient
// +genreconciler:krshapedlogic=false
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target Cluster",type=string,JSONPath=`.spec.placement.clusterProfileRef.name`,priority=0
// +kubebuilder:printcolumn:name="Target Namespace",type=string,JSONPath=`.spec.placement.namespace`,priority=0
// +kubebuilder:printcolumn:name="Legacy Target Cluster",type=string,JSONPath=`.spec.clusterProfileRef.name`,priority=1
type KnativeServing struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnativeServingSpec   `json:"spec,omitempty"`
	Status KnativeServingStatus `json:"status,omitempty"`
}

// GetSpec implements KComponent
func (ks *KnativeServing) GetSpec() base.KComponentSpec {
	return &ks.Spec
}

// GetStatus implements KComponent
func (ks *KnativeServing) GetStatus() base.KComponentStatus {
	return &ks.Status
}

// KnativeServingSpec defines the desired state of KnativeServing
// Migration from clusterProfileRef to placement is intentionally staged: first add a matching
// placement while retaining clusterProfileRef, then remove clusterProfileRef in a later update.
// Single-step swaps are rejected; placement remains correctable while the legacy field exists
// and becomes immutable after it is removed. Neither remote field can otherwise be introduced
// after creation.
// +kubebuilder:validation:XValidation:rule="(has(self.clusterProfileRef) || has(self.placement)) == (has(oldSelf.clusterProfileRef) || has(oldSelf.placement))",message="remote placement cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="!has(self.clusterProfileRef) || (has(oldSelf.clusterProfileRef) && self.clusterProfileRef == oldSelf.clusterProfileRef)",message="spec.clusterProfileRef is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.placement) || (has(self.placement) && (self.placement == oldSelf.placement || has(self.clusterProfileRef)))",message="spec.placement is immutable once spec.clusterProfileRef is removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.clusterProfileRef) || has(self.clusterProfileRef) || (has(oldSelf.placement) && has(self.placement) && [self.placement.clusterProfileRef.name, self.placement.clusterProfileRef.namespace] == [oldSelf.clusterProfileRef.name, oldSelf.clusterProfileRef.namespace])",message="spec.clusterProfileRef cannot be removed until spec.placement targets the same ClusterProfile in a prior update"
type KnativeServingSpec struct {
	base.CommonSpec `json:",inline"`

	// Enables controller to trust registries with self-signed certificates
	ControllerCustomCerts base.CustomCerts `json:"controller-custom-certs,omitempty"`

	// Ingress allows configuration of different ingress adapters to be shipped.
	Ingress *IngressConfigs `json:"ingress,omitempty"`

	// Security allows configuration of different security adapters to be shipped.
	Security *SecurityConfigs `json:"security,omitempty"`
}

// KnativeServingStatus defines the observed state of KnativeServing
type KnativeServingStatus struct {
	duckv1.Status `json:",inline"`

	// The version of the installed release
	// +optional
	Version string `json:"version,omitempty"`

	// The url links of the manifests, separated by comma
	// +optional
	Manifests []string `json:"manifests,omitempty"`
}

// KnativeServingList contains a list of KnativeServing
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type KnativeServingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnativeServing `json:"items"`
}

// IngressConfigs specifies options for the ingresses.
type IngressConfigs struct {
	// +optional
	Istio base.IstioIngressConfiguration `json:"istio,omitempty"`
	// +optional
	Kourier base.KourierIngressConfiguration `json:"kourier,omitempty"`
	// +optional
	Contour base.ContourIngressConfiguration `json:"contour,omitempty"`
	// +optional
	GatewayAPI base.GatewayAPIIngressConfiguration `json:"gateway-api,omitempty"`
}

// SecurityConfigs specifies options for the security
type SecurityConfigs struct {
	SecurityGuard base.SecurityGuardConfiguration `json:"securityGuard"`
}
