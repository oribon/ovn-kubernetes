# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [EgressTrafficClass](#egresstrafficclass)
- [EgressTrafficClassList](#egresstrafficclasslist)



#### Destination



Destination matches a network, optionally restricted to L4 protocols and ports.



_Appears in:_
- [EgressTrafficClassSpec](#egresstrafficclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _string_ | CIDR must use the canonical network address. |  | MaxLength: 43 <br /> |
| `protocols` _[ProtocolMatch](#protocolmatch) array_ | Protocols are ORed together; omission matches all protocols and ports. |  | Optional: \{\} <br /> |


#### EgressTrafficClass



EgressTrafficClass defines destinations selectable by EgressIP resources.



_Appears in:_
- [EgressTrafficClassList](#egresstrafficclasslist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `EgressTrafficClass` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EgressTrafficClassSpec](#egresstrafficclassspec)_ |  |  |  |


#### EgressTrafficClassList



EgressTrafficClassList is a list of EgressTrafficClasses.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `EgressTrafficClassList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[EgressTrafficClass](#egresstrafficclass) array_ |  |  |  |


#### EgressTrafficClassSpec



EgressTrafficClassSpec defines destination matches.



_Appears in:_
- [EgressTrafficClass](#egresstrafficclass)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `destinations` _[Destination](#destination) array_ | Destinations are ORed together. |  | MaxItems: 25 <br />MinItems: 1 <br /> |


#### Port



Port selects exactly one port number or inclusive range.



_Appears in:_
- [PortProtocol](#portprotocol)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `number` _integer_ | Number selects one port. |  | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `range` _[PortRange](#portrange)_ | Range selects an inclusive range of ports. |  | Optional: \{\} <br /> |


#### PortProtocol



PortProtocol optionally restricts both remote and local ports (AND semantics).



_Appears in:_
- [ProtocolMatch](#protocolmatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `destinationPort` _[Port](#port)_ | DestinationPort matches the remote port. |  | Optional: \{\} <br /> |
| `sourcePort` _[Port](#port)_ | SourcePort matches the local pod port. |  | Optional: \{\} <br /> |


#### PortRange



PortRange is an inclusive range of transport ports.



_Appears in:_
- [Port](#port)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `start` _integer_ | Start is the first port. |  | Maximum: 65535 <br />Minimum: 1 <br /> |
| `end` _integer_ | End is the last port. |  | Maximum: 65535 <br />Minimum: 1 <br /> |


#### ProtocolMatch



ProtocolMatch selects exactly one transport protocol.



_Appears in:_
- [Destination](#destination)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tcp` _[PortProtocol](#portprotocol)_ | TCP selects TCP traffic. |  | Optional: \{\} <br /> |
| `udp` _[PortProtocol](#portprotocol)_ | UDP selects UDP traffic. |  | Optional: \{\} <br /> |
| `sctp` _[PortProtocol](#portprotocol)_ | SCTP selects SCTP traffic. |  | Optional: \{\} <br /> |
