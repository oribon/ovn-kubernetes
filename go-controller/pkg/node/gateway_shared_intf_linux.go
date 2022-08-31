//go:build linux
// +build linux

package node

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/discovery/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// deletes the local bridge used for DGP and removes the corresponding iface, as well as OVS bridge mappings
func deleteLocalNodeAccessBridge() error {
	// remove br-local bridge
	_, stderr, err := util.RunOVSVsctl("--if-exists", "del-br", types.LocalBridgeName)
	if err != nil {
		return fmt.Errorf("failed to delete bridge %s, stderr:%s (%v)",
			types.LocalBridgeName, stderr, err)
	}
	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	if len(stdout) > 0 {
		locnetMapping := fmt.Sprintf("%s:%s", types.LocalNetworkName, types.LocalBridgeName)
		if strings.Contains(stdout, locnetMapping) {
			var newMappings string
			bridgeMappings := strings.Split(stdout, ",")
			for _, bridgeMapping := range bridgeMappings {
				if bridgeMapping != locnetMapping {
					if len(newMappings) != 0 {
						newMappings += ","
					}
					newMappings += bridgeMapping
				}
			}
			_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
				fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", newMappings))
			if err != nil {
				return fmt.Errorf("failed to set ovn-bridge-mappings, stderr:%s, error: (%v)", stderr, err)
			}
		}
	}

	klog.Info("Local Node Access bridge removed")
	return nil
}

// addGatewayIptRules adds the necessary iptable rules for a service on the node
func addGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := addIptRules(rules); err != nil {
		klog.Errorf("Failed to add iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

// delGatewayIptRules removes the iptable rules for a service from the node
func delGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := delIptRules(rules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

func updateEgressSVCIptRules(svc *kapi.Service, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) {
	if !shouldConfigureEgressSVC(svc, svcHasLocalHostNetEndPnt, npw) {
		return
	}

	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()

	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	cachedEps := npw.egressServiceInfo[key]
	if cachedEps == nil {
		cachedEps = &serviceEps{map[string]bool{}, map[string]bool{}}
		npw.egressServiceInfo[key] = cachedEps
	}

	// We create a copy of the cached eps for the service to deduce which
	// eps should be added/removed iptables rules.
	copiedEps := serviceEps{}
	for addr := range cachedEps.v4 {
		copiedEps.v4[addr] = true
	}
	for addr := range cachedEps.v6 {
		copiedEps.v6[addr] = true
	}

	epSlices, err := npw.watchFactory.GetEndpointSlices(svc.Namespace, svc.Name)
	if err != nil {
		klog.V(5).Infof("No endpointslice found for egress service %s in namespace %s during update", svc.Name, svc.Namespace)
		return
	}

	v4Eps := &[]string{} // All current v4 eps
	v6Eps := &[]string{} // All current v6 eps
	for _, epSlice := range epSlices {
		if epSlice.AddressType == v1.AddressTypeFQDN {
			continue
		}
		epsToAppend := v4Eps
		if epSlice.AddressType == v1.AddressTypeIPv6 {
			epsToAppend = v6Eps
		}

		for _, ep := range epSlice.Endpoints {
			*epsToAppend = append(*epsToAppend, ep.Addresses...)
		}
	}

	// If an iptable rule already exists in the cache we don't need to do anything,
	// otherwise we add a new rule for it later.
	v4ToAdd := []string{}
	v6ToAdd := []string{}
	for _, addr := range *v4Eps {
		if copiedEps.v4[addr] {
			delete(copiedEps.v4, addr)
			continue
		}
		v4ToAdd = append(v4ToAdd, addr)
	}
	for _, addr := range *v6Eps {
		if copiedEps.v6[addr] {
			delete(copiedEps.v6, addr)
			continue
		}
		v6ToAdd = append(v6ToAdd, addr)
	}

	addRules := egressSVCIPTRulesForEndpoints(svc, v4ToAdd, v6ToAdd)
	if err := addIptRules(addRules); err != nil {
		klog.Errorf("Failed to add iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	// Update the cache with the added endpoints.
	for _, addr := range v4ToAdd {
		cachedEps.v4[addr] = true
	}

	for _, addr := range v6ToAdd {
		cachedEps.v6[addr] = true
	}

	// We need to delete the rules that correspond to deleted endpoints -
	// these are the addresses that we did not delete from the copy of the cache earlier.
	v4ToDelete := make([]string, len(copiedEps.v4))
	v6ToDelete := make([]string, len(copiedEps.v6))
	for addr := range copiedEps.v4 {
		v4ToDelete = append(v4ToDelete, addr)
	}
	for addr := range copiedEps.v6 {
		v6ToDelete = append(v6ToDelete, addr)
	}

	delRules := egressSVCIPTRulesForEndpoints(svc, v4ToDelete, v6ToDelete)
	if err := delIptRules(delRules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	// Update the cache with the deleted endpoints.
	for _, addr := range v4ToDelete {
		delete(cachedEps.v4, addr)
	}
	for _, addr := range v6ToDelete {
		delete(cachedEps.v6, addr)
	}
}

func delAllEgressSVCIptRules(svc *kapi.Service, npw *nodePortWatcher) {
	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()
	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	allEps, found := npw.egressServiceInfo[key]
	if !found {
		return
	}

	v4ToDelete := make([]string, len(allEps.v4))
	v6ToDelete := make([]string, len(allEps.v6))
	for addr := range allEps.v4 {
		v4ToDelete = append(v4ToDelete, addr)
	}
	for addr := range allEps.v6 {
		v6ToDelete = append(v6ToDelete, addr)
	}

	delRules := egressSVCIPTRulesForEndpoints(svc, v4ToDelete, v6ToDelete)
	if err := delIptRules(delRules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}

	delete(npw.egressServiceInfo, key)
}

func shouldConfigureEgressSVC(svc *kapi.Service, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) bool {
	svcHost, _ := util.GetEgressSVCHost(svc)

	return svcHost == npw.nodeName &&
		svc.Spec.Type == kapi.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0 &&
		!svcHasLocalHostNetEndPnt
}
