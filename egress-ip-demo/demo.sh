#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

: "${KUBECONFIG:?Set KUBECONFIG to the kubeconfig of the existing KinD cluster}"
OCI_BIN="${OCI_BIN:-${KIND_EXPERIMENTAL_PROVIDER:-docker}}"
KIND_NETWORK="${KIND_NETWORK:-kind}"
namespace=egress-ip-demo
udp_source_port=$((30000 + RANDOM % 20000))

bold='' dim='' reset=''
default_color='' a_color='' b_color='' success_color='' error_color=''
if [[ -t 1 && ${TERM:-dumb} != dumb && -z ${NO_COLOR:-} ]]; then
    bold=$'\033[1m'
    dim=$'\033[2m'
    reset=$'\033[0m'
    default_color=$'\033[33m'
    a_color=$'\033[36m'
    b_color=$'\033[35m'
    success_color=$'\033[32m'
    error_color=$'\033[31m'
fi

for tool in kubectl jq python3 "$OCI_BIN"; do
    command -v "$tool" >/dev/null || { echo "Missing command: $tool" >&2; exit 1; }
done
[[ -t 0 ]] || { echo 'Run this demo in a terminal; it pauses for Enter.' >&2; exit 1; }
kubectl get crd trafficclasses.k8s.ovn.org >/dev/null
kubectl -n "$namespace" wait --for=condition=Ready pod/client --timeout=30s >/dev/null

backend_ip() {
    "$OCI_BIN" inspect "egress-ip-demo-backend-$1" | jq -er --arg network "$KIND_NETWORK" \
        '.[0] | select(.State.Running) | .NetworkSettings.Networks[$network].IPAddress | select(length > 0)'
}
backend_a=$(backend_ip a)
backend_b=$(backend_ip b)
nodes=$(kubectl get nodes -o json)
owners=$(kubectl get egressips -o json)
containers=$("$OCI_BIN" ps -aq --filter "network=$KIND_NETWORK" | xargs "$OCI_BIN" inspect)

# Choose unused addresses on an eligible node's primary subnet, as the E2Es do.
allocation=$(python3 - "$nodes" "$owners" "$containers" "$KIND_NETWORK" <<'PY'
import ipaddress
import itertools
import json
import sys

nodes, owners, containers = (json.loads(value) for value in sys.argv[1:4])
network_name = sys.argv[4]
attached = {
    container["Name"].lstrip("/"): container["NetworkSettings"]["Networks"].get(network_name, {})
    for container in containers
}
used = set()
for interface in attached.values():
    for address in (interface.get("IPAddress"), interface.get("Gateway"),
                    (interface.get("IPAMConfig") or {}).get("IPv4Address")):
        if address:
            used.add(ipaddress.ip_address(address))

demo_names = ["egress-ip-demo-default", "egress-ip-demo-a", "egress-ip-demo-b"]
previous = {}
for owner in owners["items"]:
    addresses = owner["spec"].get("egressIPs", [])
    if owner["metadata"]["name"] in demo_names:
        previous[owner["metadata"]["name"]] = addresses
    else:
        used.update(ipaddress.ip_address(address) for address in addresses)
        used.update(ipaddress.ip_address(item["egressIP"]) for item in owner.get("status", {}).get("items", []))

nodes["items"].sort(key=lambda node: ("node-role.kubernetes.io/control-plane" in node["metadata"].get("labels", {}), node["metadata"]["name"]))
for node in nodes["items"]:
    name = node["metadata"]["name"]
    primary = json.loads(node["metadata"].get("annotations", {}).get("k8s.ovn.org/node-primary-ifaddr", "{}"))
    if not primary.get("ipv4") or not any(c["type"] == "Ready" and c["status"] == "True" for c in node.get("status", {}).get("conditions", [])):
        continue
    interface = ipaddress.ip_interface(primary["ipv4"])
    if attached.get(name, {}).get("IPAddress") != str(interface.ip):
        continue
    subnet = interface.network
    used.update((subnet.network_address, subnet.broadcast_address, interface.ip))
    addresses = []
    for owner_name in demo_names:
        candidates = itertools.chain(
            (ipaddress.ip_address(address) for address in previous.get(owner_name, [])),
            (subnet.broadcast_address - offset for offset in range(1, min(subnet.num_addresses, 65536))),
        )
        address = next((address for address in candidates if address in subnet and address not in used), None)
        if address is None:
            sys.exit("Not enough unused IPv4 addresses on the KinD node subnet.")
        used.add(address)
        addresses.append(str(address))
    print(name, *addresses)
    break
else:
    sys.exit("No Ready IPv4 KinD node on this runtime network. Check KUBECONFIG and run setup.sh first.")
PY
)
read -r egress_node default_ip a_ip b_ip <<< "$allocation"

source_color() {
    case "$1" in
        "$default_ip") printf '%s' "$default_color" ;;
        "$a_ip") printf '%s' "$a_color" ;;
        "$b_ip") printf '%s' "$b_color" ;;
    esac
}

stage() {
    printf '\n%s%s%s\n' "$bold" "$1" "$reset"
    printf '%s------------------------------------------------------------%s\n' "$dim" "$reset"
}

pause() {
    printf '\n%sEnter%s  %s  %s(Ctrl-C to stop)%s ' "$bold" "$reset" "$1" "$dim" "$reset"
    read -r
    printf '\n'
}

results_header() {
    printf '\n  %s%-12s %-10s %-15s %-15s %s%s\n' \
        "$bold" Destination Traffic 'Expected source' 'Observed source' Result "$reset"
}

show_and_apply() {
    local accent=$1 manifest line style traffic_selector=false
    manifest=$(cat)
    printf '\n'
    while IFS= read -r line; do
        case "$line" in
            '  trafficSelector:') traffic_selector=true ;;
            '    '*) ;;
            *) traffic_selector=false ;;
        esac
        case "$line" in
            'kind: EgressIP' | '  name:'* | *'egressIPs:'* | *'destinationCIDR:'* | *'trafficSelector:'* | \
            *'demo.ovn.org/backend:'* | *'protocols:'* | *'- tcp:'* | *'destinationPort:'* | *'number:'*)
                style="${bold}${accent}" ;;
            *)
                style=$dim
                if [[ $traffic_selector == true ]]; then
                    style="${bold}${accent}"
                fi ;;
        esac
        printf '  %s%s%s\n' "$style" "$line" "$reset"
    done <<< "$manifest"
    printf '\n'
    # Only the displayed copy is styled; kubectl receives the original YAML.
    printf '%s\n' "$manifest" | kubectl apply -f -
}

apply_egressip() {
    local name=$1 address=$2 backend=${3:-}
    {
        cat <<EOF
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: $name
spec:
  egressIPs: [$address]
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: $namespace
  podSelector:
    matchLabels:
      app: egress-ip-demo-client
EOF
        if [[ -n $backend ]]; then
            cat <<EOF
  trafficSelector:
    matchLabels:
      demo.ovn.org/backend: $backend
EOF
        fi
    } | show_and_apply "$(source_color "$address")"
    kubectl wait --for="jsonpath={.status.items[0].egressIP}=$address" "egressip/$name" --timeout=90s >/dev/null
    if [[ -n $backend ]]; then
        kubectl wait --for=condition=TrafficSelectorResolved=True "egressip/$name" --timeout=90s >/dev/null
        kubectl wait --for=condition=TrafficConflict=False "egressip/$name" --timeout=90s >/dev/null
    fi
}

apply_class() {
    local backend=$1 address=$2 protocol=${3:-all} accent=$a_color
    if [[ $backend == b ]]; then
        accent=$b_color
    fi
    {
        cat <<EOF
apiVersion: k8s.ovn.org/v1
kind: TrafficClass
metadata:
  name: egress-ip-demo-$backend
  labels:
    demo.ovn.org/backend: $backend
spec:
  matches:
    - destinationCIDR: $address/32
EOF
        if [[ $protocol == tcp ]]; then
            cat <<EOF
      protocols:
        - tcp:
            destinationPort:
              number: 8080
EOF
        fi
    } | show_and_apply "$accent"
}

probe() {
    local backend=$1 address=$2 protocol=$3 expected=$4 response='' observed='' attempt port=9090 color status row argument
    local destination_color=$a_color display_address
    local -a command
    if [[ $backend == B ]]; then
        destination_color=$b_color
    fi
    color=$(source_color "$expected")
    if [[ $protocol == TCP ]]; then
        port=8080
    fi
    for ((attempt = 0; attempt < 30; attempt++)); do
        printf '\n# %sBackend %s%s - %s port %s\n' "$destination_color" "$backend" "$reset" "$protocol" "$port"
        printf '%s$ kubectl -n %q exec client -- ' "$bold" "$namespace"
        if [[ $protocol == TCP ]]; then
            command=(curl --fail --silent --show-error --noproxy '*' \
                --connect-timeout 2 --max-time 3 "http://$address:8080/clientip")
            for argument in "${command[@]}"; do
                if [[ $argument == "http://$address:8080/clientip" ]]; then
                    printf '%s%q%s ' "$destination_color" "$argument" "${reset}${bold}"
                else
                    printf '%q ' "$argument"
                fi
            done
        else
            # A new UDP source port avoids showing an old conntrack SNAT decision after an edit.
            udp_source_port=$((udp_source_port + 1))
            command=(sh -c "printf clientip | nc -u -w 2 -p $udp_source_port $address 9090")
            display_address="${destination_color}${address}${reset}${bold}"
            printf "sh -c '%s'" "${command[2]/$address/$display_address}"
        fi
        printf '%s\n' "$reset"
        status=0
        response=$(kubectl -n "$namespace" exec client -- "${command[@]}" 2>&1) || status=$?
        response=${response//$'\r'/}
        observed=${response%:*}
        if [[ $status == 0 && $observed == "$expected" ]]; then
            printf '%s%s%s%s\n' "$bold" "$color" "$response" "$reset"
            printf -v row '  %-12s %-10s %s%-15s%s %s%s%-15s%s %sOK%s' \
                "Backend $backend" "$protocol/$port" "$color" "$expected" "$reset" \
                "$bold" "$color" "$observed" "$reset" "$success_color" "$reset"
            probe_results+=("$row")
            return
        fi
        printf '%s%s%s\n' "$error_color" "${response:-<no response>}" "$reset"
        printf '%sWaiting for source %s; retrying...%s\n' "$dim" "$expected" "$reset"
        sleep 1
    done
    printf '%sBackend %s (%s): expected %s; last response: %s%s\n' \
        "$error_color" "$backend" "$protocol" "$expected" "$response" "$reset" >&2
    return 1
}

probe_requests() {
    probe_results=()
    probe A "$backend_a" TCP "$1"
    probe B "$backend_b" TCP "$2"
    probe A "$backend_a" UDP "$3"
    printf '\n%sSummary%s\n' "$bold" "$reset"
    results_header
    printf '%s\n' "${probe_results[@]}"
}

printf '\n%sEgressIP traffic selection%s\n\n' "$bold" "$reset"
context=$(kubectl config current-context)
client_ip=$(kubectl -n "$namespace" get pod client -o json | jq -er \
    '.status.podIPs[].ip | select(contains(":") | not)')
printf '  %s%-38s%s     %sExternal backends (%s)%s\n' \
    "$bold" "KinD context: $context" "$reset" "$bold" "$KIND_NETWORK" "$reset"
printf '  +------------------------------------+     +------------------------------+\n'
printf '  | %-34s |     | %s%-28s%s |\n' '' "$a_color" "Backend A: $backend_a" "$reset"
printf '  | %s%-34s%s |---->| %-28s |\n' "$bold" 'Client pod' "$reset" 'TCP/8080  UDP/9090'
printf '  | %s%-34s%s |     +------------------------------+\n' "$bold" "$namespace/client" "$reset"
printf '  | %-34s |\n' "Pod IP: $client_ip"
printf '  | %-34s |     +------------------------------+\n' ''
printf '  | %-34s |---->| %s%-28s%s |\n' '' "$b_color" "Backend B: $backend_b" "$reset"
printf '  +------------------------------------+     | %-28s |\n' 'TCP/8080  UDP/9090'
printf '  %-38s     +------------------------------+\n' ''
printf '\n  Egress source IPs introduced during this demo:\n'
printf '  %sDefault %s%s   %sA %s%s   %sB %s%s\n' \
    "$default_color" "$default_ip" "$reset" "$a_color" "$a_ip" "$reset" "$b_color" "$b_ip" "$reset"

pause 'Start with the default EgressIP'
stage '1 / 3  Default egress'
echo 'Apply a catch-all EgressIP for the client pod.'
kubectl delete egressip egress-ip-demo-a egress-ip-demo-b --ignore-not-found --wait=true >/dev/null
kubectl label node "$egress_node" k8s.ovn.org/egress-assignable= --overwrite >/dev/null
apply_egressip egress-ip-demo-default "$default_ip"
pause 'Send requests using the default EgressIP'
probe_requests "$default_ip" "$default_ip" "$default_ip"

pause 'Apply destination-specific EgressIPs'
stage '2 / 3  Destination selection'
echo 'Each EgressIP selects a TrafficClass by label.'
apply_class a "$backend_a"
apply_class b "$backend_b"
apply_egressip egress-ip-demo-a "$a_ip" a
apply_egressip egress-ip-demo-b "$b_ip" b
kubectl get egressip egress-ip-demo-default egress-ip-demo-a egress-ip-demo-b
pause 'Repeat the requests with destination selection'
probe_requests "$a_ip" "$b_ip" "$a_ip"

pause "Update A's TrafficClass to match only TCP/8080"
stage '3 / 3  Protocol and port filtering'
echo "Update only A's TrafficClass. Keep the pod and EgressIP objects."
apply_class a "$backend_a" tcp
pause 'Check TCP selection and UDP fallback'
probe_requests "$a_ip" "$b_ip" "$default_ip"

printf '\n%sDemo complete.%s\n' "$bold" "$reset"
echo 'Resources are left in place for inspection. Rerun this script to replay the demo.'
