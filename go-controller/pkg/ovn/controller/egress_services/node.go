package egress_services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// implement node reachability check loop
// onEgressQoSAdd queues the node for processing.
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

	state, found := c.nodes[nodeName]
	if !found {
		return nil
	}

	n, err := c.nodeLister.Get(nodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if n == nil {
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

	state.ready = nodeIsReady(n)
	if !state.reachable || !state.ready || state.draining {
		state.draining = true
		for svcKey, svcState := range state.allocations {
			if err := c.clearServiceResources(svcKey, svcState); err != nil {
				return err
			}
			c.servicesQueue.AddRateLimited(svcKey) // can't rely on the annotation change to trigger, can be stuck stale?
		}
	}

	state.labels = n.Labels
	for svcKey, svcState := range state.allocations {
		if !svcState.selector.Matches(labels.Set(n.Labels)) || svcState.stale {
			if err := c.clearServiceResources(svcKey, svcState); err != nil {
				return err
			}
			c.servicesQueue.AddRateLimited(svcKey) // can't rely on the annotation change to trigger, can be stuck stale?
		}
	}

	/*
		not the place to remove, we should remove in reachability check?
		because we should free up a node only when it is ready + reachable, not when 0 allocations
		if len(state.allocations) == 0 {
			delete(c.nodes, nodeName)
		}*/

	return nil
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
	fmt.Println("ORI:", err)
	return nil
}
