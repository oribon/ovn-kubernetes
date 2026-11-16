#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

: "${KUBECONFIG:?Set KUBECONFIG to the kubeconfig of the existing KinD cluster}"
OCI_BIN="${OCI_BIN:-${KIND_EXPERIMENTAL_PROVIDER:-docker}}"
KIND_NETWORK="${KIND_NETWORK:-kind}"
AGNHOST_IMAGE="${AGNHOST_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.63.0}"
namespace=egress-ip-demo

for tool in kubectl jq "$OCI_BIN"; do
    command -v "$tool" >/dev/null || { echo "Missing command: $tool" >&2; exit 1; }
done

echo "Preparing demo in context $(kubectl config current-context), network $KIND_NETWORK"
node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
"$OCI_BIN" inspect "$node" | jq -e --arg network "$KIND_NETWORK" \
    '.[0].NetworkSettings.Networks[$network] != null' >/dev/null || {
    echo "The current Kubernetes context must point to a local KinD cluster on $KIND_NETWORK." >&2
    exit 1
}

for backend in a b; do
    name="egress-ip-demo-backend-$backend"
    if "$OCI_BIN" container inspect "$name" >/dev/null 2>&1; then
        echo "Reusing $name"
        if [[ $("$OCI_BIN" inspect --format '{{.State.Running}}' "$name") != true ]]; then
            "$OCI_BIN" start "$name" >/dev/null
        fi
        if ! "$OCI_BIN" inspect "$name" | jq -e --arg network "$KIND_NETWORK" \
            '.[0].NetworkSettings.Networks[$network] != null' >/dev/null; then
            "$OCI_BIN" network connect "$KIND_NETWORK" "$name"
        fi
    else
        echo "Creating $name"
        "$OCI_BIN" run --detach --name "$name" --network "$KIND_NETWORK" \
            --label app.kubernetes.io/part-of=egress-ip-demo \
            "$AGNHOST_IMAGE" netexec --http-port=8080 --udp-port=9090 >/dev/null
    fi
done

kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $namespace
---
apiVersion: v1
kind: Pod
metadata:
  name: client
  namespace: $namespace
  labels:
    app: egress-ip-demo-client
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: client
      image: $AGNHOST_IMAGE
      args: [netexec, --http-port=8080, --udp-port=-1]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      readinessProbe:
        tcpSocket:
          port: 8080
EOF
kubectl -n "$namespace" wait --for=condition=Ready pod/client --timeout=180s
kubectl -n "$namespace" exec client -- sh -c 'command -v curl && command -v nc' >/dev/null

for backend in a b; do
    name="egress-ip-demo-backend-$backend"
    ip=$("$OCI_BIN" inspect "$name" | jq -er --arg network "$KIND_NETWORK" \
        '.[0].NetworkSettings.Networks[$network].IPAddress | select(length > 0)')
    ready=false
    for ((attempt = 0; attempt < 30; attempt++)); do
        if kubectl -n "$namespace" exec client -- curl --fail --silent --show-error \
            --noproxy '*' --connect-timeout 2 --max-time 3 "http://$ip:8080/clientip" >/dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 1
    done
    if [[ $ready != true ]]; then
        "$OCI_BIN" logs --tail 20 "$name" >&2
        echo "Client cannot reach $name at $ip:8080." >&2
        exit 1
    fi
    echo "Backend $backend: $ip (TCP 8080, UDP 9090)"
done

echo "Setup ready. Run $(dirname "$0")/demo.sh with the same KUBECONFIG and runtime settings."
