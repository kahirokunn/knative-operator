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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlagsAcceptedBySharedmain guards against the early-flag.Parse()
// regression (see plans/fix-early-flag-parse-bug.md). flag.CommandLine
// registration order is a process-startup phenomenon invisible to normal
// unit tests, so we compile the binary and invoke it with each flag that
// sharedmain registers late via ParseAndGetRESTConfigOrDie. None of these
// flags may ever produce "flag provided but not defined".
func TestFlagsAcceptedBySharedmain(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "knative-operator")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	flags := []string{
		"--kubeconfig=/nonexistent",
		"--disable-ha",
		"--server=https://example.invalid",
		"--cluster=remote",
		"--kube-api-burst=200",
		"--kube-api-qps=100",
	}
	for _, fl := range flags {
		t.Run(strings.TrimPrefix(strings.SplitN(fl, "=", 2)[0], "--"), func(t *testing.T) {
			cmd := exec.Command(bin, fl)
			cmd.Env = append(cmd.Environ(), "KUBECONFIG=/dev/null")
			done := make(chan struct{})
			var out []byte
			var runErr error
			go func() {
				out, runErr = cmd.CombinedOutput()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = runErr
			if strings.Contains(string(out), "flag provided but not defined") {
				t.Fatalf("%s rejected as undefined — early flag.Parse() regression?\n%s", fl, out)
			}
		})
	}
}
