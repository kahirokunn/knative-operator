/*
Copyright 2020 The Knative Authors

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
	// Blank import for its package init() side effects:
	//   - access_provider_flag.go registers --clusterprofile-provider-file on
	//     flag.CommandLine before sharedmain calls flag.Parse() in
	//     ParseAndGetRESTConfigOrDie.
	//   - images.go registers the caching/v1alpha1 types with the client-go
	//     scheme used by image cache handling.
	// Removing this import will silently break both.
	_ "knative.dev/operator/pkg/reconciler/common"
	"knative.dev/operator/pkg/reconciler/knativeeventing"
	"knative.dev/operator/pkg/reconciler/knativeserving"
	kubefilteredfactory "knative.dev/pkg/client/injection/kube/informers/factory/filtered"
	"knative.dev/pkg/injection/sharedmain"
	"knative.dev/pkg/signals"
)

func main() {
	// NOTE: Do NOT call flag.Parse() here. sharedmain.MainWithContext owns
	// flag parsing via ParseAndGetRESTConfigOrDie, and registers flags like
	// --kubeconfig, --disable-ha, --server, --cluster, --kube-api-burst,
	// --kube-api-qps, and klog flags just before it parses. Parsing early
	// would reject those flags as "provided but not defined".
	ctx := signals.NewContext()
	ctx = kubefilteredfactory.WithSelectors(ctx,
		knativeserving.Selector,
		knativeeventing.Selector,
	)
	sharedmain.MainWithContext(ctx, "knative-operator",
		knativeserving.NewController,
		knativeeventing.NewController,
	)
}
