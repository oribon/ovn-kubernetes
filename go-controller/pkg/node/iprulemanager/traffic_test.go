// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestTrafficRuleRoundTripAndIdentity(t *testing.T) {
	for _, family := range []struct {
		src, dst string
		family   int
	}{
		{"10.128.0.2/32", "192.0.2.0/24", netlink.FAMILY_V4},
		{"fd00::2/128", "2001:db8::/32", netlink.FAMILY_V6},
	} {
		rule := IPRule{Priority: 6000, Table: 1007, Family: family.family, Src: netip.MustParsePrefix(family.src), Dst: netip.MustParsePrefix(family.dst), IPProto: 17,
			Dport: netlink.RulePortRange{Start: 5060, End: 5062}, Sport: netlink.RulePortRange{Start: 16384, End: 32767}}
		if got := IPRuleFromNetlinkRule(rule.toNetlinkRule()); got != rule {
			t.Fatalf("roundtrip = %v, want %v", got, rule)
		}
		rules := map[IPRule]bool{rule: true}
		peer := rule
		peer.Dport.Start++
		rules[peer] = true
		peer = rule
		peer.Sport.End--
		rules[peer] = true
		peer = rule
		peer.IPProto = 6
		rules[peer] = true
		if len(rules) != 4 {
			t.Fatal("independent L4 rules share an identity")
		}
	}
	if rule := (IPRule{}).toNetlinkRule(); rule.Dport != nil || rule.Sport != nil {
		t.Fatal("wildcard ports became explicit port zero")
	}
}
