package egressservice

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressserviceinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/informers/externalversions/egressservice/v1"
	egressservicelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/listers/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	Chain       = "OVN-KUBE-EGRESS-SVC" // called from nat-POSTROUTING and mangle-PREROUTING
	firstFWMark = 5000
	maxFWMark   = 7000
)

type Controller struct {
	stopCh <-chan struct{}
	sync.Mutex
	returnMark string // packets coming with this mark should not be modified
	thisNode   string // name of the node we're running on
	// TODO: make this smarter, currently the fwmarks aren't being reused
	// if a network "frees" one
	availableFWMark int32 // available fwmark for a new fwmark-network mapping

	egressServiceLister egressservicelisters.EgressServiceLister
	egressServiceSynced cache.InformerSynced
	egressServiceQueue  workqueue.RateLimitingInterface

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	services      map[string]*svcState // svc key -> state
	routingTables map[string]int32     // routing table name -> fwmark
}

type svcState struct {
	v4LB      string      // IPv4 ingress of the service
	v4Eps     sets.String // v4 endpoints that have an SNAT rule configured
	v6LB      string      // IPv6 ingress of the service
	v6Eps     sets.String // v6 endpoints that have an SNAT rule configured
	fwmark    int32       // FWMark corresponding to a routing table
	markedEps sets.String // All endpoints that have a FWMark rule configured

	stale bool
}

func NewController(stopCh <-chan struct{}, returnMark, thisNode string,
	esInformer egressserviceinformer.EgressServiceInformer,
	serviceInformer cache.SharedIndexInformer,
	endpointSliceInformer cache.SharedIndexInformer) *Controller {
	klog.Info("Setting up event handlers for Egress Services")

	c := &Controller{
		stopCh:          stopCh,
		returnMark:      returnMark,
		thisNode:        thisNode,
		services:        map[string]*svcState{},
		routingTables:   map[string]int32{},
		availableFWMark: firstFWMark,
	}

	c.egressServiceLister = esInformer.Lister()
	c.egressServiceSynced = esInformer.Informer().HasSynced
	c.egressServiceQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressservices",
	)
	esInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEgressServiceAdd,
		UpdateFunc: c.onEgressServiceUpdate,
		DeleteFunc: c.onEgressServiceDelete,
	}))

	c.serviceLister = corelisters.NewServiceLister(serviceInformer.GetIndexer())
	c.servicesSynced = serviceInformer.HasSynced
	serviceInformer.AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	}))

	c.endpointSliceLister = discoverylisters.NewEndpointSliceLister(endpointSliceInformer.GetIndexer())
	c.endpointSlicesSynced = endpointSliceInformer.HasSynced
	endpointSliceInformer.AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	}))

	return c
}

// onEgressServiceAdd queues the EgressService for processing.
func (c *Controller) onEgressServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.egressServiceQueue.Add(key)
}

// onEgressServiceUpdate queues the EgressService for processing.
func (c *Controller) onEgressServiceUpdate(oldObj, newObj interface{}) {
	oldEQ := oldObj.(*egressserviceapi.EgressService)
	newEQ := newObj.(*egressserviceapi.EgressService)

	if oldEQ.ResourceVersion == newEQ.ResourceVersion ||
		!newEQ.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.egressServiceQueue.Add(key)
	}
}

// onEgressServiceDelete queues the EgressService for processing.
func (c *Controller) onEgressServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.egressServiceQueue.Add(key)
}

func (c *Controller) Run(threadiness int) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Egress Services Controller")

	if !cache.WaitForNamedCacheSync("egressservices", c.stopCh, c.egressServiceSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservices_services", c.stopCh, c.servicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressservices_endpointslices", c.stopCh, c.endpointSlicesSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing Egress Services")
	err := c.repair()
	if err != nil {
		klog.Errorf("Failed to repair Egress Services entries: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runEgressServiceWorker(wg)
			}, time.Second, c.stopCh)
		}()
	}

	// wait until we're told to stop
	<-c.stopCh

	klog.Infof("Shutting down Egress Services controller")
	c.egressServiceQueue.ShutDown()

	wg.Wait()
}

// Cleanups the tables/chain so the controller starts from a clean state reflecting its empty cache.
// TODO: only clean the stale rules and update the cache accordingly.
func (c *Controller) repair() error {
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return err
		}

		err = ipt.ClearChain("nat", Chain)
		if err != nil {
			return err
		}
		err = ipt.ClearChain("mangle", Chain)
		if err != nil {
			return err
		}

		err = nodeipt.AddRules(c.defaultReturnRules(proto), true)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) runEgressServiceWorker(wg *sync.WaitGroup) {
	for c.processNextEgressServiceWorkItem(wg) {
	}
}

func (c *Controller) processNextEgressServiceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.egressServiceQueue.Get()
	if quit {
		return false
	}

	defer c.egressServiceQueue.Done(key)

	err := c.syncEgressService(key.(string))
	if err == nil {
		c.egressServiceQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.egressServiceQueue.NumRequeues(key) < 10 {
		c.egressServiceQueue.AddRateLimited(key)
		return true
	}

	c.egressServiceQueue.Forget(key)
	return true
}

func (c *Controller) syncEgressService(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for EgressService %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressService %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	es, err := c.egressServiceLister.EgressServices(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	cachedState := c.services[key]
	if svc == nil && cachedState == nil {
		return nil
	}

	if svc == nil && cachedState != nil {
		return c.clearServiceRules(key, cachedState)
	}

	if es == nil && cachedState == nil {
		return nil
	}

	if es == nil && cachedState != nil {
		return c.clearServiceRules(key, cachedState)
	}

	if cachedState != nil && cachedState.stale {
		// The service is marked stale because something failed when trying to delete it.
		// We try to delete it again before doing anything else.
		return c.clearServiceRules(key, cachedState)
	}

	// At this point both the svc and es are not nil
	shouldConfigure := c.shouldConfigureEgressSVC(svc, es.Status.Host)
	if cachedState == nil && !shouldConfigure {
		return nil
	}

	if cachedState != nil && !shouldConfigure {
		return c.clearServiceRules(key, cachedState)
	}

	lbsChanged := false
	v4LB, v6LB := "", ""
	for _, ip := range svc.Status.LoadBalancer.Ingress {
		if utilnet.IsIPv4String(ip.IP) {
			v4LB = ip.IP
			continue
		}
		v6LB = ip.IP
	}

	if cachedState != nil {
		lbsChanged = v4LB != cachedState.v4LB || v6LB != cachedState.v6LB
	}

	if lbsChanged {
		err := c.clearServiceSNATRules(key, cachedState)
		if err != nil {
			return err
		}
	}

	if cachedState == nil {
		cachedState = &svcState{
			v4Eps:     sets.NewString(),
			v6Eps:     sets.NewString(),
			markedEps: sets.NewString(),
			stale:     false,
		}
		c.services[key] = cachedState
	}
	cachedState.v4LB = v4LB
	cachedState.v6LB = v6LB

	v4Eps, v6Eps, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	v4ToAdd := v4Eps.Difference(cachedState.v4Eps)
	v6ToAdd := v6Eps.Difference(cachedState.v6Eps)
	v4ToDelete := cachedState.v4Eps.Difference(v4Eps)
	v6ToDelete := cachedState.v6Eps.Difference(v6Eps)

	if cachedState.v4LB != "" {
		for ep := range v4ToAdd {
			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)}, true)
			if err != nil {
				return err
			}
			cachedState.v4Eps.Insert(ep)
		}

		for ep := range v4ToDelete {
			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v4Eps.Delete(ep)
		}
	}

	if cachedState.v6LB != "" {
		for ep := range v6ToAdd {
			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)}, true)
			if err != nil {
				return err
			}

			cachedState.v6Eps.Insert(ep)
		}

		for ep := range v6ToDelete {
			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v6Eps.Delete(ep)
		}
	}

	// At this point we finished handling the SNAT rules
	fwmark, err := c.fwmarkForTable(es.Spec.Network)
	if err != nil {
		return err
	}

	if fwmark != cachedState.fwmark {
		err := c.clearServiceFWMarkRules(key, cachedState)
		if err != nil {
			return err
		}
	}
	cachedState.fwmark = fwmark

	if cachedState.fwmark == 0 {
		return nil
	}

	allEps := v4Eps.Union(v6Eps)

	for _, cip := range svc.Spec.ClusterIPs {
		ip := utilnet.ParseIPSloppy(cip).String()
		allEps.Insert(ip)
	}

	marksToAdd := allEps.Difference(cachedState.markedEps)
	marksToDelete := cachedState.markedEps.Difference(allEps)

	for ip := range marksToAdd {
		err := nodeipt.AddRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, ip)}, true)
		if err != nil {
			return err
		}
		cachedState.markedEps.Insert(ip)
	}

	for ip := range marksToDelete {
		err := nodeipt.DelRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, ip)})
		if err != nil {
			return err
		}
		cachedState.markedEps.Delete(ip)
	}

	return nil
}

// Returns all of the non-host endpoints for the given service grouped by IPv4/IPv6.
func (c *Controller) allEndpointsFor(svc *corev1.Service) (sets.String, sets.String, error) {
	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discoveryv1.LabelServiceName: svc.Name,
	}).AsSelectorPreValidated()

	endpointSlices, err := c.endpointSliceLister.EndpointSlices(svc.Namespace).List(esLabelSelector)
	if err != nil {
		return nil, nil, err
	}

	v4Endpoints := sets.NewString()
	v6Endpoints := sets.NewString()

	for _, eps := range endpointSlices {
		if eps.AddressType == discoveryv1.AddressTypeFQDN {
			continue
		}

		epsToInsert := v4Endpoints
		if eps.AddressType == discoveryv1.AddressTypeIPv6 {
			epsToInsert = v6Endpoints
		}

		for _, ep := range eps.Endpoints {
			for _, ip := range ep.Addresses {
				ipStr := utilnet.ParseIPSloppy(ip).String()
				if !services.IsHostEndpoint(ipStr) {
					epsToInsert.Insert(ipStr)
				}
			}
		}
	}

	return v4Endpoints, v6Endpoints, nil
}

// Clears all of the SNAT rules of the service.
func (c *Controller) clearServiceSNATRules(key string, state *svcState) error {
	for ip := range state.v4Eps {
		err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, state.v4LB, ip)})
		if err != nil {
			return err
		}

		state.v4Eps.Delete(ip)
	}

	for ip := range state.v6Eps {
		err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, state.v6LB, ip)})
		if err != nil {
			return err
		}

		state.v6Eps.Delete(ip)
	}

	state.v4LB = ""
	state.v6LB = ""

	return nil
}

// Clears all of the FWMark rules of the service.
func (c *Controller) clearServiceFWMarkRules(key string, state *svcState) error {
	for ip := range state.markedEps {
		err := nodeipt.DelRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, state.fwmark, ip)})
		if err != nil {
			return err
		}

		state.markedEps.Delete(ip)
	}

	return nil
}

// Clears all of the iptables rules that relate to the service and removes it from the cache.
func (c *Controller) clearServiceRules(key string, state *svcState) error {
	state.stale = true

	err := c.clearServiceSNATRules(key, state)
	if err != nil {
		return err
	}

	err = c.clearServiceFWMarkRules(key, state)
	if err != nil {
		return err
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)

	return nil
}

// Returns true if the controller should configure the given service as an "Egress Service"
func (c *Controller) shouldConfigureEgressSVC(svc *corev1.Service, svcHost string) bool {
	return svcHost == c.thisNode &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0
}

// Returns the fwmark value corresponding to the given routing table.
// If the table does not have an allocated fwmark value this takes care of allocating
// one and creating the necessary ip rules.
func (c *Controller) fwmarkForTable(table string) (int32, error) {
	if table == "" {
		return 0, nil
	}

	fwmark, found := c.routingTables[table]
	if found {
		return fwmark, nil
	}

	fwmark = c.availableFWMark
	if fwmark > maxFWMark {
		return 0, fmt.Errorf("could not allocate fwmark for table: %s", table)
	}

	if config.IPv4Mode {
		if err := createIPRule("-4", fwmark, table); err != nil {
			return 0, err
		}
	}

	if config.IPv6Mode {
		if err := createIPRule("-6", fwmark, table); err != nil {
			return 0, err
		}
	}

	c.routingTables[table] = fwmark
	c.availableFWMark++

	return fwmark, nil
}

// Create the ip rule to use the specified routing table for the fwmark.
// The rule is created with the same priority as the fwmark, and if a rule
// exists with that priority it deletes it beforehand.
func createIPRule(family string, fwmark int32, table string) error {
	mark := fmt.Sprintf("%d", fwmark)

	stdout, stderr, err := util.RunIP(family, "rule", "del", "prio", mark)
	if err != nil && !strings.Contains(stderr, "No such file or directory") {
		return fmt.Errorf("could not delete rule with priority %d - stdout: %s, stderr: %s, err: %v", fwmark, stdout, stderr, err)
	}

	stdout, stderr, err = util.RunIP(family, "rule", "add", "prio", mark, "fwmark", mark, "table", table)
	if err != nil {
		return fmt.Errorf("could not add rule for table %s - stdout: %s, stderr: %s, err: %v", table, stdout, stderr, err)
	}

	return nil
}

// Returns the FWMark rule that should be created for the given fwmark/endpoint
func fwmarkIPTRuleFor(comment string, fwmark int32, ip string) nodeipt.Rule {
	return nodeipt.Rule{
		Table: "mangle",
		Chain: Chain,
		Args: []string{
			"-s", ip,
			"-m", "comment", "--comment", comment,
			"-j", "MARK",
			"--set-mark", fmt.Sprintf("%d", fwmark),
		},
		Protocol: getIPTablesProtocol(ip),
	}
}

// Returns the SNAT rule that should be created for the given lb/endpoint
func snatIPTRuleFor(comment string, lb, ip string) nodeipt.Rule {
	return nodeipt.Rule{
		Table: "nat",
		Chain: Chain,
		Args: []string{
			"-s", ip,
			"-m", "comment", "--comment", comment,
			"-j", "SNAT",
			"--to-source", lb,
		},
		Protocol: getIPTablesProtocol(ip),
	}
}

// getIPTablesProtocol returns the IPTables protocol matching the protocol (v4/v6) of provided IP string
func getIPTablesProtocol(ip string) iptables.Protocol {
	if utilnet.IsIPv6String(ip) {
		return iptables.ProtocolIPv6
	}
	return iptables.ProtocolIPv4
}

func (c *Controller) defaultReturnRules(proto iptables.Protocol) []nodeipt.Rule {
	return []nodeipt.Rule{
		{
			Table: "nat",
			Chain: Chain,
			Args: []string{
				"-m", "mark", "--mark", string(c.returnMark),
				"-m", "comment", "--comment", "Do not SNAT to SVC VIP",
				"-j", "RETURN",
			},
			Protocol: proto,
		},
		{
			Table: "mangle",
			Chain: Chain,
			Args: []string{
				"-m", "mark", "--mark", string(c.returnMark),
				"-m", "comment", "--comment", "Do not mark",
				"-j", "RETURN",
			},
			Protocol: proto,
		},
	}
}
