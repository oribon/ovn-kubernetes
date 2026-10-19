// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +genclient:nonNamespaced
// +genclient:noStatus
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=etc,scope=Cluster
// +kubebuilder:printcolumn:name="Destinations",type=string,JSONPath=".spec.destinations[*].cidr"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressTrafficClass defines destinations selectable by EgressIP resources.
type EgressTrafficClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EgressTrafficClassSpec `json:"spec"`
}

// EgressTrafficClassSpec defines destination matches.
type EgressTrafficClassSpec struct {
	// Destinations are ORed together.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=25
	Destinations []Destination `json:"destinations"`
}

// Destination matches a network, optionally restricted to L4 protocols and ports.
type Destination struct {
	// CIDR must use the canonical network address.
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self) == cidr(self).masked()",message="CIDR must be a valid network address"
	CIDR string `json:"cidr"`
	// Protocols are ORed together; omission matches all protocols and ports.
	// +optional
	// +listType=atomic
	Protocols []ProtocolMatch `json:"protocols,omitempty"`
}

// ProtocolMatch selects exactly one transport protocol.
// +kubebuilder:validation:XValidation:rule="has(self.tcp) ? !has(self.udp) && !has(self.sctp) : has(self.udp) != has(self.sctp)",message="exactly one of tcp, udp, or sctp must be set"
type ProtocolMatch struct {
	// TCP selects TCP traffic.
	// +optional
	TCP *PortProtocol `json:"tcp,omitempty"`
	// UDP selects UDP traffic.
	// +optional
	UDP *PortProtocol `json:"udp,omitempty"`
	// SCTP selects SCTP traffic.
	// +optional
	SCTP *PortProtocol `json:"sctp,omitempty"`
}

// PortProtocol optionally restricts both remote and local ports (AND semantics).
type PortProtocol struct {
	// DestinationPort matches the remote port.
	// +optional
	DestinationPort *Port `json:"destinationPort,omitempty"`
	// SourcePort matches the local pod port.
	// +optional
	SourcePort *Port `json:"sourcePort,omitempty"`
}

// Port selects exactly one port number or inclusive range.
// +kubebuilder:validation:XValidation:rule="has(self.number) != has(self.range)",message="exactly one of number or range must be set"
type Port struct {
	// Number selects one port.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Number *int32 `json:"number,omitempty"`
	// Range selects an inclusive range of ports.
	// +optional
	Range *PortRange `json:"range,omitempty"`
}

// PortRange is an inclusive range of transport ports.
// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end must be greater than or equal to start"
type PortRange struct {
	// Start is the first port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`
	// End is the last port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End int32 `json:"end"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressTrafficClassList is a list of EgressTrafficClasses.
type EgressTrafficClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressTrafficClass `json:"items"`
}
