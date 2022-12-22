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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
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
func addGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) error {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := addIptRules(rules); err != nil {
		return fmt.Errorf("failed to add iptables rules for service %s/%s: %v",
			service.Namespace, service.Name, err)
	}
	return nil
}

// delGatewayIptRules removes the iptable rules for a service from the node
func delGatewayIptRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool) error {
	rules := getGatewayIPTRules(service, svcHasLocalHostNetEndPnt)

	if err := delIptRules(rules); err != nil {
		return fmt.Errorf("failed to delete iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
	return nil
}

func updateEgressSVCIptRules(svc *kapi.Service, npw *nodePortWatcher) error {
	if !shouldConfigureEgressSVC(svc, npw.nodeName) {
		return nil
	}
	conf, err := util.ParseEgressSVCAnnotation(svc.Annotations)
	if err != nil {
		return err
	}

	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()

	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	cachedConf := npw.egressServiceInfo[key]
	if cachedConf == nil {
		cachedConf = &egressServiceConfig{
			v4Eps:  sets.NewString(),
			v6Eps:  sets.NewString(),
			fwmark: conf.FWMark,
			cips:   sets.NewString(),
		}
		npw.egressServiceInfo[key] = cachedConf
	}

	epSlices, err := npw.watchFactory.GetEndpointSlices(svc.Namespace, svc.Name)
	if err != nil {
		return fmt.Errorf("failed to get endpointslices for egress service %s/%s during update: %v",
			svc.Namespace, svc.Name, err)
	}

	v4Eps := sets.NewString() // All current v4 eps
	v6Eps := sets.NewString() // All current v6 eps
	for _, epSlice := range epSlices {
		if epSlice.AddressType == v1.AddressTypeFQDN {
			continue
		}
		epsToInsert := v4Eps
		if epSlice.AddressType == v1.AddressTypeIPv6 {
			epsToInsert = v6Eps
		}

		for _, ep := range epSlice.Endpoints {
			for _, ip := range ep.Addresses {
				ipStr := utilnet.ParseIPSloppy(ip).String()
				if !isHostEndpoint(ipStr) {
					epsToInsert.Insert(ipStr)
				}
			}
		}
	}

	v4ToAdd := v4Eps.Difference(cachedConf.v4Eps).UnsortedList()
	v6ToAdd := v6Eps.Difference(cachedConf.v6Eps).UnsortedList()
	v4ToDelete := cachedConf.v4Eps.Difference(v4Eps).UnsortedList()
	v6ToDelete := cachedConf.v6Eps.Difference(v6Eps).UnsortedList()

	cips := sets.NewString(svc.Spec.ClusterIPs...)
	cipsToAdd := cips.Difference(cachedConf.cips).UnsortedList()
	cipsToDelete := cachedConf.cips.Difference(cips).UnsortedList()

	// Add rules for endpoints without one.
	addRules := egressSVCIPTRulesForEndpoints(svc, cachedConf.fwmark, v4ToAdd, v6ToAdd, cipsToAdd)
	if err := addIptRules(addRules); err != nil {
		return fmt.Errorf("failed to add iptables rules for service %s/%s during update: %v",
			svc.Namespace, svc.Name, err)
	}

	// Update the cache with the added endpoints.
	cachedConf.v4Eps.Insert(v4ToAdd...)
	cachedConf.v6Eps.Insert(v6ToAdd...)
	cachedConf.cips.Insert(cipsToAdd...)

	// Delete rules for endpoints that should not have one.
	delRules := egressSVCIPTRulesForEndpoints(svc, cachedConf.fwmark, v4ToDelete, v6ToDelete, cipsToDelete)
	if err := delIptRules(delRules); err != nil {
		return fmt.Errorf("failed to delete iptables rules for service %s/%s during update: %v",
			svc.Namespace, svc.Name, err)
	}

	// Update the cache with the deleted endpoints.
	cachedConf.v4Eps.Delete(v4ToDelete...)
	cachedConf.v6Eps.Delete(v6ToDelete...)
	cachedConf.cips.Delete(cipsToDelete...)
	return nil
}

func delAllEgressSVCIptRules(svc *kapi.Service, npw *nodePortWatcher) error {
	npw.egressServiceInfoLock.Lock()
	defer npw.egressServiceInfoLock.Unlock()
	key := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	cachedConf, found := npw.egressServiceInfo[key]
	if !found {
		return nil
	}

	v4ToDelete := cachedConf.v4Eps.UnsortedList()
	v6ToDelete := cachedConf.v6Eps.UnsortedList()
	cipsToDelete := cachedConf.cips.UnsortedList()

	delRules := egressSVCIPTRulesForEndpoints(svc, cachedConf.fwmark, v4ToDelete, v6ToDelete, cipsToDelete)
	if err := delIptRules(delRules); err != nil {
		return fmt.Errorf("failed to delete iptables rules for service %s/%s: %v", svc.Namespace, svc.Name, err)
	}

	delete(npw.egressServiceInfo, key)
	return nil
}

func shouldConfigureEgressSVC(svc *kapi.Service, thisNode string) bool {
	svcHost, _ := util.GetEgressSVCHost(svc)

	return util.HasEgressSVCAnnotation(svc) &&
		svcHost == thisNode &&
		svc.Spec.Type == kapi.ServiceTypeLoadBalancer &&
		len(svc.Status.LoadBalancer.Ingress) > 0 // && util.HasEgressSVCAnnotation(svc)
}
