#!/usr/bin/env bash

# Copyright 2019 The Knative Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This script runs the end-to-end tests against Knative Serving
# Operator built from source.  It is started by prow for each PR. For
# convenience, it can also be executed manually.

# If you already have a Knative cluster setup and kubectl pointing
# to it, call this script with the --run-tests arguments and it will use
# the cluster and run the tests.

# Calling this script without arguments will create a new cluster in
# project $PROJECT_ID, start knative in it, run the tests and delete the
# cluster.

source $(dirname $0)/e2e-common.sh

# Multi-cluster e2e requires /bin/sh and /bin/cat in the operator container so
# the access provider exec plugin can run. The default ko base image
# (distroless/static) lacks both, so we override before initialize triggers
# `ko apply`.
if [[ "${TEST_MULTICLUSTER_E2E:-}" == "1" ]]; then
  export KO_DEFAULTBASEIMAGE="${KO_DEFAULTBASEIMAGE:-cgr.dev/chainguard/busybox:latest}"
fi

function knative_setup() {
  create_namespace
  install_operator
}

# Skip installing istio as an add-on
initialize $@

if [[ "${TEST_MULTICLUSTER_E2E:-}" == "1" ]]; then
  # Register cleanup BEFORE running setup so a partial failure (e.g. CRD
  # install fails after `kind create cluster` succeeds) still tears the
  # spoke cluster down. dump_spoke_state runs first so the artifact is
  # written before the cluster is gone.
  add_trap "dump_spoke_state; delete_spoke_cluster" EXIT
  setup_multicluster_e2e || fail_test "failed to set up spoke cluster"
  export SPOKE_KUBECONFIG SPOKE_HOST_KUBECONFIG
fi

# If we got this far, the operator installed Knative Serving
header "Running tests for Knative Operator"
failed=0

# Run the integration tests
go_test_e2e -timeout=20m ./test/e2e || failed=1

# Require that tests succeeded.
(( failed )) && fail_test

success
