// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"net/netip"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
)

func TestParseDestinations(t *testing.T) {
	udp := trafficv1.ProtocolMatch{UDP: &trafficv1.PortProtocol{
		DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)},
		SourcePort:      &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}},
	}}
	raw := []trafficv1.Destination{
		{CIDR: "2001:db8::/32"},
		{CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{udp, udp, {SCTP: &trafficv1.PortProtocol{}}}},
		{CIDR: "192.0.2.0/24"},
	}
	matches, err := ParseDestinations(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []Destination{
		{CIDR: netip.MustParsePrefix("192.0.2.0/24")},
		{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolUDP, DestinationPort: PortRange{5060, 5060}, SourcePort: PortRange{16384, 32767}},
		{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolSCTP},
		{CIDR: netip.MustParsePrefix("2001:db8::/32")},
	}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("matches = %#v, want %#v", matches, want)
	}
	if len(UniqueCIDRs(matches)) != 2 {
		t.Fatal("routes were not deduplicated")
	}
	cidrOnly, l4 := SplitByL4(matches)
	if len(cidrOnly) != 2 || len(l4) != 2 {
		t.Fatal("wrong CIDR/L4 partition")
	}

	for name, invalid := range map[string]trafficv1.Destination{
		"host bits":      {CIDR: "192.0.2.1/24"},
		"invalid CIDR":   {CIDR: "invalid"},
		"empty protocol": {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{}}},
		"two protocols":  {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{}, UDP: &trafficv1.PortProtocol{}}}},
		"empty port":     {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{}}}}},
		"zero port":      {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Number: ptr.To[int32](0)}}}}},
		"reversed range": {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 99, End: 1}}}}}},
		"oversized port": {CIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Number: ptr.To[int32](65536)}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseDestinations([]trafficv1.Destination{invalid}); err == nil {
				t.Fatal("invalid match accepted")
			}
		})
	}
}

func TestDestinationOverlap(t *testing.T) {
	base := Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: ProtocolUDP, DestinationPort: PortRange{5060, 5062}}
	for _, tc := range []struct {
		name    string
		other   Destination
		overlap bool
	}{
		{"CIDR-only", Destination{CIDR: base.CIDR}, true},
		{"other family", Destination{CIDR: netip.MustParsePrefix("2001:db8::/32")}, false},
		{"disjoint CIDR", Destination{CIDR: netip.MustParsePrefix("198.51.100.0/24")}, false},
		{"other protocol", Destination{CIDR: base.CIDR, Protocol: ProtocolTCP}, false},
		{"destination boundary", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, DestinationPort: PortRange{5062, 5100}}, true},
		{"disjoint ports", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, DestinationPort: PortRange{5063, 5100}}, false},
		{"source and destination can match one packet", Destination{CIDR: base.CIDR, Protocol: ProtocolUDP, SourcePort: PortRange{16384, 32767}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if base.Overlaps(tc.other) != tc.overlap || tc.other.Overlaps(base) != tc.overlap {
				t.Fatal("wrong overlap result")
			}
		})
	}
	a, b := base, base
	a.SourcePort, b.SourcePort = PortRange{100, 200}, PortRange{201, 300}
	if a.Overlaps(b) {
		t.Fatal("disjoint source ports overlap")
	}
}

func TestDisabledTrafficSelectorIsCatchAll(t *testing.T) {
	before := config.OVNKubernetesFeature.EnableEgressTrafficClass
	t.Cleanup(func() { config.OVNKubernetesFeature.EnableEgressTrafficClass = before })
	spec := eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "test"}}}
	config.OVNKubernetesFeature.EnableEgressTrafficClass = false
	if HasTrafficSelector(spec) {
		t.Fatal("disabled feature interpreted selector")
	}
	config.OVNKubernetesFeature.EnableEgressTrafficClass = true
	if !HasTrafficSelector(spec) || HasTrafficSelector(eipv1.EgressIPSpec{}) {
		t.Fatal("absent and populated selectors are not distinguished")
	}
}

func TestParseTrafficSelector(t *testing.T) {
	for _, selector := range []*metav1.LabelSelector{
		nil, {},
		{MatchLabels: map[string]string{"bad key": "one"}},
		{MatchLabels: map[string]string{"class": "bad value"}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpIn}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: metav1.LabelSelectorOpExists, Values: []string{"one"}}}},
	} {
		parsed, err := ParseTrafficSelector(selector)
		if err == nil || parsed.Matches(labels.Set{}) || parsed.Matches(labels.Set{"class": "one"}) {
			t.Fatalf("invalid selector %+v did not fail closed: %v, %v", selector, parsed, err)
		}
	}
	parsed, err := ParseTrafficSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}})
	if err != nil || !parsed.Matches(labels.Set{"class": "one"}) || parsed.Matches(labels.Set{"class": "two"}) {
		t.Fatalf("valid selector lost its match: %v, %v", parsed, err)
	}
}
