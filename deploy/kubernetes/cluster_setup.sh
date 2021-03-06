#!/bin/bash

set -x
set -o nounset
set -o errexit

mydir="$(dirname $0)"

source "$mydir/../common.sh"

# DEPLOY_VERSION should point to the overlays name.
ensure_var DEPLOY_VERSION
ensure_kustomize

if ! ${KUBECTL} get namespace "${GCFS_NS}" -v="${VERBOSITY}";
then
  ${KUBECTL} create namespace "${GCFS_NS}" -v="${VERBOSITY}"
fi

# GKE requires this extra cluster-admin rolebinding in order to create clusterroles
if ! kubectl get clusterrolebinding cluster-admin-binding; then
  kubectl create clusterrolebinding cluster-admin-binding --clusterrole cluster-admin --user $(gcloud config get-value account)
fi

if [ "${DEPLOY_VERSION}" != dev ]; then
  if ! kubectl get secret gcp-filestore-csi-driver-sa --namespace=$GCFS_NS; then
    kubectl create secret generic gcp-filestore-csi-driver-sa --from-file="$GCFS_SA_FILE" --namespace=$GCFS_NS
  fi
fi

readonly tmp_spec=/tmp/gcp-filestore-csi-driver-specs-generated.yaml
${KUSTOMIZE_PATH} build "${PKGDIR}/deploy/kubernetes/overlays/${DEPLOY_VERSION}" | tee $tmp_spec
${KUBECTL} apply -v="${VERBOSITY}" -f $tmp_spec
