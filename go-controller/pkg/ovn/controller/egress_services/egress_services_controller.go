package egress_services

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	utilnet "k8s.io/utils/net"
)

const (
	maxRetries = 10
)

type Controller struct {
	client   kubernetes.Interface
	nbClient libovsdbclient.Client
	stopCh   <-chan struct{}
	sync.Mutex

	initClusterEgressPolicies   func(client libovsdbclient.Client) error
	createNoRerouteNodePolicies func(client libovsdbclient.Client, node *corev1.Node) error
	deleteNoRerouteNodePolicies func(client libovsdbclient.Client, node string) error

	services            map[string]*svcState
	nodes               map[string]*nodeState
	unallocatedServices map[string]labels.Selector

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
	reachable   bool
	draining    bool
}

func NewController(
	client kubernetes.Interface,
	nbClient libovsdbclient.Client,
	initClusterEgressPolicies func(libovsdbclient.Client) error,
	createNoRerouteNodePolicies func(client libovsdbclient.Client, node *corev1.Node) error,
	deleteNoRerouteNodePolicies func(client libovsdbclient.Client, node string) error,
	stopCh <-chan struct{},
	serviceInformer coreinformers.ServiceInformer,
	endpointSliceInformer discoveryinformers.EndpointSliceInformer,
	nodeInformer coreinformers.NodeInformer) *Controller {
	klog.Info("Setting up event handlers for Egress Services")
	c := &Controller{
		client:                      client,
		nbClient:                    nbClient,
		initClusterEgressPolicies:   initClusterEgressPolicies,
		createNoRerouteNodePolicies: createNoRerouteNodePolicies,
		deleteNoRerouteNodePolicies: deleteNoRerouteNodePolicies,
		stopCh:                      stopCh,
		services:                    map[string]*svcState{},
		nodes:                       map[string]*nodeState{},
		unallocatedServices:         map[string]labels.Selector{},
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
}

func (c *Controller) Run(threadiness int) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Egress Services Controller")

	if !cache.WaitForNamedCacheSync("egressservices", c.stopCh, c.servicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressserviceendpointslices", c.stopCh, c.endpointSlicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservicenodes", c.stopCh, c.nodesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to repair Egress Services entries: %v", err)
	}

	err = c.initClusterEgressPolicies(c.nbClient)
	if err != nil {
		klog.Errorf("Failed to init Egress Services cluster policies: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runServiceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNodeWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	go c.checkNodesReachability()

	// wait until we're told to stop
	<-c.stopCh

	klog.Infof("Shutting down Egress Services controller")
	c.servicesQueue.ShutDown()
	c.nodesQueue.ShutDown()

	wg.Wait()
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It deletes all logical router policies from OVN that belong to services which are no longer
// egress services, and the policies of endpoints that do not belong to an egress service.
// In addition, it removes the egress service labels of deleted services from the nodes.
func (c *Controller) repair() error {
	c.Lock()
	defer c.Unlock()

	svcKeyToAllV4Endpoints := map[string]sets.String{}
	svcKeyToAllV6Endpoints := map[string]sets.String{}
	svcKeyToConfiguredV4Endpoints := map[string][]string{}
	svcKeyToConfiguredV6Endpoints := map[string][]string{}
	services, _ := c.serviceLister.List(labels.Everything())

	for _, svc := range services {
		if util.HasEgressSVCAnnotation(svc) && util.HasEgressSVCHostAnnotation(svc) &&
			util.ServiceTypeHasLoadBalancer(svc) && len(svc.Status.LoadBalancer.Ingress) > 0 {
			var err error
			key, _ := cache.MetaNamespaceKeyFunc(svc)
			conf, err := util.ParseEgressSVCAnnotation(svc)
			if err != nil && !util.IsAnnotationNotSetError(err) {
				klog.Errorf("can't parse %s egress service configuration, err: %v", key, err)
				continue
			}
			svcHost, _ := util.GetEgressSVCHost(svc)
			nodeState, ok := c.nodes[svcHost]
			if !ok {
				nodeState, err = c.nodeStateFor(svcHost)
				if err != nil {
					klog.Errorf("can't fetch egress service %s node %s state, err: %v", key, svcHost, err)
					continue
				}
			}

			v4, v6, epNodes, err := c.allEndpointsFor(svc)
			if err != nil {
				klog.Errorf("can't fetch all endpoints for egress service %s, err: %v", key, err)
				continue
			}

			totalEps := len(v4) + len(v6)
			if totalEps == 0 {
				klog.Errorf("Egress service %s has no endpoints", key)
				continue
			}

			hostValid := true
			if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal {
				hostValid = false
				for _, node := range epNodes {
					if node == svcHost {
						hostValid = true
						break
					}
				}
			}

			if !hostValid {
				klog.Errorf("Node %s no longer valid for egress service %s", svcHost, key)
				continue
			}

			svcKeyToAllV4Endpoints[key] = v4
			svcKeyToAllV6Endpoints[key] = v6
			svcKeyToConfiguredV4Endpoints[key] = []string{}
			svcKeyToConfiguredV6Endpoints[key] = []string{}

			selector, _ := metav1.LabelSelectorAsSelector(&conf.NodeSelector)
			svcState := &svcState{node: svcHost, selector: selector, v4Endpoints: sets.NewString(), v6Endpoints: sets.NewString(), stale: false}
			nodeState.allocations[key] = svcState
			c.nodes[svcHost] = nodeState
			c.services[key] = svcState
		}
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		if item.Priority != ovntypes.EgressSVCReroutePriority {
			return false
		}

		svcKey, found := item.ExternalIDs["EgressSVC"]
		if !found {
			klog.Infof("Egress service repair will delete lrp because it uses the egress service priority but does not belong to one: %v", item)
			return true
		}

		svc, found := c.services[svcKey]
		if !found {
			klog.Infof("Egress service repair will delete %s because it is no longer a valid egress service: %v", svcKey, item)
			return true
		}

		v4Eps := svcKeyToAllV4Endpoints[svcKey]
		v6Eps := svcKeyToAllV6Endpoints[svcKey]

		// we extract the IP from the match: "ip4.src == IP" / "ip6.src == IP"
		splitMatch := strings.Split(item.Match, " ")
		logicalIP := splitMatch[len(splitMatch)-1]
		if !v4Eps.Has(logicalIP) && !v6Eps.Has(logicalIP) {
			klog.Infof("Egress service repair will delete %s because it is no longer an endpoint of the service %s: %v", logicalIP, svcKey, item)
			return true
		}

		if len(item.Nexthops) != 1 {
			klog.Infof("Egress service repair will delete %s because it is does not have only one nexthop for service %s: %v", logicalIP, svcKey, item)
			return true
		}

		node := c.nodes[svc.node]
		if item.Nexthops[0] != node.v4MgmtIP.String() && item.Nexthops[0] != node.v6MgmtIP.String() {
			klog.Infof("Egress service repair will delete %s because it is uses a stale nexthop for service %s: %v", logicalIP, svcKey, item)
			return true
		}

		if utilnet.IsIPv4String(logicalIP) {
			svcKeyToConfiguredV4Endpoints[svcKey] = append(svcKeyToConfiguredV4Endpoints[svcKey], logicalIP)
			return false
		}

		svcKeyToConfiguredV6Endpoints[svcKey] = append(svcKeyToConfiguredV6Endpoints[svcKey], logicalIP)
		return false
	}

	err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(c.nbClient, ovntypes.OVNClusterRouter, p)
	if err != nil {
		return fmt.Errorf("error deleting stale logical router policies from router %s: %v", ovntypes.OVNClusterRouter, err)
	}

	// update caches after transaction completed
	for key, v4ToAdd := range svcKeyToConfiguredV4Endpoints {
		c.services[key].v4Endpoints.Insert(v4ToAdd...)
	}

	for key, v6ToAdd := range svcKeyToConfiguredV6Endpoints {
		c.services[key].v6Endpoints.Insert(v6ToAdd...)
	}

	nodes, _ := c.nodeLister.List(labels.Everything())
	svcLabelToNode := map[string]string{}
	for key, state := range c.services {
		namespace, name, _ := cache.SplitMetaNamespaceKey(key)
		svcLabelToNode[c.nodeLabelForService(namespace, name)] = state.node
	}

	for _, node := range nodes {
		labelsToRemove := map[string]any{}
		for labelKey := range node.Labels {
			if strings.HasPrefix(labelKey, util.EgressSVCLabelPrefix) && svcLabelToNode[labelKey] != node.Name {
				labelsToRemove[labelKey] = nil // Patching with a nil value results in the delete of the key
			}
		}
		err := c.patchNodeLabels(node.Name, labelsToRemove)
		if err != nil {
			klog.Errorf("Failed to remove stale labels %v from node %s, err: %v", labelsToRemove, node.Name, err)
			continue
		}
	}

	return nil
}
