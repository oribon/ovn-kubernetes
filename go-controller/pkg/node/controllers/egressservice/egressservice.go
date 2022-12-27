package egressservice

import (
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"

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
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	Chain = "OVN-KUBE-EGRESS-SVC" // called from nat-POSTROUTING and mangle-PREROUTING
)

type Controller struct {
	client kubernetes.Interface
	stopCh <-chan struct{}
	sync.Mutex
	thisNode string

	egressServiceLister egressservicelisters.EgressServiceLister
	egressServiceSynced cache.InformerSynced
	egressServiceQueue  workqueue.RateLimitingInterface

	serviceLister  corelisters.ServiceLister
	servicesSynced cache.InformerSynced

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced

	services map[string]*svcState // svc key -> state
}

type svcState struct {
	cips   sets.String
	v4LB   string
	v4Eps  sets.String
	v6Eps  sets.String
	v6LB   string
	fwmark uint32

	stale      bool
	staleRules []nodeipt.Rule
}

func NewController(client kubernetes.Interface, stopCh <-chan struct{}, thisNode string,
	esInformer egressserviceinformer.EgressServiceInformer,
	serviceInformer cache.SharedIndexInformer,
	endpointSliceInformer cache.SharedIndexInformer) *Controller {
	klog.Info("Setting up event handlers for Egress Services")

	c := &Controller{
		client:   client,
		stopCh:   stopCh,
		thisNode: thisNode,
		services: map[string]*svcState{},
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

func (c *Controller) repair() error { // actually do something here
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}

	err = ipt.NewChain("nat", Chain)
	if err != nil {
		return err
	}
	err = ipt.NewChain("mangle", Chain)
	if err != nil {
		return err
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

	// At this point both svc and es are not nil
	shouldConfigure := c.shouldConfigureEgressSVC(svc, es.Status.Host)
	if cachedState == nil && !shouldConfigure {
		return nil
	}

	if cachedState != nil && !shouldConfigure {
		return c.clearServiceRules(key, cachedState)
	}

	fwMarkChanged := false
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
		fwMarkChanged = cachedState.fwmark != es.Spec.FWMark
		lbsChanged = v4LB != cachedState.v4LB || v6LB != cachedState.v6LB
	}

	if fwMarkChanged || lbsChanged {
		return c.clearServiceRules(key, cachedState)
	}

	if cachedState == nil {
		cachedState = &svcState{
			cips:       sets.NewString(),
			v4LB:       v4LB,
			v6LB:       v6LB,
			v4Eps:      sets.NewString(),
			v6Eps:      sets.NewString(),
			fwmark:     es.Spec.FWMark,
			stale:      false,
			staleRules: []nodeipt.Rule{},
		}
		c.services[key] = cachedState
	}

	// if stale rules, delete them
	if len(cachedState.staleRules) > 0 {
		err := nodeipt.DelRules(cachedState.staleRules)
		if err != nil {
			return err
		}
	}
	cachedState.staleRules = []nodeipt.Rule{}

	cips := sets.NewString() // current cips
	for _, cip := range svc.Spec.ClusterIPs {
		ip := utilnet.ParseIPSloppy(cip).String()
		cips.Insert(ip)
	}
	cipsToAdd := cips.Difference(cachedState.cips).UnsortedList()
	cipsToDelete := cachedState.cips.Difference(cips).UnsortedList()

	if cachedState.fwmark != 0 {
		for _, cip := range cipsToAdd {
			err := nodeipt.AddRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, cip)})
			if err != nil {
				return err
			}
			cachedState.cips.Insert(cip)
		}

		for _, cip := range cipsToDelete {
			err := nodeipt.DelRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, cip)})
			if err != nil {
				return err
			}
			cachedState.cips.Delete(cip)
		}
	}

	v4Eps, v6Eps, err := c.allEndpointsFor(svc) // current eps
	if err != nil {
		return err
	}

	v4ToAdd := v4Eps.Difference(cachedState.v4Eps).UnsortedList()
	v6ToAdd := v6Eps.Difference(cachedState.v6Eps).UnsortedList()
	v4ToDelete := cachedState.v4Eps.Difference(v4Eps).UnsortedList()
	v6ToDelete := cachedState.v6Eps.Difference(v6Eps).UnsortedList()

	if cachedState.v4LB != "" {
		addedFWMarks := []nodeipt.Rule{}
		for _, ep := range v4ToAdd {
			if cachedState.fwmark != 0 {
				fwmarkRule := fwmarkIPTRuleFor(key, cachedState.fwmark, ep)
				err := nodeipt.AddRules([]nodeipt.Rule{fwmarkRule})
				if err != nil {
					return err
				}
				addedFWMarks = append(addedFWMarks, fwmarkRule)
			}

			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)})
			if err != nil {
				cachedState.staleRules = addedFWMarks
				return err
			}

			cachedState.v4Eps.Insert(ep)
		}

		for _, ep := range v4ToDelete {
			if cachedState.fwmark != 0 {
				err := nodeipt.DelRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, ep)})
				if err != nil {
					return err
				}
			}

			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v4LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v4Eps.Delete(ep)
		}
	}

	if cachedState.v6LB != "" {
		addedFWMarks := []nodeipt.Rule{}
		for _, ep := range v6ToAdd {
			if cachedState.fwmark != 0 {
				fwmarkRule := fwmarkIPTRuleFor(key, cachedState.fwmark, ep)
				err := nodeipt.AddRules([]nodeipt.Rule{fwmarkRule})
				if err != nil {
					return err
				}
				addedFWMarks = append(addedFWMarks, fwmarkRule)
			}

			err := nodeipt.AddRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)})
			if err != nil {
				cachedState.staleRules = append(cachedState.staleRules, addedFWMarks...)
				return err
			}

			cachedState.v6Eps.Insert(ep)
		}

		for _, ep := range v6ToDelete {
			if cachedState.fwmark != 0 {
				err := nodeipt.DelRules([]nodeipt.Rule{fwmarkIPTRuleFor(key, cachedState.fwmark, ep)})
				if err != nil {
					return err
				}
			}

			err := nodeipt.DelRules([]nodeipt.Rule{snatIPTRuleFor(key, cachedState.v6LB, ep)})
			if err != nil {
				return err
			}

			cachedState.v6Eps.Delete(ep)
		}
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

func (c *Controller) clearServiceRules(key string, state *svcState) error {
	state.stale = true

	rules := []nodeipt.Rule{}
	if state.fwmark != 0 {
		for _, ip := range state.cips.UnsortedList() {
			rules = append(rules, fwmarkIPTRuleFor(key, state.fwmark, ip))
		}
		for _, ip := range state.v4Eps.UnsortedList() {
			rules = append(rules, fwmarkIPTRuleFor(key, state.fwmark, ip))
		}
		for _, ip := range state.v6Eps.UnsortedList() {
			rules = append(rules, fwmarkIPTRuleFor(key, state.fwmark, ip))
		}
	}

	if state.v4LB != "" {
		for _, ip := range state.v4Eps.UnsortedList() {
			rules = append(rules, snatIPTRuleFor(key, state.v4LB, ip))
		}
	}

	if state.v6LB != "" {
		for _, ip := range state.v6Eps.UnsortedList() {
			rules = append(rules, snatIPTRuleFor(key, state.v6LB, ip))
		}
	}

	rules = append(rules, state.staleRules...)

	err := nodeipt.DelRules(rules)
	if err != nil {
		return err
	}

	delete(c.services, key)
	c.egressServiceQueue.Add(key)

	return nil
}

func (c *Controller) shouldConfigureEgressSVC(svc *corev1.Service, svcHost string) bool {
	return svcHost == c.thisNode &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0
}

func fwmarkIPTRuleFor(comment string, fwmark uint32, ip string) nodeipt.Rule {
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

// Returns all of the SNAT rules that should be created for an egress service with the given endpoints
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
