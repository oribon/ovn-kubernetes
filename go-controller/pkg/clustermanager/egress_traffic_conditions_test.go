// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestTrafficConditionLifecycle(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEgressTrafficClass = true
	ctx := context.Background()
	eip := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", Generation: 1}, Spec: eipv1.EgressIPSpec{
		TrafficSelector:   &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}},
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "one"}},
		PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "sip"}},
	}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: "192.0.2.10"}},
		Conditions: []metav1.Condition{{Type: "OtherWriter", Status: metav1.ConditionTrue, Reason: "Healthy", Message: "keep", LastTransitionTime: metav1.Now()}}}}
	peer := eip.DeepCopy()
	peer.Name = "two"
	peer.Spec.TrafficSelector.MatchLabels["traffic"] = "two"
	class := &trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"traffic": "one"}},
		Spec: trafficv1.EgressTrafficClassSpec{Destinations: []trafficv1.Destination{{CIDR: "198.51.100.0/24", Protocols: []trafficv1.ProtocolMatch{
			{UDP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)}}},
		}}}}}
	peerClass := class.DeepCopy()
	peerClass.Name = "two"
	peerClass.Labels["traffic"] = "two"
	peerClass.Spec.Destinations[0].Protocols[0].UDP = &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload", Labels: map[string]string{"tenant": "one"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sip", Namespace: ns.Name, Labels: map[string]string{"app": "sip"}}, Spec: corev1.PodSpec{NodeName: "node"}}
	catchAll := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "catch-all"}}
	clients := util.GetOVNClientset(eip, peer, catchAll, class, peerClass, ns, pod)
	wf, err := factory.NewClusterManagerWatchFactory(clients.GetClusterManagerClientset())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wf.Stop)
	for _, pair := range []struct {
		obj runtime.Object
		add func(interface{}) error
	}{
		{eip, wf.EgressIPInformer().Informer().GetStore().Add},
		{peer, wf.EgressIPInformer().Informer().GetStore().Add},
		{catchAll, wf.EgressIPInformer().Informer().GetStore().Add},
		{class, wf.EgressTrafficClassInformer().Informer().GetStore().Add},
		{peerClass, wf.EgressTrafficClassInformer().Informer().GetStore().Add},
		{ns, wf.NamespaceCoreInformer().Informer().GetStore().Add},
		{pod, wf.PodCoreInformer().Informer().GetStore().Add},
	} {
		if err = pair.add(pair.obj); err != nil {
			t.Fatal(err)
		}
	}
	recorder := record.NewFakeRecorder(10)
	e := &egressIPClusterController{watchFactory: wf, kube: &kube.KubeOVN{EIPClient: clients.EgressIPClient}, recorder: recorder}
	get := func(name string) *eipv1.EgressIP {
		t.Helper()
		got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Deliver status writes to the informer without sharing mutable test objects.
		if err = wf.EgressIPInformer().Informer().GetStore().Update(got.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		return got
	}
	check := func(name, typ string, status metav1.ConditionStatus) *metav1.Condition {
		t.Helper()
		c := meta.FindStatusCondition(get(name).Status.Conditions, typ)
		if c == nil || c.Status != status {
			t.Fatalf("%s %s = %+v, want %s", name, typ, c, status)
		}
		return c
	}
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	check("one", trafficSelectorResolved, metav1.ConditionTrue)
	for _, name := range []string{"one", "two"} {
		if c := check(name, trafficConflict, metav1.ConditionTrue); !strings.Contains(c.Message, "workload/sip") {
			t.Fatal("conflict does not identify shared pod")
		}
	}
	before := get("one").Status.Conditions
	unrelated := pod.DeepCopy()
	unrelated.Name, unrelated.Labels = "unrelated", map[string]string{"app": "other"}
	if affected, err := e.affectsTrafficConditions(unrelated); err != nil || affected {
		t.Fatalf("unrelated pod queued a global scan: affected=%t, err=%v", affected, err)
	}
	if affected, err := e.affectsTrafficConditions(pod); err != nil || !affected {
		t.Fatalf("selected pod did not queue a scan: affected=%t, err=%v", affected, err)
	}
	if err = wf.PodCoreInformer().Informer().GetStore().Add(unrelated); err != nil {
		t.Fatal(err)
	}
	fakeClient := clients.EgressIPClient.(*eipfake.Clientset)
	fakeClient.ClearActions()
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	if actions := fakeClient.Actions(); len(actions) != 0 {
		t.Fatalf("unchanged selection made API calls: %+v", actions)
	}
	patchCount := func() int {
		count := 0
		for _, action := range clients.EgressIPClient.(*eipfake.Clientset).Actions() {
			if action.GetVerb() == "patch" {
				count++
			}
		}
		return count
	}
	actions := patchCount()
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	if patchCount() != actions {
		t.Fatal("unchanged conditions were written again")
	}
	if !reflect.DeepEqual(before, get("one").Status.Conditions) {
		t.Fatal("unchanged reconciliation moved transition times")
	}
	// Previously admitted malformed selectors must not block healthy owners.
	invalid := eip.DeepCopy()
	invalid.Name, invalid.Status.Conditions = "invalid", nil
	invalid.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Create(ctx, invalid, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Add(invalid); err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	if c := check("invalid", trafficSelectorResolved, metav1.ConditionFalse); c.Reason != "InvalidTrafficSelection" {
		t.Fatalf("invalid selector not diagnosed: %+v", c)
	}
	check("invalid", trafficConflict, metav1.ConditionUnknown)
	check("one", trafficConflict, metav1.ConditionTrue)
	select {
	case event := <-recorder.Events:
		t.Fatalf("invalid selector emitted a misleading no-class warning: %s", event)
	default:
	}
	// Assignment and condition updates own separate fields on the legacy endpoint.
	if err = e.patchEgressIP("one", egressIPPatch{Status: &egressIPItemsPatch{Items: []eipv1.EgressIPStatusItem{{Node: "replacement", EgressIP: "192.0.2.11"}}}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, get("one").Status.Conditions) {
		t.Fatal("assignment patch erased conditions")
	}
	if err = wf.EgressTrafficClassInformer().Informer().GetStore().Delete(peerClass); err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	check("one", trafficConflict, metav1.ConditionFalse)
	check("two", trafficSelectorResolved, metav1.ConditionFalse)
	check("two", trafficConflict, metav1.ConditionFalse)
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "NoTrafficClass") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing warning")
	}
	if meta.FindStatusCondition(get("one").Status.Conditions, "OtherWriter") == nil || get("one").Status.Items[0].Node != "replacement" {
		t.Fatal("condition patch lost another writer's state")
	}
	// A peer selecting a different actual pod is not a conflict.
	if err = wf.EgressTrafficClassInformer().Informer().GetStore().Add(peerClass); err != nil {
		t.Fatal(err)
	}
	peer = get("two")
	peer.Spec.PodSelector.MatchLabels["app"] = "another-app"
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Update(ctx, peer, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Update(peer); err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	check("one", trafficConflict, metav1.ConditionFalse)
	// Removing a selector removes only our conditions.
	eip = get("one")
	eip.Spec.TrafficSelector = nil
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Update(ctx, eip, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Update(eip); err != nil {
		t.Fatal(err)
	}
	if err = e.reconcileTrafficConditions(); err != nil {
		t.Fatal(err)
	}
	if len(get("one").Status.Conditions) != 1 {
		t.Fatal("selector removal left traffic conditions")
	}
	if err = e.patchEgressIP("one", egressIPPatch{Status: &egressIPItemsPatch{}}); err != nil {
		t.Fatal(err)
	}
	if items := get("one").Status.Items; items == nil || len(items) != 0 {
		t.Fatal("empty assignment patch must retain the required items array")
	}
}

func TestTrafficConditionRelevantEvents(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "one"}}, Spec: corev1.PodSpec{NodeName: "node"}}
	updated := pod.DeepCopy()
	updated.Status.PodIP = "10.128.0.2"
	if trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("unrelated pod status change triggered a global overlap scan")
	}
	updated.Labels["app"] = "two"
	if !trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("workload selector change was missed")
	}
	updated = pod.DeepCopy()
	updated.Status.Phase = corev1.PodSucceeded
	if !trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("completed pod could leave a stale conflict")
	}
	eip := &eipv1.EgressIP{}
	changed := eip.DeepCopy()
	changed.Status.Conditions = []metav1.Condition{{Type: trafficConflict, Status: metav1.ConditionTrue}}
	if trafficConditionNeedsUpdate(eip, changed) {
		t.Fatal("condition writer enqueued itself")
	}
	changed.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}}
	if !trafficConditionNeedsUpdate(eip, changed) {
		t.Fatal("traffic selector change was missed")
	}
}
