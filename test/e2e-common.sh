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

# This script provides helper methods to perform cluster actions.
source "$(dirname "${BASH_SOURCE[0]}")/../vendor/knative.dev/hack/e2e-tests.sh"

# The previous serving release, installed by the operator. This value should be in the semantic format of major.minor.
readonly PREVIOUS_SERVING_RELEASE_VERSION="1.21"
# The previous eventing release, installed by the operator. This value should be in the semantic format of major.minor.
readonly PREVIOUS_EVENTING_RELEASE_VERSION="1.21"
# The target serving/eventing release to upgrade, installed by the operator. It can be a release available under
# kodata or an incoming new release. This value should be in the semantic format of major.minor.
readonly TARGET_RELEASE_VERSION="latest"
# This is the branch name of knative repos, where we run the upgrade tests.
readonly KNATIVE_REPO_BRANCH="${PULL_BASE_REF}"
# Namespaces used for tests
# This environment variable TEST_NAMESPACE defines the namespace to install Knative Serving.
export TEST_NAMESPACE="${TEST_NAMESPACE:-knative-operator-testing}"
export SYSTEM_NAMESPACE=${TEST_NAMESPACE}
export TEST_OPERATOR_NAMESPACE="knative-operator"
# This environment variable TEST_EVENTING_NAMESPACE defines the namespace to install Knative Eventing.
# It is different from the namespace to install Knative Serving.
# We will use only one namespace, when Knative supports both components can coexist under one namespace.
export TEST_EVENTING_NAMESPACE="knative-eventing"
export TEST_RESOURCE="knative"
export TEST_EVENTING_MONITORING_NAMESPACE="knative-monitoring"
export KO_FLAGS="${KO_FLAGS:-}"
export INGRESS_CLASS=${INGRESS_CLASS:-istio.ingress.networking.knative.dev}
export TIMEOUT_CI=30m

# Boolean used to indicate whether to generate serving YAML based on the latest code in the branch KNATIVE_SERVING_REPO_BRANCH.
GENERATE_SERVING_YAML=0

readonly OPERATOR_DIR="$(dirname "${BASH_SOURCE[0]}")/.."
readonly KNATIVE_DIR=$(dirname ${OPERATOR_DIR})
release_yaml="$(mktemp)"
release_eventing_yaml="$(mktemp)"

readonly SERVING_ARTIFACTS=("serving" "serving-crds.yaml" "serving-core.yaml" "serving-hpa.yaml" "serving-post-install-jobs.yaml")
readonly EVENTING_ARTIFACTS=("eventing" "eventing-crds.yaml" "eventing-core.yaml" "in-memory-channel.yaml" "mt-channel-broker.yaml"
  "eventing-post-install.yaml" "eventing-tls-networking.yaml")

function is_ingress_class() {
  [[ "${INGRESS_CLASS}" == *"${1}"* ]]
}

# Add function call to trap
# Parameters: $1 - Function to call
#             $2...$n - Signals for trap
function add_trap() {
  local cmd=$1
  shift
  for trap_signal in $@; do
    local current_trap="$(trap -p $trap_signal | cut -d\' -f2)"
    local new_cmd="($cmd)"
    [[ -n "${current_trap}" ]] && new_cmd="${current_trap};${new_cmd}"
    trap -- "${new_cmd}" $trap_signal
  done
}

# Setup and run kail in the background to collect logs
# from all pods.
function test_setup_logging() {
  echo ">> Setting up logging..."

  # Install kail if needed.
  if ! which kail > /dev/null; then
    bash <( curl -sfL https://raw.githubusercontent.com/boz/kail/master/godownloader.sh) -b "$GOPATH/bin"
  fi

  # Capture all logs.
  kail > ${ARTIFACTS}/k8s.log-$(basename ${E2E_SCRIPT}).txt &
  local kail_pid=$!
  # Clean up kail so it doesn't interfere with job shutting down
  add_trap "kill $kail_pid || true" EXIT
}

# Generic test setup. Used by the common test scripts.
function test_setup() {
  test_setup_logging
}

# Download the repository of Knative. The purpose of this function is to download the source code of
# knative component for further use, based on component name and branch name.
# Parameters:
#  $1 - component repo name, either knative/serving or knative/eventing,
#  $2 - component name,
#  $3 - branch of the repository.
function download_knative() {
  local component_repo component_name
  component_repo=$1
  component_name=$2
  # Go the directory to download the source code of knative
  cd ${KNATIVE_DIR}
  # Download the source code of knative
  git clone "https://github.com/${component_repo}.git" "${component_name}"
  cd "${component_name}"
  local branch=$3
  if [ -n "${branch}" ] ; then
    git fetch origin ${branch}:${branch}
    git checkout ${branch}
  fi
  cd ${OPERATOR_DIR}
}

# Install Istio.
function install_istio() {
  echo ">> Installing Istio"
  curl -sL https://istio.io/downloadIstioctl | ISTIO_VERSION=1.25.2 sh -
  $HOME/.istioctl/bin/istioctl install --set values.cni.cniBinDir=/home/kubernetes/bin -y
}

function create_namespace() {
  echo ">> Creating test namespaces for knative operator"
  kubectl get ns ${TEST_OPERATOR_NAMESPACE} || kubectl create namespace ${TEST_OPERATOR_NAMESPACE}
  echo ">> Creating test namespaces for knative serving and eventing"
  # All the custom resources and Knative Serving resources are created under this TEST_NAMESPACE.
  kubectl get ns ${TEST_NAMESPACE} || kubectl create namespace ${TEST_NAMESPACE}
  kubectl get ns ${TEST_EVENTING_NAMESPACE} || kubectl create namespace ${TEST_EVENTING_NAMESPACE}
}

function download_latest_release() {
  download_nightly_artifacts "${SERVING_ARTIFACTS[@]}"
  download_nightly_artifacts "${EVENTING_ARTIFACTS[@]}"
}

function download_nightly_artifacts() {
  array=("$@")
  component=${array[0]}
  unset array[0]
  counter=0
  linkprefix="https://storage.googleapis.com/knative-nightly/${component}/latest"
  version_exists=$(if_version_exists ${TARGET_RELEASE_VERSION} "knative-${component}")
  if [ "${version_exists}" == "no" ]; then
    header "Download the nightly build as the target version for Knative ${component}"
    knative_version_dir=${OPERATOR_DIR}/cmd/operator/kodata/knative-${component}/${TARGET_RELEASE_VERSION}
    mkdir ${knative_version_dir}
    for artifact in "${array[@]}";
      do
        ((counter=counter+1))
        wget ${linkprefix}/${artifact} -O ${knative_version_dir}/${counter}-${artifact}
      done
    if [ "${component}" == "serving" ]; then
      # Download the latest net-istio into the ingress directory.
      ingress_version_dir=${OPERATOR_DIR}/cmd/operator/kodata/ingress/${TARGET_RELEASE_VERSION}/istio
      mkdir -p ${ingress_version_dir}
      wget https://storage.googleapis.com/knative-nightly/net-istio/latest/net-istio.yaml -O ${ingress_version_dir}/net-istio.yaml
    fi
  fi
}

function install_operator() {
  create_namespace
  if is_ingress_class istio; then
    install_istio || fail_test "Istio installation failed"
  fi
  cd ${OPERATOR_DIR}
  download_latest_release
  header "Installing Knative operator"
  # Deploy the operator
  ko apply ${KO_FLAGS} -f config/
  wait_until_pods_running ${TEST_OPERATOR_NAMESPACE} || fail_test "Operator did not come up"
}

# Uninstalls Knative Serving from the current cluster.
function knative_teardown() {
  echo ">> Uninstalling Knative serving"
  echo ">> Bringing down Serving"
  kubectl delete -n $TEST_NAMESPACE KnativeServing --all
  echo ">> Bringing down Eventing"
  kubectl delete -n $TEST_NAMESPACE KnativeEventing --all
  echo ">> Bringing down Istio"
  $HOME/.istioctl/bin/istioctl x uninstall --purge
  kubectl delete --ignore-not-found=true clusterrolebinding cluster-admin-binding
  echo ">> Bringing down Operator"
  ko delete --ignore-not-found=true -f config/ || return 1
  echo ">> Removing test namespaces"
  kubectl delete all --all --ignore-not-found --now --timeout 60s -n $TEST_NAMESPACE
  kubectl delete --ignore-not-found --now --timeout 300s namespace $TEST_NAMESPACE
}

function wait_for_file() {
  local file timeout waits
  file="$1"
  waits=300
  timeout=$waits

  echo "Waiting for existence of file: ${file}"

  while [ ! -f "${file}" ]; do
    # When the timeout is equal to zero, show an error and leave the loop.
    if [ "${timeout}" == 0 ]; then
      echo "ERROR: Timeout (${waits}s) while waiting for the file ${file}."
      return 1
    fi

    sleep 1

    # Decrease the timeout of one
    ((timeout--))
  done
  return 0
}

function install_previous_operator_release() {
  install_operator
  install_previous_knative
}

function install_previous_knative() {
  header "Create the custom resources for Knative of the previous version"
  create_knative_serving ${PREVIOUS_SERVING_RELEASE_VERSION}
  create_knative_eventing ${PREVIOUS_EVENTING_RELEASE_VERSION}
}

function create_knative_serving() {
  version=${1}
  echo ">> Creating the custom resource of Knative Serving:"
  cat <<EOF | kubectl apply -f -
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: ${TEST_RESOURCE}
  namespace: ${TEST_NAMESPACE}
spec:
  version: "${version}"
  config:
    domain:
      example.com: |
    tracing:
      backend: "zipkin"
      zipkin-endpoint: "http://zipkin.${TEST_EVENTING_MONITORING_NAMESPACE}.svc:9411/api/v2/spans"
      debug: "true"
      sample-rate: "1.0"
EOF
}

function create_knative_eventing() {
  version=${1}
  echo ">> Creating the custom resource of Knative Eventing:"
  cat <<-EOF | kubectl apply -f -
apiVersion: operator.knative.dev/v1beta1
kind: KnativeEventing
metadata:
  name: ${TEST_RESOURCE}
  namespace: ${TEST_EVENTING_NAMESPACE}
spec:
  version: "${version}"
  config:
    tracing:
      backend: "zipkin"
      zipkin-endpoint: "http://zipkin.${TEST_EVENTING_MONITORING_NAMESPACE}.svc:9411/api/v2/spans"
      debug: "true"
      sample-rate: "1.0"
EOF
}

function create_latest_custom_resource() {
  echo ">> Creating the custom resource of Knative Serving:"
  cat <<-EOF | kubectl apply -f -
apiVersion: operator.knative.dev/v1beta1
kind: KnativeServing
metadata:
  name: ${TEST_RESOURCE}
  namespace: ${TEST_NAMESPACE}
spec:
  version: "${TARGET_RELEASE_VERSION}"
  config:
    domain:
      example.com: |
    tracing:
      backend: "zipkin"
      zipkin-endpoint: "http://zipkin.${TEST_EVENTING_MONITORING_NAMESPACE}.svc:9411/api/v2/spans"
      debug: "true"
      sample-rate: "1.0"
EOF

  echo ">> Creating the custom resource of Knative Eventing:"
  cat <<-EOF | kubectl apply -f -
apiVersion: operator.knative.dev/v1beta1
kind: KnativeEventing
metadata:
  name: ${TEST_RESOURCE}
  namespace: ${TEST_EVENTING_NAMESPACE}
spec:
  version: "${TARGET_RELEASE_VERSION}"
  config:
    tracing:
      backend: "zipkin"
      zipkin-endpoint: "http://zipkin.${TEST_EVENTING_MONITORING_NAMESPACE}.svc:9411/api/v2/spans"
      debug: "true"
      sample-rate: "1.0"
EOF
}

function if_version_exists() {
  version=$1
  component=$2
  knative_dir=${OPERATOR_DIR}/cmd/operator/kodata/${component}
  versions=$(ls ${knative_dir})
  for eachversion in ${versions}
  do
    if [[ "${eachversion}" == ${version}* ]]; then
      echo "yes"
      exit
    fi
  done
  echo "no"
}

# ---------------------------------------------------------------------------
# Multi-cluster e2e helpers (Cluster Inventory API / spoke cluster bootstrap)
#
# Activated only when TEST_MULTICLUSTER_E2E=1 is exported. None of these
# functions are referenced from the existing single-cluster code paths, and
# they intentionally do not modify the helpers above.
# ---------------------------------------------------------------------------

# Note: intentionally not marked readonly so the file can be sourced more than
# once (e.g. from interactive shells or wrapping harnesses) and so individual
# variables can be overridden from the environment.
: "${SPOKE_CLUSTER_NAME:=spoke}"
: "${SPOKE_KUBECONFIG:=/tmp/spoke.kubeconfig}"
: "${SPOKE_HOST_KUBECONFIG:=/tmp/spoke-host.kubeconfig}"
: "${CLUSTER_INVENTORY_CRD_URL:=https://raw.githubusercontent.com/kubernetes-sigs/cluster-inventory-api/v0.1.0/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml}"
: "${MC_PROVIDER_CONFIGMAP:=clusterprofile-provider-file}"
: "${MC_PROVIDER_TOKEN_SECRET:=clusterprofile-provider-token}"
: "${MC_PROVIDER_MOUNT_PATH:=/etc/cluster-inventory}"
: "${MC_PROVIDER_TOKEN_MOUNT_PATH:=/etc/cluster-inventory/access}"
: "${MC_PROVIDER_NAME:=e2e-static-token}"
export SPOKE_CLUSTER_NAME SPOKE_KUBECONFIG SPOKE_HOST_KUBECONFIG
export CLUSTER_INVENTORY_CRD_URL MC_PROVIDER_CONFIGMAP MC_PROVIDER_TOKEN_SECRET
export MC_PROVIDER_MOUNT_PATH MC_PROVIDER_TOKEN_MOUNT_PATH MC_PROVIDER_NAME

function create_spoke_cluster() {
  echo ">> Creating spoke KinD cluster: ${SPOKE_CLUSTER_NAME}"
  if kind get clusters 2>/dev/null | grep -q "^${SPOKE_CLUSTER_NAME}$"; then
    echo ">> Spoke cluster already exists, reusing"
  else
    kind create cluster --name "${SPOKE_CLUSTER_NAME}" --wait 120s || return 1
  fi
  # IMPORTANT: --internal is required so the hub pod can reach the spoke API
  # server via the shared 'kind' docker bridge. The host kubeconfig
  # (127.0.0.1:<port>) will not work from inside a pod.
  kind get kubeconfig --internal --name "${SPOKE_CLUSTER_NAME}" > "${SPOKE_KUBECONFIG}" || return 1
  # The --internal kubeconfig cannot be dialled from the runner host, so we
  # also export a host-reachable copy used for `kubectl wait` and dumps.
  kind get kubeconfig --name "${SPOKE_CLUSTER_NAME}" > "${SPOKE_HOST_KUBECONFIG}" || return 1
  export SPOKE_KUBECONFIG SPOKE_HOST_KUBECONFIG
  echo ">> Spoke kubeconfig written to ${SPOKE_KUBECONFIG} (internal) and ${SPOKE_HOST_KUBECONFIG} (host)"

  # Wait for the spoke API server to be fully ready before issuing
  # TokenRequest API calls. `kind create --wait` only blocks on node Ready
  # condition; the SA token controller and TokenRequest API may still be
  # racing.
  echo ">> Waiting for spoke nodes and core components"
  KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl wait --for=condition=Ready node --all --timeout=120s || return 1
  KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl -n kube-system rollout status deployment/coredns --timeout=120s || return 1
}

function delete_spoke_cluster() {
  if kind get clusters 2>/dev/null | grep -q "^${SPOKE_CLUSTER_NAME}$"; then
    kind delete cluster --name "${SPOKE_CLUSTER_NAME}" || true
  fi
  rm -f "${SPOKE_KUBECONFIG}" "${SPOKE_HOST_KUBECONFIG}"
}

# dump_spoke_state writes a snapshot of the spoke cluster (resources + events)
# to ARTIFACTS so failures in CI leave actionable forensic data behind.
function dump_spoke_state() {
  if [[ -z "${SPOKE_HOST_KUBECONFIG:-}" || ! -f "${SPOKE_HOST_KUBECONFIG}" ]]; then
    return 0
  fi
  local out="${ARTIFACTS:-/tmp}/spoke-dump.txt"
  echo ">> Dumping spoke cluster state to ${out}"
  {
    echo "=== kubectl get all -A ==="
    KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl get all -A -o wide || true
    echo
    echo "=== kubectl get events -A ==="
    KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl get events -A --sort-by=.lastTimestamp || true
    echo
    echo "=== kubectl get nodes ==="
    KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl get nodes -o wide || true
  } > "${out}" 2>&1 || true
}

function install_cluster_inventory_crd() {
  echo ">> Installing ClusterProfile CRD on hub"
  kubectl apply -f "${CLUSTER_INVENTORY_CRD_URL}" || return 1
  kubectl wait --for=condition=Established --timeout=60s \
    crd/clusterprofiles.multicluster.x-k8s.io || return 1
}

function create_spoke_kubeconfig_secret() {
  local ns="$1"
  kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "${ns}" create secret generic spoke-kubeconfig \
    --from-file=kubeconfig="${SPOKE_KUBECONFIG}" \
    --dry-run=client -o yaml | kubectl apply -f -
}

# _spoke_bootstrap_token creates a cluster-admin ServiceAccount on the spoke
# and prints its bearer token to stdout. The token is later exposed to the
# hub operator pod through the access provider exec plugin.
#
# The token line itself is emitted to stdout (so callers can capture it via
# command substitution) but xtrace is suppressed around the `create token`
# call so the bearer token never lands in CI logs verbatim when the harness
# is run with `set -x`.
function _spoke_bootstrap_token() {
  local sa_ns="kube-system"
  local sa_name="knative-operator-e2e"
  KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl -n "${sa_ns}" create serviceaccount "${sa_name}" \
    --dry-run=client -o yaml | KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl apply -f - >/dev/null
  KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl create clusterrolebinding "${sa_name}" \
    --clusterrole=cluster-admin \
    --serviceaccount="${sa_ns}:${sa_name}" \
    --dry-run=client -o yaml | KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl apply -f - >/dev/null
  { set +x; } 2>/dev/null
  KUBECONFIG="${SPOKE_HOST_KUBECONFIG}" kubectl -n "${sa_ns}" create token "${sa_name}" --duration=24h
}

# _spoke_endpoint extracts the internal API server URL from the spoke
# kubeconfig (the one obtained with --internal).
function _spoke_endpoint() {
  KUBECONFIG="${SPOKE_KUBECONFIG}" kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'
}

# _spoke_ca_b64 extracts the spoke API server CA bundle (already base64-encoded
# in the kubeconfig) and prints it.
function _spoke_ca_b64() {
  KUBECONFIG="${SPOKE_KUBECONFIG}" kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}'
}

function apply_cluster_profile() {
  local cp_namespace="${1:-default}"
  echo ">> Applying ClusterProfile CR for spoke in namespace ${cp_namespace}"
  kubectl create namespace "${cp_namespace}" --dry-run=client -o yaml | kubectl apply -f -

  export SPOKE_CLUSTER_NAME
  SPOKE_INTERNAL_ENDPOINT="$(_spoke_endpoint)" || return 1
  SPOKE_CA_DATA_B64="$(_spoke_ca_b64)" || return 1
  export SPOKE_INTERNAL_ENDPOINT SPOKE_CA_DATA_B64

  envsubst < test/config/multicluster/clusterprofile.yaml.tmpl \
    | kubectl -n "${cp_namespace}" apply -f - || return 1

  # Wait until the ClusterProfile is observable through the hub apiserver so
  # the operator informer has a chance to populate before tests start
  # reconciling.
  local i
  for i in $(seq 1 30); do
    if kubectl -n "${cp_namespace}" get clusterprofile "${SPOKE_CLUSTER_NAME}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "ERROR: timed out waiting for ClusterProfile/${SPOKE_CLUSTER_NAME} in ${cp_namespace}" >&2
  return 1
}

# install_access_provider_config builds the access-provider plumbing
# for the hub operator:
#
#   1. A ConfigMap (MC_PROVIDER_CONFIGMAP) with two non-secret entries:
#        - config.json : the access-provider JSON consumed by --clusterprofile-provider-file
#        - exec.sh     : a tiny shell exec plugin that reads the token from the mounted Secret
#   2. A Secret (MC_PROVIDER_TOKEN_SECRET) carrying ONLY the bearer token under key "token".
#   3. A patch on the operator deployment that mounts both volumes and adds
#      --clusterprofile-provider-file. The patch uses a JSON patch (`add`)
#      so it does not clobber any existing args/volumes on the container.
#
# Requires: TEST_MULTICLUSTER_E2E=1 and an operator image with /bin/sh and
# /bin/cat available (see KO_DEFAULTBASEIMAGE override in e2e-tests.sh).
function install_access_provider_config() {
  echo ">> Installing access provider ConfigMap/Secret and patching operator deployment"
  local token
  token="$(_spoke_bootstrap_token)" || return 1
  if [[ -z "${token}" ]]; then
    echo "ERROR: failed to obtain spoke service account token" >&2
    return 1
  fi

  local tmpdir
  tmpdir="$(mktemp -d)" || return 1

  cat > "${tmpdir}/provider-config.json" <<EOF
{
  "providers": [
    {
      "name": "${MC_PROVIDER_NAME}",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1",
        "command": "/bin/sh",
        "args": ["${MC_PROVIDER_MOUNT_PATH}/exec.sh"],
        "interactiveMode": "Never"
      }
    }
  ]
}
EOF

  # The exec plugin reads the bearer token from a file mounted from a Secret
  # at runtime. The token value is NOT baked into the script so a
  # `kubectl describe configmap` cannot leak it.
  cat > "${tmpdir}/exec.sh" <<EOF
#!/bin/sh
TOKEN=\$(cat ${MC_PROVIDER_TOKEN_MOUNT_PATH}/token)
cat <<JSON
{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"\${TOKEN}"}}
JSON
EOF
  chmod +x "${tmpdir}/exec.sh"

  kubectl -n "${TEST_OPERATOR_NAMESPACE}" create configmap "${MC_PROVIDER_CONFIGMAP}" \
    --from-file=config.json="${tmpdir}/provider-config.json" \
    --from-file=exec.sh="${tmpdir}/exec.sh" \
    --dry-run=client -o yaml | kubectl apply -f - || { rm -rf "${tmpdir}"; return 1; }

  # Write the token to a temp file so it never appears on a kubectl command
  # line (which would land in `ps`/audit logs).
  local token_file="${tmpdir}/token"
  ( { set +x; } 2>/dev/null; printf '%s' "${token}" > "${token_file}" )
  chmod 600 "${token_file}"

  kubectl -n "${TEST_OPERATOR_NAMESPACE}" create secret generic "${MC_PROVIDER_TOKEN_SECRET}" \
    --from-file=token="${token_file}" \
    --dry-run=client -o yaml | kubectl apply -f - || { rm -rf "${tmpdir}"; return 1; }

  rm -rf "${tmpdir}"

  # Patch the operator deployment using a JSON patch with `add` operations.
  # config/manager/operator.yaml does not currently set `args` or `volumes`
  # on the knative-operator container, so the strategic-merge `args` clobber
  # risk is real if upstream ever adds defaults — using JSON patch with
  # explicit `add` keeps the patch surgical (whole-array replacement only
  # happens for the keys we explicitly add).
  kubectl -n "${TEST_OPERATOR_NAMESPACE}" patch deployment knative-operator \
    --type=json \
    -p "$(cat <<EOF
[
  {"op": "add", "path": "/spec/template/spec/containers/0/args", "value": ["--clusterprofile-provider-file=${MC_PROVIDER_MOUNT_PATH}/config.json"]},
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [
    {"name": "access-config", "configMap": {"name": "${MC_PROVIDER_CONFIGMAP}", "defaultMode": 493}},
    {"name": "provider-token", "secret": {"secretName": "${MC_PROVIDER_TOKEN_SECRET}", "defaultMode": 256}}
  ]},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [
    {"name": "access-config", "mountPath": "${MC_PROVIDER_MOUNT_PATH}", "readOnly": true},
    {"name": "provider-token", "mountPath": "${MC_PROVIDER_TOKEN_MOUNT_PATH}", "readOnly": true}
  ]}
]
EOF
)" || return 1

  kubectl -n "${TEST_OPERATOR_NAMESPACE}" rollout status deployment/knative-operator --timeout=180s || return 1
}

function setup_multicluster_e2e() {
  # Pre-flight: ensure required tools are available before we start spinning
  # up clusters. Failing fast here gives a clearer error than a half-created
  # KinD cluster with a "command not found" trace.
  local cmd
  for cmd in kind envsubst kubectl ko docker; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      echo "ERROR: required command not found: ${cmd}" >&2
      return 1
    fi
  done

  create_spoke_cluster || return 1
  install_cluster_inventory_crd || return 1
  install_access_provider_config || return 1
  apply_cluster_profile "default" || return 1
}
