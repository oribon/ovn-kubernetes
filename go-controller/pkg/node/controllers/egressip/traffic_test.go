// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	eipinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	trafficfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1/apis/clientset/versioned/fake"
	trafficinformers "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1/apis/informers/externalversions"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

func TestSelectiveNodeOwnerLifecycle(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(ovnconfig.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = ovnconfig.PrepareTestConfig() })
	ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass = true
	_, clusterCIDR, _ := net.ParseCIDR("10.128.0.0/14")
	ovnconfig.Default.ClusterSubnets = []ovnconfig.CIDRNetworkEntry{{CIDR: clusterCIDR}}
	initial := nodeConfig{linkConfigs: []linkConfig{
		{linkName: "dummy1", address: []address{{cidr: "5.5.5.10/24"}}},
		{linkName: "dummy2", address: []address{{cidr: "6.6.6.10/24"}}},
	}}
	netns, cleanup, err := setupFakeTestNode(initial)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { g.Expect(cleanup()).To(gomega.Succeed()) })
	err = netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName("dummy1")
		if err != nil {
			return err
		}
		if err = netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("5.5.5.254")}); err != nil {
			return err
		}
		secondLink, err := netlink.LinkByName("dummy2")
		if err != nil {
			return err
		}
		if err = netlink.RouteAdd(&netlink.Route{LinkIndex: secondLink.Attrs().Index, Gw: net.ParseIP("6.6.6.254"), Priority: 100}); err != nil {
			return err
		}
		node := getNodeObj(initial, false)
		client := fake.NewSimpleClientset(&node)
		core := informers.NewSharedInformerFactory(client, 0).Core().V1()
		eips := eipinformers.NewSharedInformerFactory(eipfake.NewSimpleClientset(), 0).K8s().V1().EgressIPs()
		classes := trafficinformers.NewSharedInformerFactory(trafficfake.NewSimpleClientset(), 0).K8s().V1().EgressTrafficClasses()
		g.Expect(core.Nodes().Informer().GetStore().Add(&node)).To(gomega.Succeed())
		g.Expect(core.Namespaces().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed())
		g.Expect(core.Pods().Informer().GetStore().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "work"}, Spec: corev1.PodSpec{NodeName: node1Name}, Status: corev1.PodStatus{PodIP: "10.128.0.2", PodIPs: []corev1.PodIP{{IP: "10.128.0.2"}}}})).To(gomega.Succeed())
		one := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: node1Name, EgressIP: "5.5.5.100"}}}}
		two := one.DeepCopy()
		two.Name = "two"
		two.Spec.TrafficSelector.MatchLabels["class"] = "two"
		two.Status.Items[0].EgressIP = "5.5.5.101"
		class := &trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"class": "one"}}, Spec: trafficv1.EgressTrafficClassSpec{Destinations: []trafficv1.Destination{{CIDR: "203.0.113.0/24", Protocols: []trafficv1.ProtocolMatch{{UDP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)}}}}}}}}
		peerClass := class.DeepCopy()
		peerClass.Name = "two"
		peerClass.Labels["class"] = "two"
		peerClass.Spec.Destinations[0].Protocols = []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](22)}}}}
		for _, owner := range []*eipv1.EgressIP{one, two} {
			g.Expect(eips.Informer().GetStore().Add(owner)).To(gomega.Succeed())
		}
		for _, tc := range []*trafficv1.EgressTrafficClass{class, peerClass} {
			g.Expect(classes.Informer().GetStore().Add(tc)).To(gomega.Succeed())
		}
		nft := nodenft.SetFakeNFTablesHelper()
		newController := func() *Controller {
			c, err := NewController(&kube.Kube{KClient: client}, eips, classes, core.Nodes().Informer(), core.Namespaces(), core.Pods(), func(string) (util.NetInfo, error) { return &util.DefaultNetInfo{}, nil }, routemanager.NewController(), true, false, node1Name, linkmanager.NewController(node1Name, true, false, nil))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(c.initNFTables()).To(gomega.Succeed())
			g.Expect(c.ruleManager.OwnPriority(ruleTrafficPriority)).To(gomega.Succeed())
			g.Expect(c.ruleManager.OwnPriority(rulePriority)).To(gomega.Succeed())
			t.Cleanup(func() { c.eIPQueue.ShutDown(); c.podQueue.ShutDown(); c.namespaceQueue.ShutDown() })
			return c
		}
		c := newController()
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRoute := func(targetLink netlink.Link, cidr string, present bool) {
			t.Helper()
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: util.CalculateRouteTableID(targetLink.Attrs().Index)}, netlink.RT_FILTER_TABLE)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			found := false
			for _, route := range routes {
				if route.Dst != nil && route.Dst.String() == cidr {
					found = true
				}
			}
			g.Expect(found).To(gomega.Equal(present), "route %s", cidr)
		}
		checkRoute(link, "203.0.113.0/24", true)
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		checkRule := func(expected iprulemanager.IPRule, present bool) {
			t.Helper()
			rules, err := netlink.RuleList(netlink.FAMILY_V4)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			count := 0
			for _, rule := range rules {
				if iprulemanager.IPRuleFromNetlinkRule(&rule) == expected {
					count++
				}
			}
			if present {
				g.Expect(count).To(gomega.Equal(1), "shared policy rule must survive exactly once")
			} else {
				g.Expect(count).To(gomega.BeZero(), "last owner must remove the policy rule")
			}
		}
		originalProtocols := peerClass.DeepCopy().Spec.Destinations[0].Protocols
		peerClass.Spec.Destinations[0].Protocols = class.DeepCopy().Spec.Destinations[0].Protocols
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		state, _ := c.cache.Load("one")
		sharedRule := state.namespacesWithPodIPConfigs["work"][getPodNamespacedName(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "work", Name: "pod"}})].elems[0].ipRule
		checkRule(sharedRule, true)
		// Resolving an overlap by changing one owner must preserve the other's rule.
		class.Spec.Destinations[0].CIDR = "203.0.114.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRule(sharedRule, true)
		class.Spec.Destinations[0].CIDR = "203.0.113.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		// Deleting one of two identical rules must not remove the surviving owner.
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRule(sharedRule, true)
		peerClass.Spec.Destinations[0].Protocols = originalProtocols
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		g.Expect(c.syncEIP("two")).To(gomega.Succeed())
		checkRule(sharedRule, false)
		g.Expect(eips.Informer().GetStore().Add(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		// An invalid stored selector removes its old datapath without affecting peers.
		invalid := one.DeepCopy()
		invalid.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}
		g.Expect(eips.Informer().GetStore().Update(invalid)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRule(sharedRule, false)
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		g.Expect(c.repairNode()).To(gomega.Succeed())
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		class.Spec.Destinations[0].CIDR = "203.0.114.0/24"
		g.Expect(classes.Informer().GetStore().Update(class)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "203.0.113.0/24", true)
		checkRoute(link, "203.0.114.0/24", true)
		// Moving one owner must preserve the peer's old table, including across a failed API write.
		rejectPatch := true
		client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if rejectPatch {
				rejectPatch = false
				return true, nil, errors.New("temporary annotation failure")
			}
			return false, nil, nil
		})
		one.Status.Items[0].EgressIP = "6.6.6.100"
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).NotTo(gomega.Succeed())
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		netlinkOps := util.GetNetLinkOps()
		defer util.SetNetLinkOpMockInst(netlinkOps)
		util.SetNetLinkOpMockInst(&failingTrafficRouteOps{NetLinkOps: netlinkOps, linkIndex: secondLink.Attrs().Index})
		g.Expect(c.syncEIP("one")).To(gomega.MatchError(gomega.ContainSubstring("injected destination route failure")))
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(secondLink, "6.6.6.0/24", true)
		checkRoute(secondLink, "203.0.114.0/24", false)
		applied, _ := c.cache.Load("one")
		g.Expect(applied.eIPConfig.routes).NotTo(gomega.BeEmpty())
		for _, route := range applied.eIPConfig.routes {
			g.Expect(route.LinkIndex).To(gomega.Equal(secondLink.Attrs().Index))
		}
		// Deletion after a partial move must remove the routes already installed on the new link.
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(secondLink, "6.6.6.0/24", false)
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(link, "203.0.113.0/24", true)
		one.Status.Items[0].EgressIP = "5.5.5.100"
		g.Expect(eips.Informer().GetStore().Add(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		one.Status.Items[0].EgressIP = "6.6.6.100"
		g.Expect(eips.Informer().GetStore().Update(one)).To(gomega.Succeed())
		util.SetNetLinkOpMockInst(&failingTrafficRouteOps{NetLinkOps: netlinkOps, linkIndex: secondLink.Attrs().Index})
		g.Expect(c.syncEIP("one")).To(gomega.MatchError(gomega.ContainSubstring("injected destination route failure")))
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "5.5.5.0/24", true)
		checkRoute(link, "203.0.113.0/24", true)
		checkRoute(link, "203.0.114.0/24", false)
		checkRoute(secondLink, "203.0.114.0/24", true)
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 6.6.6.100"))
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 5.5.5.100"))
		g.Expect(eips.Informer().GetStore().Delete(one)).To(gomega.Succeed())
		g.Expect(c.syncEIP("one")).To(gomega.Succeed())
		checkRoute(link, "203.0.113.0/24", true)
		checkRoute(secondLink, "203.0.114.0/24", false)
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("snat ip to 6.6.6.100"))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("snat ip to 5.5.5.101"))
		updatedNode, err := client.CoreV1().Nodes().Get(context.Background(), node1Name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(core.Nodes().Informer().GetStore().Update(updatedNode)).To(gomega.Succeed())
		// Rebuild destination rules and SNAT after an offline matcher edit.
		peerClass.Spec.Destinations[0].Protocols[0].TCP.DestinationPort.Number = ptr.To[int32](80)
		g.Expect(classes.Informer().GetStore().Update(peerClass)).To(gomega.Succeed())
		restarted := newController()
		g.Expect(restarted.repairNode()).To(gomega.Succeed())
		g.Expect(restarted.syncEIP("two")).To(gomega.Succeed())
		rules, err := netlink.RuleList(netlink.FAMILY_V4)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		var ports []uint16
		for _, rule := range rules {
			if rule.Priority == ruleTrafficPriority && rule.Dport != nil {
				ports = append(ports, rule.Dport.Start)
			}
		}
		g.Expect(ports).To(gomega.Equal([]uint16{80}))
		g.Expect(nft.Dump()).To(gomega.ContainSubstring("tcp dport 80"))
		g.Expect(nft.Dump()).NotTo(gomega.ContainSubstring("tcp dport 22"))
		return nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

type failingTrafficRouteOps struct {
	util.NetLinkOps
	linkIndex int
}

func (ops *failingTrafficRouteOps) RouteReplace(route *netlink.Route) error {
	if route.LinkIndex == ops.linkIndex && route.Dst != nil && route.Dst.String() == "203.0.114.0/24" {
		ops.linkIndex = 0
		return errors.New("injected destination route failure")
	}
	return ops.NetLinkOps.RouteReplace(route)
}

func TestSelectiveSNATAndRules(t *testing.T) {
	before := ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass
	t.Cleanup(func() { ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass = before })
	ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass = true
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth-mgmt", Index: 7}}
	dest := egressip.Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24"), Protocol: egressip.ProtocolUDP,
		DestinationPort: egressip.PortRange{Start: 5060, End: 5062}, SourcePort: egressip.PortRange{Start: 16384, End: 32767}}
	peer := dest
	peer.DestinationPort = egressip.PortRange{Start: 5080, End: 5080}
	configs := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2"), net.ParseIP("fd00::2")}, link, net.ParseIP("198.51.100.1"), false, []egressip.Destination{dest}, true)
	other := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2")}, link, net.ParseIP("198.51.100.2"), false, []egressip.Destination{peer}, true)
	if configs.len() != 1 || other.len() != 1 {
		t.Fatal("wrong address-family filtering")
	}
	if r := configs.elems[0].ipRule; r.Priority != 6000 || r.IPProto != 17 || r.Dst != dest.CIDR || r.Dport.Start != 5060 || r.Sport.End != 32767 {
		t.Fatalf("incorrect steering rule: %v", r)
	}
	fake := nodenft.SetFakeNFTablesHelper()
	c := &Controller{}
	if err := c.syncSelectiveSNATRules(append(configs.elems, other.elems...)); err != nil {
		t.Fatal(err)
	}
	wantOne := `ip saddr 10.128.0.2 oifname "eth-mgmt" ip daddr 192.0.2.0/24 meta l4proto udp udp dport 5060-5062 udp sport 16384-32767 snat ip to 198.51.100.1`
	wantTwo := `ip saddr 10.128.0.2 oifname "eth-mgmt" ip daddr 192.0.2.0/24 meta l4proto udp udp dport 5080 udp sport 16384-32767 snat ip to 198.51.100.2`
	dump := fake.Dump()
	if !strings.Contains(dump, wantOne) || !strings.Contains(dump, wantTwo) {
		t.Fatalf("wrong selective SNAT rules:\n%s", dump)
	}
	if err := c.syncSelectiveSNATRules(other.elems); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fake.Dump(), wantOne) || !strings.Contains(fake.Dump(), wantTwo) {
		t.Fatal("removing one owner affected its peer")
	}
	if n := generatePodConfig([]net.IP{net.ParseIP("10.128.0.2")}, link, net.ParseIP("198.51.100.1"), false, nil, true).len(); n != 0 {
		t.Fatal("unresolved selector installed rules")
	}
}

func TestDestinationRoutesKeepGatewayAndPeers(t *testing.T) {
	_, specific, _ := net.ParseCIDR("203.0.113.0/24")
	routes := []netlink.Route{{LinkIndex: 7, Table: 1007, Gw: net.ParseIP("198.51.100.254")}, {LinkIndex: 7, Table: 1007, Dst: specific, Gw: net.ParseIP("198.51.100.253")}}
	dest := egressip.Destination{CIDR: netip.MustParsePrefix("192.0.2.0/24")}
	got := replaceDefaultRouteWithDestinations(routes, []egressip.Destination{dest, dest, {CIDR: netip.MustParsePrefix("2001:db8::/32")}}, false)
	if len(got) != 2 || got[0].Dst.String() != specific.String() || got[0].Gw.String() != "198.51.100.253" || got[1].Gw.String() != "198.51.100.254" {
		t.Fatalf("routes lost original gateways or deduplication: %+v", got)
	}
	c := &Controller{cache: syncmap.NewSyncMap[*state]()}
	one, two := newState(), newState()
	one.eIPConfig = &eIPConfig{addr: egressip.GetNetlinkAddress(net.ParseIP("198.51.100.1"), 7), routes: got}
	two.eIPConfig = &eIPConfig{addr: egressip.GetNetlinkAddress(net.ParseIP("198.51.100.2"), 7), routes: got[1:]}
	c.cache.Store("one", one)
	c.cache.Store("two", two)
	// An address can move before all of its routes have been reconciled.
	two.eIPConfig.addr.LinkIndex = 8
	if peers := c.routesForOtherEIPs("one"); len(peers) != 1 || !containsRoute(peers, got[1]) || containsRoute(peers, got[0]) {
		t.Fatal("route ownership is not independent")
	}
	movedRoute := got[1]
	movedRoute.LinkIndex, movedRoute.Table = 8, 1008
	if containsRoute(c.routesForOtherEIPs("one"), movedRoute) {
		t.Fatal("route ownership ignored the route's interface and table")
	}
}

func TestSelectiveSNATKernelSyntax(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root and network namespaces")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft tool unavailable")
	}
	netns, err := testutils.NewNS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netns.Close(); _ = testutils.UnmountNS(netns) })
	err = netns.Do(func(ns.NetNS) error {
		nft, err := knftables.New(knftables.InetFamily, "traffic-test")
		if err != nil {
			return err
		}
		tx := nft.NewTransaction()
		tx.Add(&knftables.Table{})
		tx.Add(&knftables.Chain{Name: selectiveSNATChain, Type: knftables.PtrTo(knftables.NATType), Hook: knftables.PtrTo(knftables.PostroutingHook), Priority: knftables.PtrTo(knftables.SNATPriority)})
		for _, family := range []struct {
			src, dst, eip string
			v6            bool
		}{{"10.128.0.2", "192.0.2.0/24", "198.51.100.1", false}, {"fd00::2", "2001:db8::/32", "2001:db8:1::1", true}} {
			for _, protocol := range []int{0, egressip.ProtocolTCP, egressip.ProtocolUDP, egressip.ProtocolSCTP} {
				match := egressip.Destination{CIDR: netip.MustParsePrefix(family.dst), Protocol: protocol}
				if protocol != 0 {
					match.DestinationPort = egressip.PortRange{Start: 80, End: 90}
					match.SourcePort = egressip.PortRange{Start: 5060, End: 5060}
				}
				config := &podIPConfig{trafficMatch: match, snatKey: family.src, snatValue: family.eip, ifName: "eth-mgmt", v6: family.v6}
				tx.Add(&knftables.Rule{Chain: selectiveSNATChain, Rule: selectiveSNATRule(config)})
			}
		}
		if err = nft.Run(context.Background(), tx); err != nil {
			return fmt.Errorf("kernel rejected selective SNAT: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDestinationRoutesPreserveForwarding(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		t.Run(fmt.Sprintf("IPv6=%t", ipv6), func(t *testing.T) {
			prefix, covering, gateway, backup := "192.0.2.0/24", "192.0.0.0/16", "198.51.100.1", "198.51.100.2"
			if ipv6 {
				prefix, covering, gateway, backup = "2001:db8:1::/64", "2001:db8::/32", "2001:db8:ffff::1", "2001:db8:ffff::2"
			}
			_, dst, _ := net.ParseCIDR(prefix)
			_, parent, _ := net.ParseCIDR(covering)
			primary := netlink.Route{LinkIndex: 7, Table: 1007, Gw: net.ParseIP(gateway), Priority: 10, MTU: 1400, Flags: int(netlink.FLAG_ONLINK)}
			secondary := primary
			secondary.Gw, secondary.Priority = net.ParseIP(backup), 100
			destinations := []egressip.Destination{{CIDR: netip.MustParsePrefix(prefix)}}
			for _, routes := range [][]netlink.Route{{primary, secondary}, {secondary, primary}} {
				want := append([]netlink.Route(nil), routes...)
				for i := range want {
					want[i].Dst = dst
				}
				if got := replaceDefaultRouteWithDestinations(routes, destinations, ipv6); !reflect.DeepEqual(got, want) {
					t.Fatalf("lost default alternatives/attributes: got %+v, want %+v", got, want)
				}
			}
			// A more-specific destination must continue using the covering administrator route.
			specific := secondary
			specific.Dst = parent
			want := specific
			want.Dst = dst
			got := replaceDefaultRouteWithDestinations([]netlink.Route{primary, specific}, destinations, ipv6)
			if !reflect.DeepEqual(got, []netlink.Route{specific, want}) {
				t.Fatalf("selected destination bypassed covering route: %+v", got)
			}
			// An exact destination already has all its alternatives; do not synthesize duplicates.
			primary.Dst, secondary.Dst = dst, dst
			got = replaceDefaultRouteWithDestinations([]netlink.Route{primary, secondary}, destinations, ipv6)
			if !reflect.DeepEqual(got, []netlink.Route{primary, secondary}) {
				t.Fatalf("changed exact routes: %+v", got)
			}
		})
	}
}
