package servicefwmark

import (
	"fmt"
	"sync"
	"time"

	fwmarklisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/servicefwmark/v1/apis/listers/servicefwmark/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

type Controller struct {
	client kubernetes.Interface
	stopCh <-chan struct{}

	ServiceFWMarkLister fwmarklisters.ServiceFWMarkLister
	ServiceFWMarkSynced cache.InformerSynced
	ServiceFWMarkQueue  workqueue.RateLimitingInterface

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced
}

func NewController(client kubernetes.Interface, stopCh <-chan struct{}) *Controller {
	c := &Controller{
		client: client,
		stopCh: stopCh,
	}

	return c
}

func (c *Controller) Run(threadiness int) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting ServiceFWMark Controller")

	if !cache.WaitForNamedCacheSync("servicefwmarks", c.stopCh, c.ServiceFWMarkSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing ServiceFWMarks")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to repair ServiceFWMark entries: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runServiceFWMarkWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// wait until we're told to stop
	<-c.stopCh

	klog.Infof("Shutting down ServiceFWMark controller")
	c.ServiceFWMarkQueue.ShutDown()

	wg.Wait()
}

func (c *Controller) repair() error {
	return nil
}

func (c *Controller) runServiceFWMarkWorker(wg *sync.WaitGroup) {
	for c.processNextServiceFWMarkWorkItem(wg) {
	}
}

func (c *Controller) processNextServiceFWMarkWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.ServiceFWMarkQueue.Get()
	if quit {
		return false
	}

	defer c.ServiceFWMarkQueue.Done(key)

	err := c.syncServiceFWMark(key.(string))
	if err == nil {
		c.ServiceFWMarkQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.ServiceFWMarkQueue.NumRequeues(key) < 10 {
		c.ServiceFWMarkQueue.AddRateLimited(key)
		return true
	}

	c.ServiceFWMarkQueue.Forget(key)
	return true
}

func (c *Controller) syncServiceFWMark(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for ServiceFWMark %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing ServiceFWMark %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	svcfwmark, err := c.ServiceFWMarkLister.ServiceFWMarks(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if svcfwmark == nil {
		// delete stuff
	}

	return nil
}
