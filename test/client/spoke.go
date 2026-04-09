//go:build e2e
// +build e2e

/*
Copyright 2024 The Knative Authors

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

package client

import (
	"os"
	"testing"

	"knative.dev/operator/test"
)

// SetupSpoke creates a test.Clients pointing at the spoke cluster referenced
// by the SPOKE_KUBECONFIG environment variable. If the env var is not set,
// the test is skipped so the default single-cluster e2e path remains
// unaffected.
func SetupSpoke(t *testing.T) *test.Clients {
	t.Helper()
	path := os.Getenv("SPOKE_KUBECONFIG")
	if path == "" {
		t.Skip("SPOKE_KUBECONFIG not set; skipping multi-cluster e2e")
	}
	clients, err := test.NewClients(path, "")
	if err != nil {
		t.Fatalf("Couldn't initialize spoke clients: %v", err)
	}
	return clients
}
