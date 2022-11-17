/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=servicefwmarks
// +kubebuilder::singular=servicefwmark
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// ServiceFWMark is a CRD that allows the user to define a fwmark value
// for all the traffic of a given service, which means that all traffic
// originating from endpoints of the service and all traffic heading to it
// will be marked with the given fwmark.
type ServiceFWMark struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceFWMarkSpec   `json:"spec,omitempty"`
	Status ServiceFWMarkStatus `json:"status,omitempty"`
}

// ServiceFWMarkSpec defines the desired state of ServiceFWMark
type ServiceFWMarkSpec struct {
	// The fwmark value to set for the service's traffic.
	// +kubebuilder:validation:Maximum:=20000
	// +kubebuilder:validation:Minimum:=10000
	FWMark int `json:"fwmark"`
}

// ServiceFWMarkStatus defines the observed state of ServiceFWMark
type ServiceFWMarkStatus struct {
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=servicefwmarks
// +kubebuilder::singular=servicefwmark
// ServiceFWMarkList contains a list of ServiceFWMarks
type ServiceFWMarkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceFWMark `json:"items"`
}
