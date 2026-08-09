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
	"encoding/json"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch/v5"
	mf "github.com/manifestival/manifestival"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	"knative.dev/operator/pkg/apis/operator/base"
)

const strategicMergeDeleteDirective = "delete"

type resourceIdentity struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
}

// ApplyResourcePatches applies user-provided patches in declaration order. A
// strategic merge patch with a root-level "$patch: delete" directive removes
// the matching resource from the manifest.
func ApplyResourcePatches(manifest *mf.Manifest, patches []base.ResourcePatch) error {
	for i, resourcePatch := range patches {
		matches := matchingResources(*manifest, resourcePatch.Target)
		if len(matches) == 0 {
			return fmt.Errorf("patch %d target %s did not match any generated resource", i, describeTarget(resourcePatch.Target))
		}
		if len(matches) > 1 {
			return fmt.Errorf("patch %d target %s matched %d generated resources; specify target.namespace", i, describeTarget(resourcePatch.Target), len(matches))
		}

		patchJSON, err := yaml.YAMLToJSON([]byte(resourcePatch.Patch.Content))
		if err != nil {
			return fmt.Errorf("patch %d target %s: convert patch content to JSON: %w", i, describeTarget(resourcePatch.Target), err)
		}

		if isDelete, err := isStrategicMergeDelete(resourcePatch.Patch.Type, patchJSON); err != nil {
			return fmt.Errorf("patch %d target %s: %w", i, describeTarget(resourcePatch.Target), err)
		} else if isDelete {
			if matches[0].GetKind() == "CustomResourceDefinition" {
				return fmt.Errorf("patch %d target %s: deleting CustomResourceDefinitions is not supported", i, describeTarget(resourcePatch.Target))
			}
			resourceManifest, err := mf.ManifestFrom(mf.Slice(matches))
			if err != nil {
				return fmt.Errorf("patch %d target %s: build resource removal filter: %w", i, describeTarget(resourcePatch.Target), err)
			}
			*manifest = manifest.Filter(mf.Not(mf.In(resourceManifest)))
			continue
		}

		identity := identityOf(&matches[0])
		patched, err := manifest.Transform(func(resource *unstructured.Unstructured) error {
			if identityOf(resource) != identity {
				return nil
			}
			return applyResourcePatch(resource, resourcePatch.Patch.Type, patchJSON)
		})
		if err != nil {
			return fmt.Errorf("patch %d target %s: %w", i, describeTarget(resourcePatch.Target), err)
		}
		*manifest = patched
	}
	return nil
}

func matchingResources(manifest mf.Manifest, target base.PatchTarget) []unstructured.Unstructured {
	matches := make([]unstructured.Unstructured, 0, 1)
	for _, resource := range manifest.Resources() {
		if resource.GetAPIVersion() != target.APIVersion || resource.GetKind() != target.Kind || resource.GetName() != target.Name {
			continue
		}
		if target.Namespace != "" && resource.GetNamespace() != target.Namespace {
			continue
		}
		matches = append(matches, resource)
	}
	return matches
}

func applyResourcePatch(resource *unstructured.Unstructured, patchType base.PatchType, patchJSON []byte) error {
	originalIdentity := identityOf(resource)
	originalJSON, err := resource.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal generated resource: %w", err)
	}

	var patchedJSON []byte
	switch patchType {
	case base.JSONPatchType:
		decoded, err := jsonpatch.DecodePatch(patchJSON)
		if err != nil {
			return fmt.Errorf("decode JSON patch: %w", err)
		}
		patchedJSON, err = decoded.Apply(originalJSON)
		if err != nil {
			return fmt.Errorf("apply JSON patch: %w", err)
		}
	case base.MergePatchType:
		patchedJSON, err = jsonpatch.MergePatch(originalJSON, patchJSON)
		if err != nil {
			return fmt.Errorf("apply merge patch: %w", err)
		}
	case base.StrategicMergePatchType:
		dataStruct, err := scheme.Scheme.New(resource.GroupVersionKind())
		if runtime.IsNotRegisteredError(err) {
			return fmt.Errorf("strategic merge patch is not supported for %s; use a json or merge patch", resource.GroupVersionKind())
		}
		if err != nil {
			return fmt.Errorf("resolve strategic merge patch schema for %s: %w", resource.GroupVersionKind(), err)
		}
		patchedJSON, err = strategicpatch.StrategicMergePatch(originalJSON, patchJSON, dataStruct)
		if err != nil {
			return fmt.Errorf("apply strategic merge patch: %w", err)
		}
	default:
		return fmt.Errorf("unsupported patch type %q", patchType)
	}

	patched := &unstructured.Unstructured{}
	if err := patched.UnmarshalJSON(patchedJSON); err != nil {
		return fmt.Errorf("decode patched resource: %w", err)
	}
	if patchedIdentity := identityOf(patched); patchedIdentity != originalIdentity {
		return fmt.Errorf("patch must not change apiVersion, kind, metadata.name, or metadata.namespace")
	}
	*resource = *patched
	return nil
}

func isStrategicMergeDelete(patchType base.PatchType, patchJSON []byte) (bool, error) {
	if patchType != base.StrategicMergePatchType {
		return false, nil
	}
	content := map[string]interface{}{}
	if err := json.Unmarshal(patchJSON, &content); err != nil {
		return false, fmt.Errorf("decode strategic merge patch: %w", err)
	}
	directive, found := content["$patch"]
	if !found {
		return false, nil
	}
	directiveString, ok := directive.(string)
	if !ok {
		return false, fmt.Errorf("root $patch directive must be a string")
	}
	return directiveString == strategicMergeDeleteDirective, nil
}

func identityOf(resource *unstructured.Unstructured) resourceIdentity {
	return resourceIdentity{
		apiVersion: resource.GetAPIVersion(),
		kind:       resource.GetKind(),
		namespace:  resource.GetNamespace(),
		name:       resource.GetName(),
	}
}

func describeTarget(target base.PatchTarget) string {
	namespace := target.Namespace
	if namespace == "" {
		namespace = "*"
	}
	return fmt.Sprintf("%s, Kind=%s, %s/%s", target.APIVersion, target.Kind, namespace, target.Name)
}
