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
	"fmt"

	mfc "github.com/manifestival/client-go-client"
	mf "github.com/manifestival/manifestival"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"knative.dev/operator/pkg/apis/operator/base"
	"knative.dev/pkg/logging"

	clusterinventoryclient "sigs.k8s.io/cluster-inventory-api/client/clientset/versioned"
	"sigs.k8s.io/cluster-inventory-api/pkg/credentials"
)

// CredentialProvidersConfigPath is the path to the credential providers
// config file for the Cluster Inventory API. Set via the
// --credential-providers-config flag in cmd/operator/main.go.
var CredentialProvidersConfigPath string

// ResolveTargetCluster returns a Stage that, when a ClusterProfileRef is
// set on the instance, swaps the manifest's Client to point at the remote
// cluster. If no ClusterProfileRef is set, it is a no-op.
//
// localConfig is the rest.Config for the local (management) cluster,
// used to fetch the ClusterProfile resource.
func ResolveTargetCluster(localConfig *rest.Config) Stage {
	return func(ctx context.Context, manifest *mf.Manifest, instance base.KComponent) error {
		cpRef := instance.GetSpec().GetClusterProfileRef()
		if cpRef == nil {
			return nil
		}

		logger := logging.FromContext(ctx)
		logger.Infof("Resolving target cluster from ClusterProfile %s/%s",
			cpRef.Namespace, cpRef.Name)

		if CredentialProvidersConfigPath == "" {
			return fmt.Errorf(
				"spec.clusterProfileRef is set but --credential-providers-config flag is not configured; " +
					"start the operator with --credential-providers-config=/path/to/config")
		}

		// 1. Fetch the ClusterProfile from the local (hub) cluster.
		ciClient, err := clusterinventoryclient.NewForConfig(localConfig)
		if err != nil {
			return fmt.Errorf("failed to create cluster-inventory client: %w", err)
		}
		cp, err := ciClient.ApisV1alpha1().ClusterProfiles(cpRef.Namespace).Get(
			ctx, cpRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get ClusterProfile %s/%s: %w",
				cpRef.Namespace, cpRef.Name, err)
		}

		// 2. Build a rest.Config for the remote (spoke) cluster.
		credProvider, err := credentials.NewFromFile(CredentialProvidersConfigPath)
		if err != nil {
			return fmt.Errorf("failed to load credential providers config: %w", err)
		}
		remoteConfig, err := credProvider.BuildConfigFromCP(cp)
		if err != nil {
			return fmt.Errorf("failed to build config from ClusterProfile %s/%s: %w",
				cpRef.Namespace, cpRef.Name, err)
		}

		// 3. Swap the manifest client to the remote cluster.
		remoteClient, err := mfc.NewClient(remoteConfig)
		if err != nil {
			return fmt.Errorf("failed to create remote manifestival client for ClusterProfile %s/%s: %w",
				cpRef.Namespace, cpRef.Name, err)
		}
		manifest.Client = remoteClient

		logger.Infof("Manifest client redirected to remote cluster via ClusterProfile %s/%s",
			cpRef.Namespace, cpRef.Name)
		return nil
	}
}

// ResolveTargetClusterForManifest is like ResolveTargetCluster but
// operates on an already-constructed manifest, for use in FinalizeKind
// where the stage pipeline is not available.
func ResolveTargetClusterForManifest(
	ctx context.Context,
	localConfig *rest.Config,
	manifest *mf.Manifest,
	instance base.KComponent,
) error {
	return ResolveTargetCluster(localConfig)(ctx, manifest, instance)
}
