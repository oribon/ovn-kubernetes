package egress_services

import (
	"fmt"
	"net"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	discoveryinformers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	maxRetries = 10
)

type Controller struct {
	client   kubernetes.Interface
	nbClient libovsdbclient.Client
	sync.Mutex

	/* revisit, reuse types.DefaultNoRereoutePriority functions for egressip
	v4JoinSubnet     string
	v6JoinSubnet     string
	v4ClusterSubnets []*net.IPNet //getClusterSubnets in eip controller / egressfw controller
	v6ClusterSubnets []*net.IPNet*/

	services map[string]*svcState
	nodes    map[string]*nodeState

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced
	servicesQueue  workqueue.RateLimitingInterface

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	nodeLister  corelisters.NodeLister
	nodesSynced cache.InformerSynced
	nodesQueue  workqueue.RateLimitingInterface
}

type svcState struct {
	node        string
	selector    labels.Selector
	v4Endpoints sets.String
	v6Endpoints sets.String
	stale       bool
}

type nodeState struct {
	name        string
	labels      map[string]string
	v4MgmtIP    net.IP
	v6MgmtIP    net.IP
	allocations map[string]*svcState
	ready       bool
	reachable   bool
	draining    bool
}

func NewController(
	client kubernetes.Interface,
	nbClient libovsdbclient.Client,
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer) *Controller {
	klog.Info("Setting up event handlers for Egress Services")
	c := &Controller{
		client:   client,
		nbClient: nbClient,
		services: map[string]*svcState{},
		nodes:    map[string]*nodeState{},
	}

	c.serviceLister = serviceInformer.Lister()
	c.servicesSynced = serviceInformer.Informer().HasSynced
	c.servicesQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressservices",
	)
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	})

	c.endpointSliceLister = endpointSliceInformer.Lister()
	c.endpointSlicesSynced = endpointSliceInformer.Informer().HasSynced
	endpointSliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	})

	c.nodeLister = nodeInformer.Lister()
	c.nodesSynced = nodeInformer.Informer().HasSynced
	c.nodesQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressservicenodes",
	)
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNodeAdd,
		UpdateFunc: c.onNodeUpdate,
		DeleteFunc: c.onNodeDelete,
	})

	return c
	// update clustersubnets?
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Egress Services Controller")

	if !cache.WaitForNamedCacheSync("egressservices", stopCh, c.servicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressserviceendpointslices", stopCh, c.endpointSlicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservicenodes", stopCh, c.nodesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to delete stale Egress Services entries: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runServiceWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNodeWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	// wait until we're told to stop
	<-stopCh

	klog.Infof("Shutting down Egress Services controller")
	c.servicesQueue.ShutDown()
	c.nodesQueue.ShutDown()

	wg.Wait()
}

func (c *Controller) repair() error {
	// remove all logical router policies
	// remove all host-labels from nodes
	// remove all host-annotations from services
	return nil
}
