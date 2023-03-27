//go:build !ignore_autogenerated
// +build !ignore_autogenerated

/*


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
func (in *EgressService) DeepCopyInto(out *EgressService) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressService.
func (in *EgressService) DeepCopy() *EgressService {
	if in == nil {
		return nil
	}
	out := new(EgressService)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EgressService) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressServiceList) DeepCopyInto(out *EgressServiceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]EgressService, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressServiceList.
func (in *EgressServiceList) DeepCopy() *EgressServiceList {
	if in == nil {
		return nil
	}
	out := new(EgressServiceList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *EgressServiceList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressServiceSNAT) DeepCopyInto(out *EgressServiceSNAT) {
	*out = *in
	in.NodeSelector.DeepCopyInto(&out.NodeSelector)
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressServiceSNAT.
func (in *EgressServiceSNAT) DeepCopy() *EgressServiceSNAT {
	if in == nil {
		return nil
	}
	out := new(EgressServiceSNAT)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressServiceSpec) DeepCopyInto(out *EgressServiceSpec) {
	*out = *in
	if in.SNAT != nil {
		in, out := &in.SNAT, &out.SNAT
		*out = new(EgressServiceSNAT)
		(*in).DeepCopyInto(*out)
	}
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressServiceSpec.
func (in *EgressServiceSpec) DeepCopy() *EgressServiceSpec {
	if in == nil {
		return nil
	}
	out := new(EgressServiceSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *EgressServiceStatus) DeepCopyInto(out *EgressServiceStatus) {
	*out = *in
	return
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new EgressServiceStatus.
func (in *EgressServiceStatus) DeepCopy() *EgressServiceStatus {
	if in == nil {
		return nil
	}
	out := new(EgressServiceStatus)
	in.DeepCopyInto(out)
	return out
}
