//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*
Copyright The Kubernetes Authors.

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

// Code generated by deepcopy-gen. DO NOT EDIT.

package v1

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressQoS) DeepCopyInto(out *EgressQoS) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressQoS.
func (in *EgressQoS) DeepCopy() *EgressQoS {
	if in == nil {
		return nil
	}
	out := new(EgressQoS)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EgressQoS) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressQoSList) DeepCopyInto(out *EgressQoSList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]EgressQoS, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressQoSList.
func (in *EgressQoSList) DeepCopy() *EgressQoSList {
	if in == nil {
		return nil
	}
	out := new(EgressQoSList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EgressQoSList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressQoSRule) DeepCopyInto(out *EgressQoSRule) {
	*out = *in
	if in.DstCIDR != nil {
		in, out := &in.DstCIDR, &out.DstCIDR
		*out = new(string)
		**out = **in
	}
	in.PodSelector.DeepCopyInto(&out.PodSelector)
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressQoSRule.
func (in *EgressQoSRule) DeepCopy() *EgressQoSRule {
	if in == nil {
		return nil
	}
	out := new(EgressQoSRule)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressQoSSpec) DeepCopyInto(out *EgressQoSSpec) {
	*out = *in
	if in.Egress != nil {
		in, out := &in.Egress, &out.Egress
		*out = make([]EgressQoSRule, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressQoSSpec.
func (in *EgressQoSSpec) DeepCopy() *EgressQoSSpec {
	if in == nil {
		return nil
	}
	out := new(EgressQoSSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressQoSStatus) DeepCopyInto(out *EgressQoSStatus) {
	*out = *in
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressQoSStatus.
func (in *EgressQoSStatus) DeepCopy() *EgressQoSStatus {
	if in == nil {
		return nil
	}
	out := new(EgressQoSStatus)
	in.DeepCopyInto(out)
	return out
}
