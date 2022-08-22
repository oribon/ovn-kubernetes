package egress_services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (c *Controller) checkNodesReachability() {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			c.checkNodesReachabilityIterate()
		case <-c.stopCh:
			klog.V(5).Infof("Stop channel got triggered: will stop checkNodesReachability")
			return
		}
	}
}

func (c *Controller) checkNodesReachabilityIterate() {
	c.Lock()
	defer c.Unlock()

	nodesToFree := []string{}
	for _, node := range c.nodes {
		wasReachable := node.reachable
		isReachable := c.isReachable(node)
		node.reachable = isReachable
		if wasReachable && !isReachable { // we should start draining it
			c.nodesQueue.Add(node.name)
			continue
		}

		startedDrain := node.draining
		fullyDrained := len(node.allocations) == 0
		if startedDrain && fullyDrained && isReachable {
			nodesToFree = append(nodesToFree, node.name)
		}
	}

	for _, node := range nodesToFree {
		delete(c.nodes, node)
		c.nodesQueue.Add(node)
	}
}

// implement node reachability check loop
// implement allow policies (like egressip)
func (c *Controller) isReachable(node *nodeState) bool {
	return true
}

func (c *Controller) onNodeAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nodesQueue.Add(key)
}

func (c *Controller) onNodeUpdate(oldObj, newObj interface{}) {
	oldNode := oldObj.(*corev1.Node)
	newNode := newObj.(*corev1.Node)

	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return
	}

	oldNodeLabels := labels.Set(oldNode.Labels)
	newNodeLabels := labels.Set(newNode.Labels)
	oldNodeReady := nodeIsReady(oldNode)
	newNodeReady := nodeIsReady(newNode)

	if labels.Equals(oldNodeLabels, newNodeLabels) &&
		oldNodeReady == newNodeReady {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.nodesQueue.Add(key)
	}
}

func (c *Controller) onNodeDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nodesQueue.Add(key)
}

func (c *Controller) runNodeWorker(wg *sync.WaitGroup) {
	for c.processNextNodeWorkItem(wg) {
	}
}

func (c *Controller) processNextNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.nodesQueue.Get()
	if quit {
		return false
	}

	defer c.nodesQueue.Done(key)

	err := c.syncNode(key.(string))
	if err == nil {
		c.nodesQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.nodesQueue.NumRequeues(key) < maxRetries {
		c.nodesQueue.AddRateLimited(key)
		return true
	}

	c.nodesQueue.Forget(key)
	return true
}

func (c *Controller) syncNode(key string) error {
	c.Lock()
	defer c.Unlock()

	startTime := time.Now()
	_, nodeName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Processing sync for Egress Service node %s", nodeName)

	defer func() {
		klog.V(4).Infof("Finished syncing Egress Service node %s: %v", nodeName, time.Since(startTime))
	}()

	n, err := c.nodeLister.Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	state := c.nodes[nodeName]

	if n == nil && state == nil {
		return nil
	}

	if n == nil && state != nil {
		state.draining = true
		for svcKey, svcState := range state.allocations {
			if err := c.clearServiceResources(svcKey, svcState); err != nil {
				return err
			}
			c.servicesQueue.AddRateLimited(svcKey) // can't rely on the annotation change to trigger, can be stuck stale?
		}
		delete(c.nodes, nodeName)
		return nil
	}

	nodeReady := nodeIsReady(n) // n != nil here
	nodeLabels := n.Labels
	if state == nil && !nodeReady {
		return nil
	}

	if state == nil && nodeReady {
		for svcKey, selector := range c.unallocatedServices {
			if selector.Matches(labels.Set(nodeLabels)) {
				c.servicesQueue.Add(svcKey)
			}
		}
		return nil
	}

	if !state.reachable || !nodeReady || state.draining {
		state.draining = true
		for svcKey, svcState := range state.allocations {
			if err := c.clearServiceResources(svcKey, svcState); err != nil {
				return err
			}
			c.servicesQueue.AddRateLimited(svcKey) // can't rely on the annotation change to trigger, can be stuck stale?
		}
		return nil
	}

	state.labels = nodeLabels
	for svcKey, svcState := range state.allocations {
		if !svcState.selector.Matches(labels.Set(n.Labels)) || svcState.stale {
			if err := c.clearServiceResources(svcKey, svcState); err != nil {
				return err
			}
			c.servicesQueue.AddRateLimited(svcKey) // can't rely on the annotation change to trigger, can be stuck stale?
		}
	}

	for svcKey, selector := range c.unallocatedServices {
		if selector.Matches(labels.Set(nodeLabels)) {
			c.servicesQueue.Add(svcKey)
		}
	}

	return nil
}

func (c *Controller) nodeStateFor(name string) (*nodeState, error) {
	node, err := c.nodeLister.Get(name)
	if err != nil {
		return nil, err
	}

	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}

	mgmtIPs := make([]net.IP, len(nodeSubnets))
	for i, subnet := range nodeSubnets {
		mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
	}

	var v4IP, v6IP net.IP
	for _, ip := range mgmtIPs {
		if utilnet.IsIPv4(ip) {
			v4IP = ip
			continue
		}
		v6IP = ip
	}

	return &nodeState{name: name, v4MgmtIP: v4IP, v6MgmtIP: v6IP, allocations: map[string]*svcState{}, labels: node.Labels, reachable: true, draining: false}, nil
}

func (c *Controller) cachedNodesFor(selector labels.Selector) (sets.String, []*nodeState) {
	names := sets.NewString()
	states := []*nodeState{}
	for _, n := range c.nodes {
		if selector.Matches(labels.Set(n.labels)) {
			names.Insert(n.name)
			states = append(states, n)
		}
	}

	return names, states
}

func nodeIsReady(n *corev1.Node) bool {
	for _, condition := range n.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *Controller) labelNodeForService(namespace, name, node string) error {
	labels := map[string]any{c.nodeLabelForService(namespace, name): ""}
	return c.patchNodeLabels(node, labels)
}

func (c *Controller) removeNodeServiceLabel(namespace, name, node string) error {
	labels := map[string]any{c.nodeLabelForService(namespace, name): nil}
	return c.patchNodeLabels(node, labels)
}

func (c *Controller) patchNodeLabels(node string, labels map[string]any) error {
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"labels": labels,
		},
	}

	klog.Infof("Setting labels %v on node %s", labels, node)
	patchData, err := json.Marshal(&patch)
	if err != nil {
		klog.Errorf("Error in setting labels on node %s: %v", node, err)
		return err
	}

	_, err = c.client.CoreV1().Nodes().Patch(context.TODO(), node, types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
