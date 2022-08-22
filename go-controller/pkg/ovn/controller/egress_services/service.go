package egress_services

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// onServiceAdd queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	service := obj.(*corev1.Service)
	if !util.HasEgressSVCAnnotation(service) {
		return
	}

	klog.V(4).Infof("Adding egress service %s", key)
	c.servicesQueue.Add(key)
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*corev1.Service)
	newService := newObj.(*corev1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	if !util.HasEgressSVCAnnotation(oldService) && !util.HasEgressSVCAnnotation(newService) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.servicesQueue.Add(key)
	}
}

// onServiceDelete queues the Service for processing.
func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	service := obj.(*corev1.Service)
	if !util.HasEgressSVCAnnotation(service) {
		return
	}

	klog.V(4).Infof("Deleting egress service %s", key)
	c.servicesQueue.Add(key)
}

func (c *Controller) runServiceWorker(wg *sync.WaitGroup) {
	for c.processNextServiceWorkItem(wg) {
	}
}

func (c *Controller) processNextServiceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.servicesQueue.Get()
	if quit {
		return false
	}

	defer c.servicesQueue.Done(key)

	err := c.syncService(key.(string))
	if err == nil {
		c.servicesQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.servicesQueue.NumRequeues(key) < maxRetries {
		c.servicesQueue.AddRateLimited(key)
		return true
	}

	c.servicesQueue.Forget(key)
	return true
}

func (c *Controller) syncService(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for Egress Service %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing Egress Service %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	svc, err := c.serviceLister.Services(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	state, found := c.services[key]
	if svc == nil && !found {
		delete(c.unallocatedServices, key) // just in case
		return nil
	}

	if svc == nil {
		return c.clearServiceResources(key, state)
	}

	if state != nil && state.stale {
		return c.clearServiceResources(key, state) // if it's stale the node tried to delete it but failed, attempt cleaning up
	}

	conf, err := util.ParseEgressSVCAnnotation(svc)
	if err != nil && !util.IsAnnotationNotSetError(err) {
		return err
	}

	if conf == nil && !found {
		return nil
	}

	if conf == nil && found {
		return c.clearServiceResources(key, state)
	}

	if conf != nil && !found {
		selector, _ := metav1.LabelSelectorAsSelector(&conf.NodeSelector)
		c.unallocatedServices[key] = selector

		node, err := c.selectNodeFor(selector)
		if err != nil {
			return err
		}

		err = c.annotateServiceWithNode(name, namespace, node.name)
		if err != nil {
			return err
		}

		delete(c.unallocatedServices, key) // we found a node
		// updating caches
		newState := &svcState{node: node.name, selector: selector, v4Endpoints: sets.NewString(), v6Endpoints: sets.NewString(), stale: false}
		c.services[key] = newState
		node.allocations[key] = newState
		c.nodes[node.name] = node
		state = newState
	}

	state.selector, _ = metav1.LabelSelectorAsSelector(&conf.NodeSelector)
	node := c.nodes[state.node]

	if !state.selector.Matches(labels.Set(node.labels)) {
		err := c.clearServiceResources(key, state)
		if err != nil {
			return err
		}
		c.servicesQueue.AddRateLimited(key)
		return nil
	}

	// todo?: check if assigned node is still valid...
	// if it matches the labels, if it is ready and reachable
	// might not be here, we should trigger it from the node's reconciliation
	// also, think about where/when we label the node

	// update path

	v4Endpoints, v6Endpoints, err := c.allEndpointsFor(svc)
	if err != nil {
		return err
	}

	v4ToAdd := v4Endpoints.Difference(state.v4Endpoints)
	v6ToAdd := v6Endpoints.Difference(state.v6Endpoints)
	v4ToRemove := state.v4Endpoints.Difference(v4Endpoints)
	v6ToRemove := state.v6Endpoints.Difference(v6Endpoints)

	allOps := []libovsdb.Operation{}
	createOps, err := c.createLogicalRouterPoliciesOps(key, node.v4MgmtIP.String(), node.v6MgmtIP.String(), v4ToAdd.UnsortedList(), v6ToAdd.UnsortedList())
	if err != nil {
		return err
	}
	allOps = append(allOps, createOps...)

	deleteOps, err := c.deleteLogicalRouterPoliciesOps(key, v4ToRemove, v6ToRemove)
	if err != nil {
		return err
	}
	allOps = append(allOps, deleteOps...)

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to update router policies for %s, err: %v", key, err)
	}

	// update caches
	state.v4Endpoints.Insert(v4ToAdd.UnsortedList()...)
	state.v4Endpoints.Delete(v4ToRemove.UnsortedList()...)
	state.v6Endpoints.Insert(v6ToAdd.UnsortedList()...)
	state.v6Endpoints.Delete(v6ToRemove.UnsortedList()...)

	return c.labelNodeForService(namespace, name, node.name)
}

func (c *Controller) clearServiceResources(key string, svcState *svcState) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	svcState.stale = true // clear annotation first, as it gets deleted from the cache later
	if err := c.removeServiceNodeAnnotation(name, namespace); err != nil {
		return err
	}

	p := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs["EgressSVC"] == key
	}

	deleteOps := []libovsdb.Operation{}
	deleteOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, deleteOps, ovntypes.OVNClusterRouter, p)
	if err != nil {
		return err
	}

	if _, err := libovsdbops.TransactAndCheck(c.nbClient, deleteOps); err != nil {
		return fmt.Errorf("failed to clean router policies for %s, err: %v", key, err)
	}

	nodeState, found := c.nodes[svcState.node]
	if found {
		if err := c.removeNodeServiceLabel(namespace, name, svcState.node); err != nil {
			return fmt.Errorf("failed to remove svc node label for %s, err: %v", svcState.node, err)
		}
		delete(nodeState.allocations, key)
	}

	delete(c.services, key)
	delete(c.unallocatedServices, key)
	return nil
}

func (c *Controller) annotateServiceWithNode(name, namespace string, node string) error {
	annotations := map[string]any{util.EgressSVCHostAnnotation: node}
	return c.patchServiceAnnotations(name, namespace, annotations)
}

func (c *Controller) removeServiceNodeAnnotation(name, namespace string) error {
	annotations := map[string]any{util.EgressSVCHostAnnotation: nil} // patching with nil causes a remove
	return c.patchServiceAnnotations(name, namespace, annotations)
}

func (c *Controller) patchServiceAnnotations(name, namespace string, annotations map[string]any) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"annotations": annotations,
		},
	}

	klog.Infof("Setting annotations %v on service %s/%s", annotations, namespace, name)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting annotations on service %s/%s: %v", namespace, name, err)
		return err
	}

	_, err = c.client.CoreV1().Services(namespace).Patch(context.TODO(), name, types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func (c *Controller) nodeLabelForService(namespace, name string) string {
	return fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s", namespace, name)
}

func (c *Controller) selectNodeFor(selector labels.Selector) (*nodeState, error) {
	nodes, err := c.nodeLister.List(selector)
	if err != nil {
		return nil, err
	}

	// TODO: in reachability check, we should mark an unreachable node before deleting it from cache
	allNodes := sets.NewString()
	for _, n := range nodes {
		if nodeIsReady(n) {
			allNodes.Insert(n.Name)
		}
	}

	cachedNames, cachedStates := c.cachedNodesFor(selector)

	freeNodes := allNodes.Difference(cachedNames)
	if freeNodes.Len() > 0 {
		// we have a matching node with 0 allocations
		node, _ := freeNodes.PopAny()
		return c.nodeStateFor(node)
	}

	// we need to use one of the used nodes, we will pick the one with the least amount of allocations
	sort.Slice(cachedStates, func(i, j int) bool {
		return len(cachedStates[i].allocations) < len(cachedStates[j].allocations)
	})

	for _, node := range cachedStates {
		if !node.draining {
			return node, nil
		}
	}

	return nil, fmt.Errorf("no suitable node for selector: %s", selector.String())
}

func (c *Controller) allEndpointsFor(svc *corev1.Service) (sets.String, sets.String, error) {
	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discovery.LabelServiceName: svc.Name,
	}).AsSelectorPreValidated()

	endpointSlices, err := c.endpointSliceLister.EndpointSlices(svc.Namespace).List(esLabelSelector)
	if err != nil {
		return nil, nil, err
	}

	v4Endpoints := sets.NewString()
	v6Endpoints := sets.NewString()
	for _, eps := range endpointSlices {
		for _, ep := range eps.Endpoints {
			for _, addr := range ep.Addresses {
				if utilnet.IsIPv4String(addr) {
					v4Endpoints.Insert(addr)
					continue
				}
				v6Endpoints.Insert(addr)
			}
		}
	}

	return v4Endpoints, v6Endpoints, nil
}

func (c *Controller) createLogicalRouterPoliciesOps(key, v4MgmtIP, v6MgmtIP string, v4Endpoints, v6Endpoints []string) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip4.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v4MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				"EgressSVC": key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs["EgressSVC"] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip6.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v6MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				"EgressSVC": key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs["EgressSVC"] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}

func (c *Controller) deleteLogicalRouterPoliciesOps(key string, v4Endpoints, v6Endpoints sets.String) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints.UnsortedList() {
		match := fmt.Sprintf("ip4.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs["EgressSVC"] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints.UnsortedList() {
		match := fmt.Sprintf("ip6.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs["EgressSVC"] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}
