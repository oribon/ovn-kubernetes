// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

const selectiveSNATChain = "egress-ip-traffic-snat"

func (c *Controller) destinationMatches(eip *eipv1.EgressIP) ([]egressip.Destination, bool, error) {
	if !egressip.HasTrafficSelector(eip.Spec) {
		return nil, false, nil
	}
	selector, err := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector)
	if err != nil {
		klog.Errorf("EgressIP %s selects no traffic: %v", eip.Name, err)
		return nil, true, nil
	}
	classes, err := c.trafficLister.List(selector)
	if err != nil {
		return nil, true, err
	}
	var raw []trafficv1.Destination
	for _, class := range classes {
		raw = append(raw, class.Spec.Destinations...)
	}
	destinations, err := egressip.ParseDestinations(raw)
	return destinations, true, err
}

func (c *Controller) onTrafficClassChange(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	class, ok := obj.(*trafficv1.EgressTrafficClass)
	if !ok {
		klog.Errorf("Unexpected EgressTrafficClass event %T", obj)
		return
	}
	eips, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list EgressIPs after TrafficClass event: %v", err)
		return
	}
	for _, eip := range eips {
		if !egressip.HasTrafficSelector(eip.Spec) {
			continue
		}
		selector, err := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector)
		if err != nil {
			klog.Errorf("Invalid EgressIP %s trafficSelector: %v", eip.Name, err)
			continue
		}
		if selector.Matches(labels.Set(class.Labels)) {
			c.eIPQueue.Add(eip.Name)
		}
	}
}

func (c *Controller) onTrafficClassUpdate(old, new interface{}) {
	before := old.(*trafficv1.EgressTrafficClass)
	after := new.(*trafficv1.EgressTrafficClass)
	if reflect.DeepEqual(before.Labels, after.Labels) && reflect.DeepEqual(before.Spec, after.Spec) {
		return
	}
	if !reflect.DeepEqual(before.Labels, after.Labels) {
		c.onTrafficClassChange(old)
	}
	c.onTrafficClassChange(new)
}

func replaceDefaultRouteWithDestinations(routes []netlink.Route, destinations []egressip.Destination, ipv6 bool) []netlink.Route {
	var filtered []netlink.Route
	for _, route := range routes {
		if route.Dst != nil {
			ones, _ := route.Dst.Mask.Size()
			if ones != 0 {
				filtered = append(filtered, route)
			}
		}
	}
	for _, prefix := range egressip.UniqueCIDRs(destinations) {
		if prefix.Addr().Is6() != ipv6 {
			continue
		}
		_, network, _ := net.ParseCIDR(prefix.String())
		covering, bestPrefix := longestCoveringRoutes(routes, network)
		if bestPrefix == prefix.Bits() && prefix.Bits() != 0 {
			continue // The exact routes were retained above.
		}
		// Keep all alternatives and their metrics/flags; the kernel selects the nexthop.
		for _, route := range covering {
			route.Dst = network
			filtered = append(filtered, route)
		}
	}
	return filtered
}

// Keep every route tied for the longest covering prefix, including alternate nexthops.
func longestCoveringRoutes(routes []netlink.Route, network *net.IPNet) ([]netlink.Route, int) {
	bestPrefix := -1
	prefixLength, _ := network.Mask.Size()
	var covering []netlink.Route
	for _, route := range routes {
		ones := 0
		if route.Dst != nil {
			ones, _ = route.Dst.Mask.Size()
			if ones > prefixLength || !route.Dst.Contains(network.IP) {
				continue
			}
		}
		if ones > bestPrefix {
			bestPrefix = ones
			covering = nil
		}
		if ones == bestPrefix {
			covering = append(covering, route)
		}
	}
	return covering, bestPrefix
}

// Caller holds configurationMutex, which also serializes every applied-state update.
func (c *Controller) routesForOtherEIPs(name string) []netlink.Route {
	var routes []netlink.Route
	for _, key := range c.cache.GetKeys() {
		if key == name {
			continue
		}
		state, ok := c.cache.Load(key)
		if ok && state.eIPConfig != nil {
			routes = append(routes, state.eIPConfig.routes...)
		}
	}
	return routes
}

// Caller holds configurationMutex; rules are shared independently of SNAT addresses.
func (c *Controller) ipRulesForOtherEIPs(name string) sets.Set[iprulemanager.IPRule] {
	rules := sets.New[iprulemanager.IPRule]()
	for _, key := range c.cache.GetKeys() {
		if key == name {
			continue
		}
		state, ok := c.cache.Load(key)
		if !ok {
			continue
		}
		for _, pods := range state.namespacesWithPodIPConfigs {
			for _, configs := range pods {
				for _, config := range configs.elems {
					rules.Insert(config.ipRule)
				}
			}
		}
	}
	return rules
}

func containsRoute(routes []netlink.Route, candidate netlink.Route) bool {
	for _, route := range routes {
		if util.RouteEqual(&route, &candidate) {
			return true
		}
	}
	return false
}

func (c *Controller) syncSelectiveSNAT() error {
	if !ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass {
		return nil
	}
	var configs []*podIPConfig
	for _, name := range c.cache.GetKeys() {
		state, ok := c.cache.Load(name)
		if !ok {
			continue
		}
		for _, pods := range state.namespacesWithPodIPConfigs {
			for _, pod := range pods {
				configs = append(configs, pod.elems...)
			}
		}
	}
	return c.syncSelectiveSNATRules(configs)
}

func (c *Controller) syncSelectiveSNATRules(configs []*podIPConfig) error {
	if !ovnconfig.OVNKubernetesFeature.EnableEgressTrafficClass {
		return nil
	}
	rules := []string{}
	for _, config := range configs {
		if config.trafficMatch.CIDR.IsValid() {
			rules = append(rules, selectiveSNATRule(config))
		}
	}
	slices.Sort(rules)
	rules = slices.Compact(rules)
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{Name: selectiveSNATChain})
	tx.Flush(&knftables.Chain{Name: selectiveSNATChain})
	for _, rule := range rules {
		tx.Add(&knftables.Rule{Chain: selectiveSNATChain, Rule: rule})
	}
	return nft.Run(context.TODO(), tx)
}

func selectiveSNATRule(config *podIPConfig) string {
	family := "ip"
	if config.v6 {
		family = "ip6"
	}
	d := config.trafficMatch
	parts := []string{family, "saddr", config.snatKey, "oifname", fmt.Sprintf("%q", config.ifName), family, "daddr", d.CIDR.String()}
	if d.HasL4Filter() {
		protocol := d.ProtocolName()
		parts = append(parts, "meta", "l4proto", protocol)
		if d.DestinationPort != (egressip.PortRange{}) {
			parts = append(parts, protocol, "dport", nftPort(d.DestinationPort))
		}
		if d.SourcePort != (egressip.PortRange{}) {
			parts = append(parts, protocol, "sport", nftPort(d.SourcePort))
		}
	}
	parts = append(parts, "snat", family, "to", config.snatValue)
	return strings.Join(parts, " ")
}

func nftPort(ports egressip.PortRange) string {
	if ports.Start == ports.End {
		return fmt.Sprint(ports.Start)
	}
	return fmt.Sprintf("%d-%d", ports.Start, ports.End)
}
