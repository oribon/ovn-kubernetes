// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

const selectiveSNATChain = "egress-ip-snat-filtered"
const legacySelectiveSNATChain = "egress-ip-traffic-snat"

func (c *Controller) destinationMatches(eip *eipv1.EgressIP) ([]egressip.Destination, bool, error) {
	if !egressip.HasTrafficSelector(eip.Spec) {
		return nil, false, nil
	}
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return nil, true, nil
	}
	classes, err := c.trafficLister.List(labels.Everything())
	if err != nil {
		return nil, true, err
	}
	resolution, err := egressip.ResolveTraffic(eip, classes)
	if err != nil {
		if _, selectorErr := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector); selectorErr != nil {
			klog.Errorf("EgressIP %s selects no traffic: %v", eip.Name, err)
			return nil, true, nil
		}
		return nil, true, err
	}
	for _, unsupported := range resolution.Unsupported {
		klog.Warningf("EgressIP %s: TrafficClass %s match %d protocol %d is unsupported", eip.Name, unsupported.Class, unsupported.Match, unsupported.Protocol)
	}
	return resolution.Destinations, true, nil
}

// A class change can reject a winner or promote a previously rejected peer.
// Queue every selective owner, including owners that no longer select this class.
func (c *Controller) onTrafficClassChange(_ interface{}) {
	c.enqueueSelectivePeers()
}

func (c *Controller) onTrafficClassUpdate(old, new interface{}) {
	before := old.(*trafficv1.TrafficClass)
	after := new.(*trafficv1.TrafficClass)
	if reflect.DeepEqual(before.Labels, after.Labels) && reflect.DeepEqual(before.Spec, after.Spec) {
		return
	}
	c.enqueueSelectivePeers()
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

// Caller holds configurationMutex so the chain reflects one applied-state snapshot.
func (c *Controller) syncSelectiveSNAT() error {
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
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
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
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
			parts = append(parts, protocol, "dport", d.DestinationPort.String())
		}
		if d.SourcePort != (egressip.PortRange{}) {
			parts = append(parts, protocol, "sport", d.SourcePort.String())
		}
	}
	parts = append(parts, "snat", family, "to", config.snatValue)
	return strings.Join(parts, " ")
}

// All selective peers may change winners when one owner is edited or removed.
func (c *Controller) enqueueSelectivePeers() {
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return
	}
	eips, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list selective EgressIPs: %v", err)
		return
	}
	for _, eip := range eips {
		if egressip.HasTrafficSelector(eip.Spec) {
			c.eIPQueue.Add(eip.Name)
		}
	}
}

func (c *Controller) acceptsPodTraffic(eip *eipv1.EgressIP, pod *corev1.Pod, namespace *corev1.Namespace) (bool, error) {
	if !egressip.HasTrafficSelector(eip.Spec) {
		return true, nil
	}
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return false, nil
	}
	eips, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		return false, err
	}
	classes, err := c.trafficLister.List(labels.Everything())
	if err != nil {
		return false, err
	}
	selected, err := egressip.SelectPodTraffic(pod, namespace.Labels, eips, classes)
	if err != nil {
		return false, err
	}
	conflict, included := selected[eip.Name]
	return included && conflict == nil, nil
}
