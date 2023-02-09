package egress_services

import (
	"fmt"
	"net"

	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/services"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	utilnet "k8s.io/utils/net"
)

func (c *Controller) onServiceAdd(obj interface{}) {
	service := obj.(*corev1.Service)
	// We only care about new LoadBalancer services that have the egress-service config annotation
	if !util.ServiceTypeHasLoadBalancer(service) || len(service.Status.LoadBalancer.Ingress) == 0 {
		return
	}

	c.queueService(service)
}

func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*corev1.Service)
	newService := newObj.(*corev1.Service)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	// We only care about LoadBalancer service updates that enable/disable egress service functionality
	if !util.ServiceTypeHasLoadBalancer(oldService) && !util.ServiceTypeHasLoadBalancer(newService) {
		return
	}

	c.queueService(newService)
}

func (c *Controller) onServiceDelete(obj interface{}) {
	service := obj.(*corev1.Service)
	// We only care about deletions of LoadBalancer services
	if !util.ServiceTypeHasLoadBalancer(service) {
		return
	}

	c.queueService(service)
}

func (c *Controller) queueService(svc *corev1.Service) error {
	c.Lock()
	defer c.Unlock()

	svcKey, err := cache.MetaNamespaceKeyFunc(svc)
	if err != nil {
		return err
	}

	state := c.services[svcKey]
	if state != nil {
		c.egressServiceQueue.Add(state.egressServiceKey)
		return nil
	}

	for esKey, pending := range c.pendingEgressServices {
		if pending.svcKey == svcKey {
			c.egressServiceQueue.Add(esKey)
		}
	}

	return nil
}

// Returns all of the non-host endpoints for the given service grouped by IPv4/IPv6.
func (c *Controller) allEndpointsFor(svc *corev1.Service) (sets.String, sets.String, []string, error) {
	// Get the endpoint slices associated to the Service
	esLabelSelector := labels.Set(map[string]string{
		discovery.LabelServiceName: svc.Name,
	}).AsSelectorPreValidated()

	endpointSlices, err := c.endpointSliceLister.EndpointSlices(svc.Namespace).List(esLabelSelector)
	if err != nil {
		return nil, nil, nil, err
	}

	v4Endpoints := sets.NewString()
	v6Endpoints := sets.NewString()
	nodes := sets.NewString()
	for _, eps := range endpointSlices {
		if eps.AddressType == discovery.AddressTypeFQDN {
			continue
		}

		epsToInsert := v4Endpoints
		if eps.AddressType == discovery.AddressTypeIPv6 {
			epsToInsert = v6Endpoints
		}

		for _, ep := range eps.Endpoints {
			for _, ip := range ep.Addresses {
				ipStr := utilnet.ParseIPSloppy(ip).String()
				if !services.IsHostEndpoint(ipStr) {
					epsToInsert.Insert(ipStr)
				}
			}
			if ep.NodeName != nil {
				nodes.Insert(*ep.NodeName)
			}
		}
	}

	return v4Endpoints, v6Endpoints, nodes.UnsortedList(), nil
}

func createIPAddressNetSlice(v4ips, v6ips []string) []net.IP {
	ipAddrs := make([]net.IP, 0)
	for _, ip := range v4ips {
		ipNet := net.ParseIP(ip)
		ipAddrs = append(ipAddrs, ipNet)
	}
	for _, ip := range v6ips {
		ipNet := net.ParseIP(ip)
		ipAddrs = append(ipAddrs, ipNet)
	}
	return ipAddrs
}

func (c *Controller) addPodIPsToAddressSetOps(addrSetIPs []net.IP) ([]libovsdb.Operation, error) {
	var ops []libovsdb.Operation
	as, err := c.addressSetFactory.GetAddressSet(ovntypes.EgressServiceServedPods)
	if err != nil {
		return nil, fmt.Errorf("cannot ensure that addressSet for cluster %s exists %v", ovntypes.EgressServiceServedPods, err)
	}
	if ops, err = as.AddIPsReturnOps(addrSetIPs); err != nil {
		return nil, fmt.Errorf("cannot add egressPodIPs %v from the address set %v: err: %v", addrSetIPs, ovntypes.EgressServiceServedPods, err)
	}
	return ops, nil
}

func (c *Controller) deletePodIPsFromAddressSetOps(addrSetIPs []net.IP) ([]libovsdb.Operation, error) {
	var ops []libovsdb.Operation
	as, err := c.addressSetFactory.GetAddressSet(ovntypes.EgressServiceServedPods)
	if err != nil {
		return nil, fmt.Errorf("cannot ensure that addressSet for cluster %s exists %v", ovntypes.EgressServiceServedPods, err)
	}
	if ops, err = as.DeleteIPsReturnOps(addrSetIPs); err != nil {
		return nil, fmt.Errorf("cannot delete egressPodIPs %v from the address set %v: err: %v", addrSetIPs, ovntypes.EgressServiceServedPods, err)
	}
	return ops, nil
}

// Returns the libovsdb operations to create the logical router policies for the service,
// given its key, the nexthops (mgmt ips) and endpoints to add.
func (c *Controller) createLogicalRouterPoliciesOps(key, v4MgmtIP, v6MgmtIP string, v4Endpoints, v6Endpoints []string) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip4.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v4MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				svcExternalIDKey: key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints {
		lrp := &nbdb.LogicalRouterPolicy{
			Match:    fmt.Sprintf("ip6.src == %s", addr),
			Priority: ovntypes.EgressSVCReroutePriority,
			Nexthops: []string{v6MgmtIP},
			Action:   nbdb.LogicalRouterPolicyActionReroute,
			ExternalIDs: map[string]string{
				svcExternalIDKey: key,
			},
		}
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == lrp.Match && item.Priority == lrp.Priority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, lrp, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}

// Returns the libovsdb operations to delete the logical router policies for the service,
// given its key and endpoints to delete.
func (c *Controller) deleteLogicalRouterPoliciesOps(key string, v4Endpoints, v6Endpoints []string) ([]libovsdb.Operation, error) {
	allOps := []libovsdb.Operation{}
	var err error

	for _, addr := range v4Endpoints {
		match := fmt.Sprintf("ip4.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	for _, addr := range v6Endpoints {
		match := fmt.Sprintf("ip6.src == %s", addr)
		p := func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Match == match && item.Priority == ovntypes.EgressSVCReroutePriority && item.ExternalIDs[svcExternalIDKey] == key
		}

		allOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.nbClient, allOps, ovntypes.OVNClusterRouter, p)
		if err != nil {
			return nil, err
		}
	}

	return allOps, nil
}
