#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

: "${KUBECONFIG:?Set KUBECONFIG to the kubeconfig of the existing KinD cluster}"
OCI_BIN="${OCI_BIN:-${KIND_EXPERIMENTAL_PROVIDER:-docker}}"

"$OCI_BIN" info >/dev/null
kubectl delete egressip egress-ip-demo-default egress-ip-demo-a egress-ip-demo-b --ignore-not-found
kubectl delete trafficclass egress-ip-demo-a egress-ip-demo-b --ignore-not-found
kubectl delete namespace egress-ip-demo --ignore-not-found

for backend in a b; do
    name="egress-ip-demo-backend-$backend"
    if "$OCI_BIN" container inspect "$name" >/dev/null 2>&1; then
        "$OCI_BIN" rm -f "$name"
    fi
done
