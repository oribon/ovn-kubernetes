package egress_services

import (
	"errors"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	discovery "k8s.io/api/discovery/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// onEndpointSliceAdd queues a sync for the relevant Service for a sync
func (c *Controller) onEndpointSliceAdd(obj interface{}) {
	endpointSlice := obj.(*discovery.EndpointSlice)
	if endpointSlice == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onEndpointSliceAdd()"))
		return
	}
	c.queueServiceForEndpointSlice(endpointSlice)
}

// onEndpointSliceUpdate queues a sync for the relevant Service for a sync
func (c *Controller) onEndpointSliceUpdate(prevObj, obj interface{}) {
	prevEndpointSlice := prevObj.(*discovery.EndpointSlice)
	endpointSlice := obj.(*discovery.EndpointSlice)

	// don't process resync or objects that are marked for deletion
	if prevEndpointSlice.ResourceVersion == endpointSlice.ResourceVersion ||
		!endpointSlice.GetDeletionTimestamp().IsZero() {
		return
	}
	c.queueServiceForEndpointSlice(endpointSlice)
}

// onEndpointSliceDelete queues a sync for the relevant Service for a sync if the
// EndpointSlice resource version does not match the expected version in the
// endpointSliceTracker.
func (c *Controller) onEndpointSliceDelete(obj interface{}) {
	endpointSlice, ok := obj.(*discovery.EndpointSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		endpointSlice, ok = tombstone.Obj.(*discovery.EndpointSlice)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a EndpointSlice: %#v", obj))
			return
		}
	}

	if endpointSlice != nil {
		c.queueServiceForEndpointSlice(endpointSlice)
	}
}

func (c *Controller) queueServiceForEndpointSlice(endpointSlice *discovery.EndpointSlice) {
	key, err := services.ServiceControllerKey(endpointSlice)
	if err != nil {
		// Do not log endpointsSlices missing service labels as errors.
		// Once the service label is eventually added, we will get this event
		// and re-process.
		if errors.Is(err, services.NoServiceLabelError) {
			klog.V(5).Infof(err.Error())
		} else {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for EndpointSlice %+v: %v", endpointSlice, err))
		}
		return
	}
	c.Lock()
	defer c.Unlock()
	_, found := c.services[key]
	if !found { // we queue a service only if it's in the cache
		return
	}

	c.servicesQueue.Add(key)
}
