// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestTrafficClassClusterManagerInformers(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEgressTrafficClass = true
	config.OVNKubernetesFeature.EnableMultiNetwork = false
	clients := util.GetOVNClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}},
		&trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "traffic"}},
	).GetClusterManagerClientset()
	wf, err := NewClusterManagerWatchFactory(clients)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wf.Shutdown)
	if err = wf.Start(); err != nil {
		t.Fatal(err)
	}
	classes, err := wf.EgressTrafficClassInformer().Lister().List(labels.Everything())
	if err != nil || len(classes) != 1 {
		t.Fatalf("traffic class cache: %v, %v", classes, err)
	}
	pods, err := wf.PodCoreInformer().Lister().List(labels.Everything())
	if err != nil || len(pods) != 1 {
		t.Fatalf("pod cache without multi-network: %v, %v", pods, err)
	}
	namespaces, err := wf.NamespaceCoreInformer().Lister().List(labels.Everything())
	if err != nil || len(namespaces) != 1 {
		t.Fatalf("namespace cache without multi-network: %v, %v", namespaces, err)
	}
}
