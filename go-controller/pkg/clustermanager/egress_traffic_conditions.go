// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

const (
	trafficSelectorResolved = "TrafficSelectorResolved"
	trafficConflict         = "TrafficConflict"
)

type trafficConditionHandler struct {
	informer     cache.SharedIndexInformer
	registration cache.ResourceEventHandlerRegistration
}

func (e *egressIPClusterController) startTrafficConditions() error {
	e.trafficConditions = controller.NewReconciler("egress-traffic-conditions", &controller.ReconcilerConfig{
		Threadiness: 1, MaxAttempts: controller.InfiniteAttempts,
		Reconcile: func(string) error { return e.reconcileTrafficConditions() },
	})
	informers := []cache.SharedIndexInformer{
		e.watchFactory.EgressIPInformer().Informer(),
		e.watchFactory.EgressTrafficClassInformer().Informer(),
		e.watchFactory.PodCoreInformer().Informer(),
		e.watchFactory.NamespaceCoreInformer().Informer(),
	}
	for _, informer := range informers {
		registration, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    e.enqueueTrafficConditions,
			DeleteFunc: e.enqueueTrafficConditions,
			UpdateFunc: func(old, new interface{}) {
				if trafficConditionNeedsUpdate(old, new) {
					e.enqueueTrafficConditions(old)
					e.enqueueTrafficConditions(new)
				}
			},
		})
		if err != nil {
			return err
		}
		e.trafficConditionHandlers = append(e.trafficConditionHandlers, trafficConditionHandler{informer, registration})
		if !util.WaitForInformerCacheSyncWithTimeout("egress-traffic-conditions", e.stopChan, informer.HasSynced) {
			return fmt.Errorf("timed out syncing traffic condition informer")
		}
	}
	if err := controller.Start(e.trafficConditions); err != nil {
		return err
	}
	e.trafficConditions.Reconcile("")
	return nil
}

// Ignore workload events that cannot change any selective owner's pod set.
func (e *egressIPClusterController) enqueueTrafficConditions(obj interface{}) {
	affected, err := e.affectsTrafficConditions(obj)
	if err != nil || affected {
		e.trafficConditions.Reconcile("")
	}
}

func (e *egressIPClusterController) affectsTrafficConditions(obj interface{}) (bool, error) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	var pod *corev1.Pod
	var ns *corev1.Namespace
	switch value := obj.(type) {
	case *corev1.Pod:
		if !util.PodNeedsSNAT(value) {
			return false, nil
		}
		pod = value
		var err error
		ns, err = e.watchFactory.NamespaceCoreInformer().Lister().Get(pod.Namespace)
		if err != nil {
			return false, err
		}
	case *corev1.Namespace:
		ns = value
	default:
		return true, nil
	}
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return false, err
	}
	for _, eip := range eips {
		if !egressip.HasTrafficSelector(eip.Spec) {
			continue
		}
		nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			return false, err
		}
		if !nsSelector.Matches(labels.Set(ns.Labels)) {
			continue
		}
		if pod == nil {
			return true, nil
		}
		podSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
		if err != nil {
			return false, err
		}
		if podSelector.Matches(labels.Set(pod.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

func (e *egressIPClusterController) stopTrafficConditions() {
	for _, handler := range e.trafficConditionHandlers {
		_ = handler.informer.RemoveEventHandler(handler.registration)
	}
	if e.trafficConditions != nil {
		controller.Stop(e.trafficConditions)
	}
}

func trafficConditionNeedsUpdate(old, new interface{}) bool {
	switch before := old.(type) {
	case *eipv1.EgressIP:
		return !reflect.DeepEqual(before.Spec, new.(*eipv1.EgressIP).Spec)
	case *trafficv1.EgressTrafficClass:
		after := new.(*trafficv1.EgressTrafficClass)
		return !reflect.DeepEqual(before.Labels, after.Labels) || !reflect.DeepEqual(before.Spec, after.Spec)
	case *corev1.Pod:
		after := new.(*corev1.Pod)
		return !reflect.DeepEqual(before.Labels, after.Labels) || util.PodNeedsSNAT(before) != util.PodNeedsSNAT(after)
	case *corev1.Namespace:
		return !reflect.DeepEqual(before.Labels, new.(*corev1.Namespace).Labels)
	}
	return false
}

type selectedEgressTraffic struct {
	eip          *eipv1.EgressIP
	destinations []egressip.Destination
	pods         sets.Set[string]
	conditions   []metav1.Condition
}

func (e *egressIPClusterController) reconcileTrafficConditions() error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	classes, err := e.watchFactory.EgressTrafficClassInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}
	pods, err := e.watchFactory.PodCoreInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}
	namespaces, err := e.watchFactory.NamespaceCoreInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}
	namespaceLabels := map[string]labels.Set{}
	for _, ns := range namespaces {
		namespaceLabels[ns.Name] = labels.Set(ns.Labels)
	}
	slices.SortFunc(eips, func(a, b *eipv1.EgressIP) int { return strings.Compare(a.Name, b.Name) })
	selected := make([]selectedEgressTraffic, 0, len(eips))
	for _, eip := range eips {
		if !egressip.HasTrafficSelector(eip.Spec) {
			if err = e.patchTrafficConditions(eip, nil); err != nil {
				return err
			}
			continue
		}
		state, err := selectEgressTraffic(eip, classes, pods, namespaceLabels)
		if err != nil {
			state = selectedEgressTraffic{eip: eip, conditions: []metav1.Condition{
				{Type: trafficSelectorResolved, Status: metav1.ConditionFalse, Reason: "InvalidTrafficSelection", Message: err.Error()},
				{Type: trafficConflict, Status: metav1.ConditionUnknown, Reason: "InvalidTrafficSelection", Message: "Cannot determine conflicts for invalid traffic selection"},
			}}
		}
		selected = append(selected, state)
	}
	for i := range selected {
		for j := i + 1; j < len(selected); j++ {
			a, b := &selected[i], &selected[j]
			shared := sets.List(a.pods.Intersection(b.pods))
			if len(shared) == 0 {
				continue
			}
			for _, x := range a.destinations {
				for _, y := range b.destinations {
					if x.Overlaps(y) {
						setTrafficConflict(a, b.eip.Name, shared[0], x, y)
						setTrafficConflict(b, a.eip.Name, shared[0], y, x)
					}
				}
			}
		}
	}
	for _, state := range selected {
		if err = e.patchTrafficConditions(state.eip, state.conditions); err != nil {
			return err
		}
	}
	return nil
}

func selectEgressTraffic(eip *eipv1.EgressIP, classes []*trafficv1.EgressTrafficClass, pods []*corev1.Pod, namespaceLabels map[string]labels.Set) (selectedEgressTraffic, error) {
	state := selectedEgressTraffic{eip: eip, pods: sets.New[string]()}
	selector, err := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector)
	if err != nil {
		return state, err
	}
	var raw []trafficv1.Destination
	classCount := 0
	for _, class := range classes {
		if selector.Matches(labels.Set(class.Labels)) {
			classCount++
			raw = append(raw, class.Spec.Destinations...)
		}
	}
	state.destinations, err = egressip.ParseDestinations(raw)
	if err != nil {
		return state, err
	}
	resolved := metav1.Condition{Type: trafficSelectorResolved, Status: metav1.ConditionFalse,
		Reason: "NoMatchingEgressTrafficClass", Message: "No EgressTrafficClass resources match trafficSelector"}
	if len(state.destinations) > 0 {
		resolved.Status, resolved.Reason = metav1.ConditionTrue, "DestinationsFound"
		resolved.Message = fmt.Sprintf("Resolved %d destinations from %d EgressTrafficClass resource(s)", len(raw), classCount)
	}
	state.conditions = []metav1.Condition{resolved, {Type: trafficConflict, Status: metav1.ConditionFalse,
		Reason: "NoOverlappingTraffic", Message: "No overlapping traffic with another EgressIP targeting the same pod"}}
	nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return state, err
	}
	podSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
	if err != nil {
		return state, err
	}
	for _, pod := range pods {
		nsLabels, exists := namespaceLabels[pod.Namespace]
		if exists && util.PodNeedsSNAT(pod) && nsSelector.Matches(nsLabels) && podSelector.Matches(labels.Set(pod.Labels)) {
			state.pods.Insert(pod.Namespace + "/" + pod.Name)
		}
	}
	return state, nil
}

func setTrafficConflict(state *selectedEgressTraffic, peer, pod string, own, other egressip.Destination) {
	condition := &state.conditions[1]
	if condition.Status == metav1.ConditionTrue {
		return
	}
	condition.Status, condition.Reason = metav1.ConditionTrue, "OverlappingTraffic"
	condition.Message = fmt.Sprintf("Destination %s overlaps %s with EgressIP %s for pod %s", own, other, peer, pod)
}

func (e *egressIPClusterController) patchTrafficConditions(snapshot *eipv1.EgressIP, desired []metav1.Condition) error {
	if reflect.DeepEqual(mergeTrafficConditions(snapshot.Status.Conditions, desired), snapshot.Status.Conditions) {
		return nil
	}
	client := e.kube.EIPClient.K8sV1().EgressIPs()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(context.TODO(), snapshot.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.UID != snapshot.UID || !reflect.DeepEqual(current.Spec, snapshot.Spec) {
			return fmt.Errorf("EgressIP %s changed during traffic condition reconciliation", snapshot.Name)
		}
		conditions := mergeTrafficConditions(current.Status.Conditions, desired)
		if reflect.DeepEqual(conditions, current.Status.Conditions) {
			return nil
		}
		patch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": current.ResourceVersion},
			"status":   map[string]interface{}{"conditions": conditions},
		})
		if err != nil {
			return err
		}
		_, err = client.Patch(context.TODO(), current.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
		if err == nil {
			before := meta.FindStatusCondition(current.Status.Conditions, trafficSelectorResolved)
			after := meta.FindStatusCondition(conditions, trafficSelectorResolved)
			if after != nil && after.Status == metav1.ConditionFalse && after.Reason == "NoMatchingEgressTrafficClass" &&
				(before == nil || before.Status != after.Status || before.Reason != after.Reason) {
				e.recorder.Event(current, corev1.EventTypeWarning, "NoTrafficClass", "TrafficSelector is set but no matching EgressTrafficClass resources with destinations were found; no traffic will use this EgressIP until matching EgressTrafficClass resources are created")
			}
		}
		return err
	})
}

func mergeTrafficConditions(current, desired []metav1.Condition) []metav1.Condition {
	conditions := slices.Clone(current)
	for _, name := range []string{trafficSelectorResolved, trafficConflict} {
		condition := meta.FindStatusCondition(desired, name)
		if condition == nil {
			meta.RemoveStatusCondition(&conditions, name)
		} else {
			// Omit observedGeneration: legacy status writes also advance generation.
			meta.SetStatusCondition(&conditions, *condition)
		}
	}
	return conditions
}
