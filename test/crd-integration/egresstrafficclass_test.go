// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EgressTrafficClass admission", func() {
	DescribeTable("validates the destination and port contract", func(destinations []trafficv1.Destination, valid bool) {
		class := &trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{GenerateName: "traffic-"}, Spec: trafficv1.EgressTrafficClassSpec{Destinations: destinations}}
		err := k8sClient.Create(ctx, class)
		if valid {
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, class)).To(Succeed()) })
		} else {
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		}
	},
		Entry("IPv4 and IPv6 CIDR-only", []trafficv1.Destination{{CIDR: "192.0.2.0/24"}, {CIDR: "2001:db8::/32"}}, true),
		Entry("any UDP ports and SCTP ranges", []trafficv1.Destination{{CIDR: "0.0.0.0/0", Protocols: []trafficv1.ProtocolMatch{{UDP: &trafficv1.PortProtocol{}}, {SCTP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 1, End: 65535}}, SourcePort: &trafficv1.Port{Number: ptr.To[int32](5060)}}}}}}, true),
		Entry("no destinations", []trafficv1.Destination{}, false),
		Entry("too many destinations", func() []trafficv1.Destination {
			ds := make([]trafficv1.Destination, 26)
			for i := range ds {
				ds[i].CIDR = "192.0.2.0/24"
			}
			return ds
		}(), false),
		Entry("maximum destinations", func() []trafficv1.Destination {
			ds := make([]trafficv1.Destination, 25)
			for i := range ds {
				ds[i].CIDR = "192.0.2.0/24"
			}
			return ds
		}(), true),
		Entry("invalid CIDR", []trafficv1.Destination{{CIDR: "not-a-cidr"}}, false),
		Entry("IPv4 host bits", []trafficv1.Destination{{CIDR: "192.0.2.1/24"}}, false),
		Entry("IPv6 host bits", []trafficv1.Destination{{CIDR: "2001:db8::1/64"}}, false),
		Entry("empty protocol union", []trafficv1.Destination{{CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{}}}}, false),
		Entry("two protocols", []trafficv1.Destination{{CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{}, UDP: &trafficv1.PortProtocol{}}}}}, false),
		Entry("empty port union", trafficPortDestination(&trafficv1.Port{}), false),
		Entry("both port forms", trafficPortDestination(&trafficv1.Port{Number: ptr.To[int32](80), Range: &trafficv1.PortRange{Start: 80, End: 90}}), false),
		Entry("port zero", trafficPortDestination(&trafficv1.Port{Number: ptr.To[int32](0)}), false),
		Entry("port above 65535", trafficPortDestination(&trafficv1.Port{Number: ptr.To[int32](65536)}), false),
		Entry("reversed port range", trafficPortDestination(&trafficv1.Port{Range: &trafficv1.PortRange{Start: 90, End: 80}}), false),
		Entry("range below bound", trafficPortDestination(&trafficv1.Port{Range: &trafficv1.PortRange{Start: 0, End: 80}}), false),
		Entry("range above bound", trafficPortDestination(&trafficv1.Port{Range: &trafficv1.PortRange{Start: 80, End: 65536}}), false),
	)
	DescribeTable("distinguishes absent and empty traffic selectors", func(selector *metav1.LabelSelector, valid bool) {
		eip := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{GenerateName: "traffic-selector-"}, Spec: eipv1.EgressIPSpec{EgressIPs: []string{"192.0.2.10"}, TrafficSelector: selector, NamespaceSelector: metav1.LabelSelector{}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{}}}
		err := k8sClient.Create(ctx, eip)
		if valid {
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, eip)).To(Succeed()) })
			Expect(eip.Spec.TrafficSelector).To(Equal(selector))
		} else {
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		}
	},
		Entry("absent", nil, true),
		Entry("empty", &metav1.LabelSelector{}, false),
		Entry("matchLabels", &metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}}, true),
		Entry("matchExpressions", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpExists}}}, true),
		Entry("unknown operator", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}, false),
		Entry("In without values", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpIn}}}, false),
		Entry("Exists with values", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpExists, Values: []string{"one"}}}}, false),
		Entry("NotIn with values", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "example.com/class", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"one", ""}}}}, true),
		Entry("DoesNotExist", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpDoesNotExist}}}, true),
	)
	It("preserves assignment items and conditions through independent patches", func() {
		eip := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{GenerateName: "traffic-status-"}, Spec: eipv1.EgressIPSpec{EgressIPs: []string{"192.0.2.10"}, NamespaceSelector: metav1.LabelSelector{}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{}}}
		Expect(k8sClient.Create(ctx, eip)).To(Succeed())
		generation := eip.Generation
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, eip)).To(Succeed()) })
		Expect(k8sClient.Patch(ctx, eip, client.RawPatch(ktypes.MergePatchType, []byte(`{"status":{"conditions":[{"type":"TrafficSelectorResolved","status":"True","reason":"DestinationsFound","message":"Resolved 1 destinations from 1 EgressTrafficClass resource(s)","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`)))).To(Succeed())
		Expect(k8sClient.Patch(ctx, eip, client.RawPatch(ktypes.MergePatchType, []byte(`{"status":{"items":[{"node":"node","egressIP":"192.0.2.10"}]}}`)))).To(Succeed())
		Expect(eip.Status.Items).To(HaveLen(1))
		Expect(eip.Status.Conditions).To(HaveLen(1))
		Expect(eip.Status.Conditions[0].Type).To(Equal("TrafficSelectorResolved"))
		Expect(eip.Generation).To(BeNumerically(">", generation))
		Expect(k8sClient.Patch(ctx, eip, client.RawPatch(ktypes.MergePatchType, []byte(`{"status":{"items":[]}}`)))).To(Succeed())
		Expect(eip.Status.Items).To(BeEmpty())
		Expect(eip.Status.Conditions).To(HaveLen(1))
	})
})

func trafficPortDestination(port *trafficv1.Port) []trafficv1.Destination {
	return []trafficv1.Destination{{CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: port}}}}}
}
