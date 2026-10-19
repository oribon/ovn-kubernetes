# Egress traffic selection

`EgressTrafficClass` defines destination CIDRs and optional TCP, UDP, or SCTP
ports. An EgressIP's `trafficSelector` selects these resources by label. Only
matching traffic uses that EgressIP; other traffic follows its normal egress
path or a coexisting default EgressIP.

Enable `--enable-egress-traffic-class` on cluster-manager, ovnkube-controller,
and ovnkube-node. The flag defaults to false and implies `--enable-egress-ip`.
For Helm use `global.enableEgressTrafficClass=true`; for Kind use
`contrib/kind.sh --enable-egress-traffic-class`. The deployment passes
`OVN_EGRESS_TRAFFIC_CLASS_ENABLE` directly to the binary's CLI configuration.
Installing the CRD alone does not enable the feature. With the gate disabled,
controllers ignore `trafficSelector` and use ordinary EgressIP behavior.

## Configure traffic

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressTrafficClass
metadata:
  name: signaling
  labels:
    traffic: signaling
spec:
  destinations:
  - cidr: 192.0.2.0/24
    protocols:
    - udp:
        destinationPort:
          number: 5060
    - udp:
        sourcePort:
          range:
            start: 16384
            end: 32767
  - cidr: 2001:db8:100::/64
    protocols:
    - tcp:
        destinationPort:
          range:
            start: 5060
            end: 5062
---
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: signaling
spec:
  egressIPs:
  - 198.51.100.101
  namespaceSelector:
    matchLabels:
      tenant: example
  podSelector:
    matchLabels:
      app: phone
  trafficSelector:
    matchLabels:
      traffic: signaling
```

Each class contains 1–25 destinations, each with a network CIDR. Omit
`protocols` to match all protocols and ports for that CIDR. Destinations and
protocol entries are ORed. Each protocol entry sets exactly one of `tcp`,
`udp`, or `sctp`. Within an entry, source and destination port restrictions
are ANDed. An omitted port matches all ports; a specified port contains
exactly one `number` or inclusive `range`, between 1 and 65535.

Classes selected by one EgressIP are combined. Label or destination changes
update steering and SNAT without recreating the EgressIP. A selector matching
no class programs no matching traffic. Omitting `trafficSelector` retains
catch-all behavior; an explicitly empty selector is invalid.
Expression operators must be `In` or `NotIn` with a nonempty values list, or
`Exists` or `DoesNotExist` without values.

## Supported arrangements

1. Use a selective EgressIP on a secondary host interface for chosen
   destinations. Unmatched traffic uses normal OVN egress.
2. Add a catch-all EgressIP on the OVN network for remaining traffic while
   selective secondary-host EgressIPs handle their destinations.
3. Select multiple classes with one EgressIP to use one source address for
   several destination networks or applications.
4. Give a pod multiple selective EgressIPs, including EgressIPs on the same
   secondary interface, with disjoint CIDR or L4 matches.

For primary user-defined networks, selective OVN-network EgressIPs retain
the existing packet-mark and external-bridge OVS SNAT pipeline. Both Layer 2
and Layer 3 topologies use destination and L4 restrictions in their existing
steering and marking policies. Secondary-host EgressIPs on UDNs are unsupported.

A catch-all secondary-host EgressIP combined with a selective OVN-network
EgressIP is unsupported. Multiple catch-all EgressIPs selecting one pod retain
the existing undefined selection behavior. This feature adds no longest-prefix
or other arbitration between overlapping selective EgressIPs.

## Observe and troubleshoot

```sh
kubectl get egresstrafficclasses
kubectl get egressip signaling -o yaml
kubectl describe egressip signaling
```

`TrafficSelectorResolved=True`, reason `DestinationsFound`, means selected
classes contain destinations. `False`, reason `NoMatchingEgressTrafficClass`,
and a `NoTrafficClass` warning indicate that labels resolve to no destinations.
Check class labels and the EgressIP selector; allocating an address alone does
not cause a selective EgressIP to match traffic.

`TrafficSelectorResolved=False`, reason `InvalidTrafficSelection`, identifies
an invalid stored selector. Its traffic matches nothing, and `TrafficConflict`
is `Unknown` until the selection is corrected. Other EgressIPs continue to
reconcile normally.

`TrafficConflict=True`, reason `OverlappingTraffic`, identifies a peer and
shared pod with intersecting CIDR and L4 matches. Unset ports are wildcards:
UDP destination 5060 and UDP source 16384–32767 can match the same packet.
Definitions selected by the same EgressIP do not conflict with each other.
The condition clears when the overlap or shared workload disappears.
Conflict reporting does not reject or choose a winner; adjust the definitions.

OVN selective reroute policies use priority 100; catch-all policies use 99.
OVN port ranges are rendered as inclusive lower and upper comparisons.
Selective SNAT uses the same destination expression and takes precedence over
fallback SNAT. UDN host-CIDR policies also use priority 99, retaining the
priority limitation described by the enhancement proposal.
UDN gateway marking uses 96 for selective matches and 95 for catch-alls so
the fallback cannot claim matching traffic when a packet arrives unmarked.

For secondary-host EgressIPs, check `ip rule` for selective rules at 6000 and
catch-all rules at 6001. Destination routes retain the secondary interface's
gateway and share its routing table. In the OVN-Kubernetes nftables table,
`egress-ip-traffic-snat` precedes the existing catch-all SNAT maps and matches
the pod source, output interface, destination, protocol, and ports.

## Upgrade and rollback

Install the new CRD and updated EgressIP schema before enabling the gate.
Upgrade all components before creating selective resources: an old binary or
a component with the gate disabled interprets the EgressIP as catch-all.
Restart repair migrates owned catch-all OVN policies from priority 100 to 99,
refreshes destination matches, and reconciles Linux priorities 6000 and 6001.

Before rollback, save and remove all EgressIP resources, including catch-alls,
while the updated controllers are running. Wait for their owned reroute
policies at 99/100, gateway marking at 95/96, host rules at 6000/6001, and selective SNAT entries to be
removed. Remove the classes once unused. Downgrade the components, then
recreate the desired catch-all EgressIPs without `trafficSelector`; the old
binaries recreate their original priorities. Plan for egress interruption
during this procedure. Old binaries do not understand priority 99 or 6001,
and disabling the gate alone does not migrate configuration for them.
