// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTrafficOwnerSelection(t *testing.T) {
	x := Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24")}
	y := Destination{CIDR: netip.MustParsePrefix("198.51.100.0/24")}
	candidate := func(name string, age int64, matches ...Destination) TrafficCandidate {
		return TrafficCandidate{EgressIP: &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(age, 0))}}, Destinations: matches}
	}
	candidates := []TrafficCandidate{candidate("a", 1, x), candidate("b", 2, x, y), candidate("c", 3, y)}
	got := SelectTrafficOwners(candidates)
	if got["a"] != nil || got["b"] == nil || got["b"].Owner != "a" || got["c"] != nil {
		t.Fatalf("rejected owner blocked C: %+v", got)
	}
	slices.Reverse(candidates)
	if reversed := SelectTrafficOwners(candidates); !reflect.DeepEqual(got, reversed) {
		t.Fatalf("event order changed winners: %+v", reversed)
	}
	candidates = []TrafficCandidate{candidate("a", 1), candidate("b", 2, x, y), candidate("c", 3, y)}
	got = SelectTrafficOwners(candidates)
	if got["b"] != nil || got["c"] == nil || got["c"].Owner != "b" {
		t.Fatalf("previously active C kept preference: %+v", got)
	}
	got = SelectTrafficOwners([]TrafficCandidate{candidate("z", 1, x), candidate("a", 1, x)})
	if got["a"] != nil || got["z"] == nil || got["z"].Owner != "a" {
		t.Fatal("name tiebreaker lost")
	}
	got = SelectTrafficOwners([]TrafficCandidate{candidate("z", 1, x), candidate("a", 2, x)})
	if got["z"] != nil || got["a"] == nil {
		t.Fatal("name overrode timestamp")
	}
}
