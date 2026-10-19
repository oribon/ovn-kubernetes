// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSelectiveEgressReconciliation(t *testing.T) {
	for _, familyMode := range []string{"IPv4", "IPv6", "dual-stack"} {
		ipv6, dualStack := familyMode == "IPv6", familyMode == "dual-stack"
		for _, topologyCase := range []string{"default", types.Layer2Topology, "layer2-transit", types.Layer3Topology} {
			t.Run(fmt.Sprintf("%s/%s", topologyCase, familyMode), func(t *testing.T) {
				topology := topologyCase
				if topology == "layer2-transit" {
					topology = types.Layer2Topology
				}
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
				t.Cleanup(func() { _ = config.PrepareTestConfig() })
				config.IPv4Mode, config.IPv6Mode = !ipv6, ipv6 || dualStack
				config.OVNKubernetesFeature.EnableEgressIP = true
				config.OVNKubernetesFeature.EnableEgressTrafficClass = true
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
				config.Layer2UsesTransitRouter = topologyCase == "layer2-transit"
				podPrefix, podMask, primarySubnet := "10.128.0.", "/24", "20.128.0.0/16"
				externalPrefix, externalMask, joinIP, gatewayIP := "198.51.100.", "/24", "100.64.0.2/16", "198.51.100.254"
				cidr, l4CIDR, family := "203.0.113.0/24", "192.0.2.0/24", "ip4"
				if ipv6 {
					podPrefix, podMask, primarySubnet = "fd10::", "/64", "fd20::/48"
					externalPrefix, externalMask, joinIP, gatewayIP = "2001:db8:1::", "/64", "fd98::2/64", "2001:db8:1::fe"
					cidr, l4CIDR, family = "2001:db8:100::/64", "2001:db8:101::/64", "ip6"
				}
				if dualStack {
					primarySubnet += ",fd20::/48"
				}
				var ni util.NetInfo = &util.DefaultNetInfo{}
				nadKey := types.DefaultNetworkName
				if topology != "default" {
					nad := &nadapi.NetworkAttachmentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "blue", Namespace: "work"}, Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: fmt.Sprintf(`{"cniVersion":"0.4.0","type":"ovn-k8s-cni-overlay","name":"blue","netAttachDefName":"work/blue","topology":%q,"role":"primary","subnets":%q}`, topology, primarySubnet)}}
					var err error
					ni, err = util.ParseNADInfo(nad)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					nadKey = "work/blue"
				}
				if ni.IsUserDefinedNetwork() {
					podPrefix = "20.128.0."
					if ipv6 {
						podPrefix = "fd20::"
					}
				}
				pod := ovntest.NewPodWithLabels("work", "pod", "node", podPrefix+"10", map[string]string{"app": "test"})
				ips := ovntest.MustParseIPNets(podPrefix + "10" + podMask)
				if dualStack {
					v6Pod := "fd10::10/64"
					if ni.IsUserDefinedNetwork() {
						v6Pod = "fd20::10/64"
					}
					ips = append(ips, ovntest.MustParseIPNets(v6Pod)...)
				}

				mac, _ := net.ParseMAC("0a:58:0a:80:00:0a")
				var err error
				pod.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{IPs: ips, MAC: mac, Role: types.NetworkRolePrimary}, nadKey)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{
					"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf(`{%q:%q}`, map[bool]string{false: "ipv4", true: "ipv6"}[ipv6], externalPrefix+"2"+externalMask),
					util.OVNNodeHostCIDRs:             fmt.Sprintf(`[%q]`, externalPrefix+"2"+externalMask),
					"k8s.ovn.org/node-chassis-id":     "chassis",
					"k8s.ovn.org/node-id":             "1",
					util.OvnNodeL3GatewayConfig:       fmt.Sprintf(`{"default":{"mode":"shared","interface-id":"node","mac-address":"0a:58:c6:33:64:02","ip-addresses":[%q],"next-hops":[%q],"node-port-enable":"true"}}`, externalPrefix+"2"+externalMask, gatewayIP),
				}}}
				if dualStack {
					node.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"198.51.100.2/24","ipv6":"2001:db8:1::2/64"}`
					node.Annotations[util.OVNNodeHostCIDRs] = `["198.51.100.2/24","2001:db8:1::2/64"]`
					node.Annotations[util.OvnNodeL3GatewayConfig] = `{"default":{"mode":"shared","interface-id":"node","mac-address":"0a:58:c6:33:64:02","ip-addresses":["198.51.100.2/24","2001:db8:1::2/64"],"next-hops":["198.51.100.254","2001:db8:1::fe"],"node-port-enable":"true"}}`
				}
				one := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{util.EgressIPMarkAnnotation: "50001"}}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: externalPrefix + "10"}}}}
				two := one.DeepCopy()
				two.Name = "two"
				two.Annotations[util.EgressIPMarkAnnotation] = "50002"
				two.Spec.TrafficSelector.MatchLabels["traffic"] = "two"
				two.Status.Items[0].EgressIP = externalPrefix + "11"
				fallback := one.DeepCopy()
				fallback.Name = "fallback"
				fallback.Annotations[util.EgressIPMarkAnnotation] = "50003"
				fallback.Spec.TrafficSelector = nil
				fallback.Status.Items[0].EgressIP = externalPrefix + "12"
				class := &trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"traffic": "one"}}, Spec: trafficv1.EgressTrafficClassSpec{Destinations: []trafficv1.Destination{
					{CIDR: cidr},
					{CIDR: l4CIDR, Protocols: []trafficv1.ProtocolMatch{{UDP: &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}}}}}},
				}}}
				peerClass := class.DeepCopy()
				peerClass.Name = "two"
				peerClass.Labels["traffic"] = "two"
				peerClass.Spec.Destinations = []trafficv1.Destination{{CIDR: l4CIDR, Protocols: []trafficv1.ProtocolMatch{{TCP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](22)}}}}}}
				if dualStack {
					two.Status.Items[0].EgressIP = "2001:db8:1::11"
					peerClass.Spec.Destinations[0].CIDR = "2001:db8:101::/64"
					class.Spec.Destinations = append(class.Spec.Destinations, trafficv1.Destination{CIDR: "2001:db8:100::/64"})
				}
				clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
				wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(wf.Stop)
				for _, eip := range []*eipv1.EgressIP{one, two, fallback} {
					g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(eip)).To(gomega.Succeed())
				}
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Add(peerClass)).To(gomega.Succeed())
				g.Expect(wf.PodCoreInformer().Informer().GetStore().Add(pod)).To(gomega.Succeed())
				g.Expect(wf.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}})).To(gomega.Succeed())
				g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(node)).To(gomega.Succeed())
				router, _ := getTopologyScopedRouterName(ni, "node")
				gateway := ni.GetNetworkScopedGWRouterName("node")
				joinNetworks := []string{joinIP}
				if dualStack {
					joinNetworks = append(joinNetworks, "fd98::2/64")
				}
				nbData := []libovsdbtest.TestData{&nbdb.LogicalRouter{Name: gateway, UUID: "gw", Ports: []string{"rtoj"}}, &nbdb.LogicalRouterPort{UUID: "rtoj", Name: types.GWRouterToJoinSwitchPrefix + gateway, Networks: joinNetworks}}
				if router != gateway {
					nbData = append(nbData, &nbdb.LogicalRouter{Name: router, UUID: "cr"})
				}
				nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: nbData}, nil)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(cleanup.Cleanup)
				asf := addressset.NewOvnAddressSetFactory(nb, !ipv6, ipv6 || dualStack)
				_, err = asf.EnsureAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), types.DefaultNetworkControllerName))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				pc := NewPortCache(make(chan struct{}))
				pc.add(pod, "", nadKey, "", mac, ips)
				nm := &networkmanager.FakeNetworkManager{PrimaryNetworks: map[string]util.NetInfo{"work": ni}, NADNetworks: map[string]util.NetInfo{nadKey: ni}}
				e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), pc, nm, asf, nil, !ipv6, ipv6 || dualStack, "node", types.DefaultNetworkControllerName)
				g.Expect(e.SyncNodeCache()).To(gomega.Succeed())
				restartNetwork := func() {
					t.Helper()
					e.UnregisterNetwork(ni.GetNetworkName())
					e.ensureNetworkPending(ni.GetNetworkName())
					g.Expect(e.SyncNetworkReady(ni.GetNetworkName())).To(gomega.Succeed())
				}
				restartNetwork()
				for _, owner := range []*eipv1.EgressIP{fallback, one, two} {
					g.Expect(e.reconcileEgressIP(nil, owner)).To(gomega.Succeed())
				}
				assertGatewayMarks := func(expected int) {
					t.Helper()
					marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, gateway, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPMarkPriority(p.Priority) })
					g.Expect(err).NotTo(gomega.HaveOccurred())
					if topology == "default" || topology == types.Layer2Topology && !config.Layer2UsesTransitRouter {
						g.Expect(marks).To(gomega.BeEmpty())
						return
					}
					g.Expect(marks).To(gomega.HaveLen(expected))
					for _, mark := range marks {
						g.Expect(mark.Match).To(gomega.ContainSubstring("pkt.mark == 0"))
						name, _ := getEIPLRPObjK8MetaData(mark.ExternalIDs)
						match, err := e.egressTrafficMatch(name, strings.HasPrefix(mark.Match, "ip6"))
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(mark.Priority).To(gomega.Equal(egressIPMarkPriority(match)))
						g.Expect(mark.Match).To(gomega.HaveSuffix(match))
					}
				}
				assertOwners := func(expected int) {
					t.Helper()
					policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(policies).To(gomega.HaveLen(expected))
					for _, policy := range policies {
						name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
						if name == "fallback" {
							g.Expect(policy.Priority).To(gomega.Equal(99))
							g.Expect(policy.Match).To(gomega.Equal(family + ".src == " + ips[0].IP.String()))
						} else {
							g.Expect(policy.Priority).To(gomega.Equal(100))
							g.Expect(policy.Match).To(gomega.ContainSubstring(" && ("))
						}
						if ni.IsUserDefinedNetwork() {
							g.Expect(policy.Options["pkt_mark"]).NotTo(gomega.BeEmpty())
						}
					}
					state, _ := e.podAssignment.Load(getPodKey(pod))
					g.Expect(state.activeEgressIPs).To(gomega.HaveLen(expected))
					assertGatewayMarks(expected)
				}
				assertOwners(3)
				for _, owner := range []*eipv1.EgressIP{one, two} {
					as, err := asf.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(owner.Name, e.controllerName))
					g.Expect(err).NotTo(gomega.HaveOccurred())
					v4Name, v6Name := as.GetASHashNames()
					ownerFamily, ownerPodIP, ownerL4CIDR := family, ips[0].IP.String(), l4CIDR
					ownerIPv6 := ipv6 || dualStack && owner.Name == two.Name
					if dualStack && owner.Name == two.Name {
						ownerFamily, ownerPodIP, ownerL4CIDR = "ip6", ips[1].IP.String(), "2001:db8:101::/64"
					}
					asName := v4Name
					if ownerIPv6 {
						asName = v6Name
					}
					l4 := "udp && (udp.src >= 16384 && udp.src <= 32767)"
					if owner.Name == two.Name {
						l4 = "tcp && tcp.dst == 22"
					}
					want := fmt.Sprintf("(%s.dst == $%s || (%s.dst == %s && %s))", ownerFamily, asName, ownerFamily, ownerL4CIDR, l4)
					policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool {
						name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
						return name == owner.Name && isEgressIPReroutePriority(p.Priority)
					})
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(policies).To(gomega.HaveLen(1))
					g.Expect(policies[0].Match).To(gomega.Equal(ownerFamily + ".src == " + ownerPodIP + " && " + want))
					if ni.IsUserDefinedNetwork() && (topology == types.Layer3Topology || config.Layer2UsesTransitRouter) {
						marks, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, gateway, func(p *nbdb.LogicalRouterPolicy) bool {
							name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
							return name == owner.Name && isEgressIPMarkPriority(p.Priority)
						})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(marks).To(gomega.HaveLen(1))
						g.Expect(marks[0].Match).To(gomega.Equal(ownerFamily + ".src == " + ownerPodIP + " && pkt.mark == 0 && " + want))
						g.Expect(marks[0].Priority).To(gomega.Equal(96))
						g.Expect(marks[0].Options["pkt_mark"]).To(gomega.Equal(owner.Annotations[util.EgressIPMarkAnnotation]))
					}
					if ni.IsDefault() {
						nats, err := libovsdbops.FindNATsWithPredicate(nb, func(n *nbdb.NAT) bool {
							name, _ := getEIPLRPObjK8MetaData(n.ExternalIDs)
							return name == owner.Name
						})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(nats).To(gomega.HaveLen(1))
						g.Expect(nats[0].Match).To(gomega.Equal(want))
						g.Expect(nats[0].Priority).To(gomega.Equal(100))
					}
				}
				if ni.IsDefault() {
					nats, err := libovsdbops.FindNATsWithPredicate(nb, func(n *nbdb.NAT) bool { return n.Type == nbdb.NATTypeSNAT })
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(nats).To(gomega.HaveLen(3))
					for _, nat := range nats {
						name, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
						if name != "fallback" {
							g.Expect(nat.Match).NotTo(gomega.BeEmpty())
							g.Expect(nat.Priority).To(gomega.BeNumerically(">", 0))
						}
					}
				}
				// Invalid legacy selectors clear both CIDR and L4 matches, including at startup.
				for _, selector := range []*metav1.LabelSelector{
					{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}, {},
				} {
					invalid := one.DeepCopy()
					invalid.Spec.TrafficSelector = selector
					g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(invalid)).To(gomega.Succeed())
					g.Expect(e.reconcileEgressIP(one, invalid)).To(gomega.Succeed())
					restartNetwork()
					g.Expect(e.reconcileEgressTrafficClass(class)).To(gomega.Succeed())
					as, err := asf.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(one.Name, e.controllerName))
					g.Expect(err).NotTo(gomega.HaveOccurred())
					v4, v6 := as.GetAddresses()
					g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
					v4Name, v6Name := as.GetASHashNames()
					asName := v4Name
					if ipv6 {
						asName = v6Name
					}
					match, err := e.egressTrafficMatch(one.Name, ipv6)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(match).To(gomega.Equal(fmt.Sprintf("(%s.dst == $%s)", family, asName)))
					assertOwners(3)
					g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(one)).To(gomega.Succeed())
					g.Expect(e.reconcileEgressIP(invalid, one)).To(gomega.Succeed())
				}
				if dualStack {
					restartNetwork()
					assertOwners(3)
					// Change the actual assignment family while both pod families are present.
					updated := one.DeepCopy()
					updated.Status.Items[0].EgressIP = "2001:db8:1::10"
					g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
					g.Expect(e.reconcileEgressIP(one, updated)).To(gomega.Succeed())
					policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool {
						name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
						return name == one.Name && isEgressIPReroutePriority(p.Priority)
					})
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(policies).To(gomega.HaveLen(1))
					g.Expect(policies[0].Match).To(gomega.HavePrefix("ip6.src == " + ips[1].IP.String() + " && "))
					assertOwners(3)
					if ni.IsDefault() {
						// Reassign through the real EgressIP handler to a remote egress node.
						remote := node.DeepCopy()
						remote.Name = "remote"
						remote.Annotations["k8s.ovn.org/node-transit-switch-port-ifaddr"] = `{"ipv4":"100.88.0.3/16","ipv6":"fd97::3/64"}`
						g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(remote)).To(gomega.Succeed())
						g.Expect(e.SyncNodeCache()).To(gomega.Succeed())
						moved := updated.DeepCopy()
						moved.Status.Items[0].Node = remote.Name
						g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(moved)).To(gomega.Succeed())
						g.Expect(e.reconcileEgressIP(updated, moved)).To(gomega.Succeed())
						policies, err = libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool {
							name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
							return name == one.Name && isEgressIPReroutePriority(p.Priority)
						})
						g.Expect(err).NotTo(gomega.HaveOccurred())
						g.Expect(policies).To(gomega.HaveLen(1))
						g.Expect(policies[0].Nexthops).To(gomega.Equal([]string{"fd97::3"}))
						assertOwners(3)
					}
					return
				}
				// L4 edits rebuild the owner; a peer's assignment survives.
				class.Spec.Destinations[1].Protocols[0].UDP.DestinationPort = &trafficv1.Port{Number: ptr.To[int32](5060)}
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Update(class)).To(gomega.Succeed())
				e.addressSetFactory = &failingTrafficAddressSets{AddressSetFactory: asf, failAdd: true}
				g.Expect(e.reconcileEgressTrafficClass(class)).To(gomega.MatchError(gomega.ContainSubstring("injected served-pod address-set failure")))
				g.Expect(e.reconcileEgressTrafficClass(class)).To(gomega.Succeed())
				e.addressSetFactory = asf
				assertOwners(3)
				policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool {
					return strings.HasPrefix(p.ExternalIDs[libovsdbops.ObjectNameKey.String()], "one_")
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(policies[0].Match).To(gomega.ContainSubstring("udp.dst == 5060"))
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Delete(class)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressTrafficClass(class)).To(gomega.Succeed())
				assertOwners(3)
				empty, err := asf.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(one.Name, e.controllerName))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				v4, v6 := empty.GetAddresses()
				g.Expect(append(v4, v6...)).To(gomega.BeEmpty())
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressTrafficClass(class)).To(gomega.Succeed())
				catchAll := one.DeepCopy()
				catchAll.Spec.TrafficSelector = nil
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(catchAll)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(one, catchAll)).To(gomega.Succeed())
				assertOwners(2)
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(one)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(catchAll, one)).To(gomega.Succeed())
				assertOwners(3)
				// Rebuild all owners after an IP replacement.
				ips = ovntest.MustParseIPNets(podPrefix + "20" + podMask)
				pod.Annotations, err = util.MarshalPodAnnotation(nil, &util.PodAnnotation{IPs: ips, MAC: mac, Role: types.NetworkRolePrimary}, nadKey)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				pod.Status.PodIP = ips[0].IP.String()
				pod.Status.PodIPs = []corev1.PodIP{{IP: pod.Status.PodIP}}
				g.Expect(wf.PodCoreInformer().Informer().GetStore().Update(pod)).To(gomega.Succeed())
				pc.add(pod, "", nadKey, "", mac, ips)
				for _, owner := range []*eipv1.EgressIP{fallback, one, two} {
					g.Expect(e.addPodEgressIPAssignmentsWithLock(ni, owner.Name, owner.Status.Items, getEgressIPPktMark(owner.Name, owner.Annotations), pod)).To(gomega.Succeed())
				}
				assertOwners(3)
				// A cache restart must reconstruct each persisted owner, including legacy priority 100.
				legacyPolicies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool {
					name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
					return name == "fallback"
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(legacyPolicies).To(gomega.HaveLen(1))
				legacy := *legacyPolicies[0]
				legacy.UUID, legacy.Priority = "", types.EgressIPTrafficReroutePriority
				legacyIDs, err := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, legacy.ExternalIDs)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				legacyIDs = legacyIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: "100"})
				legacy.ExternalIDs = legacyIDs.GetExternalIDs()
				ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nb, nil, router, &legacy, libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](legacyIDs, nil))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = libovsdbops.TransactAndCheck(nb, ops)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				e.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
				g.Expect(e.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
				cache, err := e.generateCacheForEgressIP(egressIPAllNetworks)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				// Repair must recreate a missing mark even when all remaining marks are valid.
				g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb, gateway, func(p *nbdb.LogicalRouterPolicy) bool {
					name, _ := getEIPLRPObjK8MetaData(p.ExternalIDs)
					return isEgressIPMarkPriority(p.Priority) && name == one.Name
				})).To(gomega.Succeed())
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				restartNetwork()
				assertOwners(3)
				// A peer's local assignment must not keep another owner's gateway mark alive.
				cache.egressIPNameToAssignedNodes[one.Name] = []string{"other-node"}
				cache.egressIPToAssignedNodes[one.Name] = map[string]string{one.Status.Items[0].EgressIP: "other-node"}
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				assertGatewayMarks(2)
				cache, err = e.generateCacheForEgressIP(egressIPAllNetworks)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				assertGatewayMarks(3)
				otherFamily := "2001:db8::10"
				if ipv6 {
					otherFamily = "198.51.100.10"
				}
				cache.egressIPToAssignedNodes[one.Name] = map[string]string{otherFamily: "node"}
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				assertGatewayMarks(2)
				cache, err = e.generateCacheForEgressIP(egressIPAllNetworks)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(e.syncStaleGWMarkRules(cache, egressIPAllNetworks)).To(gomega.Succeed())
				assertGatewayMarks(3)
				g.Expect(e.reconcileEgressIP(one, nil)).To(gomega.Succeed())
				assertOwners(2)
				// With the gate off, persisted selective peers regain ordinary catch-all/standby semantics.
				config.OVNKubernetesFeature.EnableEgressTrafficClass = false
				g.Expect(e.syncEgressTraffic()).To(gomega.Succeed())
				g.Expect(e.migrateEgressIPReroutePriorities(egressIPAllNetworks)).To(gomega.Succeed())
				g.Expect(e.addPodEgressIPAssignmentsWithLock(ni, fallback.Name, fallback.Status.Items, getEgressIPPktMark(fallback.Name, fallback.Annotations), pod)).To(gomega.Succeed())
				g.Expect(e.addPodEgressIPAssignmentsWithLock(ni, two.Name, two.Status.Items, getEgressIPPktMark(two.Name, two.Annotations), pod)).To(gomega.Succeed())
				state, _ := e.podAssignment.Load(getPodKey(pod))
				g.Expect(state.activeEgressIPs).To(gomega.HaveLen(1))
				g.Expect(state.standbyEgressIPNames).To(gomega.HaveLen(1))
				remaining, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(remaining).To(gomega.HaveLen(1))
				g.Expect(remaining[0].Priority).To(gomega.Equal(99))
				g.Expect(remaining[0].Match).To(gomega.Equal(family + ".src == " + ips[0].IP.String()))
				// Restart reconstruction must include every namespace sharing a network.
				peerPod := pod.DeepCopy()
				peerPod.Namespace = "peer"
				nm.PrimaryNetworks[peerPod.Namespace] = ni
				g.Expect(wf.NamespaceCoreInformer().Informer().GetStore().Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: peerPod.Namespace}})).To(gomega.Succeed())
				g.Expect(wf.PodCoreInformer().Informer().GetStore().Add(peerPod)).To(gomega.Succeed())
				pc.add(peerPod, "", nadKey, "", mac, ips)
				cache, err = e.generateCacheForEgressIP(egressIPAllNetworks)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(cache.egressIPNameToPods[two.Name][ni.GetNetworkName()].egressLocalPods).To(gomega.HaveLen(2))
				// Full startup repair must clear a network even when every owner selects no namespaces.
				owners, err := wf.GetEgressIPs()
				g.Expect(err).NotTo(gomega.HaveOccurred())
				for _, owner := range owners {
					updated := owner.DeepCopy()
					updated.Spec.NamespaceSelector = metav1.LabelSelector{MatchLabels: map[string]string{"missing": "namespace"}}
					g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
				}
				e.podAssignment = syncmap.NewSyncMap[*podAssignmentState]()
				restartNetwork()
				stale, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nb, router, func(p *nbdb.LogicalRouterPolicy) bool { return isEgressIPReroutePriority(p.Priority) })
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(stale).To(gomega.BeEmpty())
				served, err := asf.GetAddressSet(getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), e.controllerName))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				servedV4, servedV6 := served.GetAddresses()
				g.Expect(append(servedV4, servedV6...)).To(gomega.BeEmpty())
			})
		}
	}
}

// Block an address-set operation after the class handler has listed all EgressIPs.
type blockingTrafficAddressSets struct {
	addressset.AddressSetFactory
	first   atomic.Bool
	entered chan string
	release chan struct{}
}

func (f *blockingTrafficAddressSets) NewAddressSet(ids *libovsdbops.DbObjectIDs, addresses []string) (addressset.AddressSet, error) {
	if f.first.CompareAndSwap(false, true) {
		f.entered <- strings.TrimPrefix(ids.GetObjectID(libovsdbops.ObjectNameKey), egressIPDestinationNetworksPrefix)
		<-f.release
	}
	return f.AddressSetFactory.NewAddressSet(ids, addresses)
}

func TestSelectiveTrafficClassConcurrentEgressIPUpdate(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableEgressTrafficClass = true
			clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Stop)
			owners := map[string]*eipv1.EgressIP{}
			for _, name := range []string{"one", "two"} {
				owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "old"}}}}
				owners[name] = owner
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed())
			}
			class := &trafficv1.EgressTrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{"traffic": "old"}}, Spec: trafficv1.EgressTrafficClassSpec{Destinations: []trafficv1.Destination{{CIDR: "192.0.2.0/24"}}}}
			newClass := class.DeepCopy()
			newClass.Name, newClass.Labels = "new", map[string]string{"traffic": "new"}
			newClass.Spec.Destinations[0].CIDR = "203.0.113.0/24"
			for _, class := range []*trafficv1.EgressTrafficClass{class, newClass} {
				g.Expect(wf.EgressTrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
			}
			nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			asf := &blockingTrafficAddressSets{AddressSetFactory: addressset.NewOvnAddressSetFactory(nb, true, false), entered: make(chan string, 1), release: make(chan struct{})}
			e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, false, "node", types.DefaultNetworkControllerName)
			var release sync.Once
			var handlers sync.WaitGroup
			t.Cleanup(func() {
				release.Do(func() { close(asf.release) })
				handlers.Wait()
			})
			done := make(chan error, 1)
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				done <- e.reconcileEgressTrafficClass(class)
			}()
			var blocked string
			g.Eventually(asf.entered).Should(gomega.Receive(&blocked))
			name := "one"
			if blocked == name {
				name = "two"
			}
			old := owners[name]
			if deleted {
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Delete(old)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(old, nil)).To(gomega.Succeed())
			} else {
				updated := old.DeepCopy()
				updated.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: newClass.Labels}
				g.Expect(wf.EgressIPInformer().Informer().GetStore().Update(updated)).To(gomega.Succeed())
				g.Expect(e.reconcileEgressIP(old, updated)).To(gomega.Succeed())
			}
			release.Do(func() { close(asf.release) })
			g.Eventually(done).Should(gomega.Receive(gomega.Succeed()))
			as, err := asf.GetAddressSet(getEgressIPDestNetworksAddrSetDbIDs(name, e.controllerName))
			if deleted {
				g.Expect(err).To(gomega.HaveOccurred(), "deleted owner was recreated by stale class work")
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
				v4, _ := as.GetAddresses()
				g.Expect(v4).To(gomega.Equal([]string{"203.0.113.0/24"}))
			}
		})
	}
}

type failingTrafficAddressSets struct {
	addressset.AddressSetFactory
	failAdd bool
}

func (f *failingTrafficAddressSets) GetAddressSet(ids *libovsdbops.DbObjectIDs) (addressset.AddressSet, error) {
	as, err := f.AddressSetFactory.GetAddressSet(ids)
	if err != nil {
		return nil, err
	}
	return &failingTrafficAddressSet{AddressSet: as, factory: f}, nil
}

type failingTrafficAddressSet struct {
	addressset.AddressSet
	factory *failingTrafficAddressSets
}

func (as *failingTrafficAddressSet) AddAddresses(addresses []string) error {
	if as.factory.failAdd {
		as.factory.failAdd = false
		return fmt.Errorf("injected served-pod address-set failure")
	}
	return as.AddressSet.AddAddresses(addresses)
}

// Per-network startup must not prune destination sets still used by a pending network.
func TestPruneEgressTrafficPreservesReferencedSets(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEgressTrafficClass = true
	clients := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients, "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{&nbdb.LogicalRouter{Name: "pending-network"}},
	}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	asf := addressset.NewOvnAddressSetFactory(nb, true, false)
	ids := getEgressIPDestNetworksAddrSetDbIDs("deleted-owner", types.DefaultNetworkControllerName)
	as, err := asf.NewAddressSet(ids, []string{"203.0.113.0/24"})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	v4, _ := as.GetASHashNames()
	policy := &nbdb.LogicalRouterPolicy{Priority: 100, Match: "ip4.dst == $" + v4, Action: nbdb.LogicalRouterPolicyActionAllow}
	ops, err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nb, nil, "pending-network", policy, func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = libovsdbops.TransactAndCheck(nb, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	e := NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, record.NewFakeRecorder(10), nil, &networkmanager.FakeNetworkManager{}, asf, nil, true, false, "node", types.DefaultNetworkControllerName)
	g.Expect(e.pruneEgressTraffic()).To(gomega.Succeed())
	sets, err := libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(sets).To(gomega.HaveLen(1))
	g.Expect(libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nb, "pending-network", func(p *nbdb.LogicalRouterPolicy) bool {
		return p.Match == policy.Match
	})).To(gomega.Succeed())
	g.Expect(e.pruneEgressTraffic()).To(gomega.Succeed())
	sets, err = libovsdbops.FindAddressSetsWithPredicate(nb, func(a *nbdb.AddressSet) bool { return a.Name == v4 })
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(sets).To(gomega.BeEmpty())
}
