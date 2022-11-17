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
// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	servicefwmarkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/servicefwmark/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeServiceFWMarks implements ServiceFWMarkInterface
type FakeServiceFWMarks struct {
	Fake *FakeK8sV1
	ns   string
}

var servicefwmarksResource = schema.GroupVersionResource{Group: "k8s.ovn.org", Version: "v1", Resource: "servicefwmarks"}

var servicefwmarksKind = schema.GroupVersionKind{Group: "k8s.ovn.org", Version: "v1", Kind: "ServiceFWMark"}

// Get takes name of the serviceFWMark, and returns the corresponding serviceFWMark object, and an error if there is any.
func (c *FakeServiceFWMarks) Get(ctx context.Context, name string, options v1.GetOptions) (result *servicefwmarkv1.ServiceFWMark, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(servicefwmarksResource, c.ns, name), &servicefwmarkv1.ServiceFWMark{})

	if obj == nil {
		return nil, err
	}
	return obj.(*servicefwmarkv1.ServiceFWMark), err
}

// List takes label and field selectors, and returns the list of ServiceFWMarks that match those selectors.
func (c *FakeServiceFWMarks) List(ctx context.Context, opts v1.ListOptions) (result *servicefwmarkv1.ServiceFWMarkList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(servicefwmarksResource, servicefwmarksKind, c.ns, opts), &servicefwmarkv1.ServiceFWMarkList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &servicefwmarkv1.ServiceFWMarkList{ListMeta: obj.(*servicefwmarkv1.ServiceFWMarkList).ListMeta}
	for _, item := range obj.(*servicefwmarkv1.ServiceFWMarkList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested serviceFWMarks.
func (c *FakeServiceFWMarks) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(servicefwmarksResource, c.ns, opts))

}

// Create takes the representation of a serviceFWMark and creates it.  Returns the server's representation of the serviceFWMark, and an error, if there is any.
func (c *FakeServiceFWMarks) Create(ctx context.Context, serviceFWMark *servicefwmarkv1.ServiceFWMark, opts v1.CreateOptions) (result *servicefwmarkv1.ServiceFWMark, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(servicefwmarksResource, c.ns, serviceFWMark), &servicefwmarkv1.ServiceFWMark{})

	if obj == nil {
		return nil, err
	}
	return obj.(*servicefwmarkv1.ServiceFWMark), err
}

// Update takes the representation of a serviceFWMark and updates it. Returns the server's representation of the serviceFWMark, and an error, if there is any.
func (c *FakeServiceFWMarks) Update(ctx context.Context, serviceFWMark *servicefwmarkv1.ServiceFWMark, opts v1.UpdateOptions) (result *servicefwmarkv1.ServiceFWMark, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(servicefwmarksResource, c.ns, serviceFWMark), &servicefwmarkv1.ServiceFWMark{})

	if obj == nil {
		return nil, err
	}
	return obj.(*servicefwmarkv1.ServiceFWMark), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeServiceFWMarks) UpdateStatus(ctx context.Context, serviceFWMark *servicefwmarkv1.ServiceFWMark, opts v1.UpdateOptions) (*servicefwmarkv1.ServiceFWMark, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(servicefwmarksResource, "status", c.ns, serviceFWMark), &servicefwmarkv1.ServiceFWMark{})

	if obj == nil {
		return nil, err
	}
	return obj.(*servicefwmarkv1.ServiceFWMark), err
}

// Delete takes name of the serviceFWMark and deletes it. Returns an error if one occurs.
func (c *FakeServiceFWMarks) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteAction(servicefwmarksResource, c.ns, name), &servicefwmarkv1.ServiceFWMark{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeServiceFWMarks) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(servicefwmarksResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &servicefwmarkv1.ServiceFWMarkList{})
	return err
}

// Patch applies the patch and returns the patched serviceFWMark.
func (c *FakeServiceFWMarks) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *servicefwmarkv1.ServiceFWMark, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(servicefwmarksResource, c.ns, name, pt, data, subresources...), &servicefwmarkv1.ServiceFWMark{})

	if obj == nil {
		return nil, err
	}
	return obj.(*servicefwmarkv1.ServiceFWMark), err
}
