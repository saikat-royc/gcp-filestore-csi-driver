#!/bin/bash

# This script will install kustomize, which is a tool that simplifies patching
# Kubernetes manifests for different environments.
# https://github.com/kubernetes-sigs/kustomize

set -o nounset
set -o errexit

readonly INSTALL_DIR="${GOPATH}/src/sigs.k8s.io/gcp-filestore-csi-driver/bin"
readonly KUSTOMIZE_PATH="${INSTALL_DIR}/kustomize"

if [ ! -f "${INSTALL_DIR}" ]; then
  mkdir -p "${INSTALL_DIR}"
fi
if [ -f "kustomize" ]; then
  rm kustomize
fi

echo "installing kustomize"

where=$PWD
if [ -f $where/kustomize ]; then
  echo "A file named kustomize already exists (remove it first)."
  exit 1
fi

tmpDir=`mktemp -d`
if [[ ! "$tmpDir" || ! -d "$tmpDir" ]]; then
  echo "Could not create temp dir."
  exit 1
fi

function cleanup {
  rm -rf "$tmpDir"
}

trap cleanup EXIT

pushd $tmpDir >& /dev/null

opsys=windows
if [[ "$OSTYPE" == linux* ]]; then
  opsys=linux_amd64
elif [[ "$OSTYPE" == darwin* ]]; then
  opsys=darwin
fi

curl -s https://api.github.com/repos/kubernetes-sigs/kustomize/releases |\
  grep browser_download |\
  grep $opsys |\
  cut -d '"' -f 4 |\
  grep /kustomize/v3.9.4 |\
  sort | tail -n 1 |\
  xargs curl -s -O -L

tar xzf ./kustomize_v*_${opsys}.tar.gz

cp ./kustomize $where

popd >& /dev/null

./kustomize version

mv kustomize "${INSTALL_DIR}"
