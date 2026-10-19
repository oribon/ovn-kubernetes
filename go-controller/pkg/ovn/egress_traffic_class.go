// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egresstrafficclass/v1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const egressIPDestinationNetworksPrefix = "egressip-dest-nets-"

type egressTrafficMatches struct {
	v4 string
	v6 string
}

func getEgressIPDestNetworksAddrSetDbIDs(name, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: egressIPDestinationNetworksPrefix + name,
		libovsdbops.NetworkKey:    "",
	})
}

func (e *EgressIPController) resolveEgressTraffic(eip *egressipv1.EgressIP) error {
	if !egressip.HasTrafficSelector(eip.Spec) {
		e.trafficMatches.Delete(eip.Name)
		return nil
	}
	selector, err := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector)
	if err != nil {
		// Invalid stored selectors must clear old matches without blocking other owners.
		klog.Errorf("EgressIP %s selects no traffic: %v", eip.Name, err)
	}
	classes, err := e.watchFactory.EgressTrafficClassInformer().Lister().List(selector)
	if err != nil {
		return err
	}
	var raw []trafficv1.Destination
	for _, class := range classes {
		raw = append(raw, class.Spec.Destinations...)
	}
	destinations, err := egressip.ParseDestinations(raw)
	if err != nil {
		return err
	}
	cidrOnly, l4 := egressip.SplitByL4(destinations)
	addresses := make([]string, 0, len(cidrOnly))
	for _, d := range cidrOnly {
		addresses = append(addresses, d.CIDR.String())
	}
	as, err := e.addressSetFactory.NewAddressSet(getEgressIPDestNetworksAddrSetDbIDs(eip.Name, e.controllerName), addresses)
	if err != nil {
		return fmt.Errorf("create destination address set for EgressIP %s: %w", eip.Name, err)
	}
	v4, v6 := as.GetASHashNames()
	e.trafficMatches.Store(eip.Name, egressTrafficMatches{
		v4: buildEgressTrafficMatch(v4, l4, false),
		v6: buildEgressTrafficMatch(v6, l4, true),
	})
	return nil
}

func buildEgressTrafficMatch(addressSet string, l4 []egressip.Destination, ipv6 bool) string {
	clauses := []string{}
	family := ipFamilyName(ipv6)
	if addressSet != "" {
		clauses = append(clauses, fmt.Sprintf("%s.dst == $%s", family, addressSet))
	}
	for _, d := range l4 {
		if d.CIDR.Addr().Is6() != ipv6 {
			continue
		}
		protocol := d.ProtocolName()
		clause := fmt.Sprintf("%s.dst == %s && %s", family, d.CIDR, protocol)
		if d.DestinationPort != (egressip.PortRange{}) {
			clause += " && " + buildPortMatch(protocol+".dst", d.DestinationPort)
		}
		if d.SourcePort != (egressip.PortRange{}) {
			clause += " && " + buildPortMatch(protocol+".src", d.SourcePort)
		}
		clauses = append(clauses, "("+clause+")")
	}
	if len(clauses) == 0 {
		return "0 == 1"
	}
	return "(" + strings.Join(clauses, " || ") + ")"
}

func buildPortMatch(field string, ports egressip.PortRange) string {
	if ports.Start == ports.End {
		return fmt.Sprintf("%s == %d", field, ports.Start)
	}
	return fmt.Sprintf("(%s >= %d && %s <= %d)", field, ports.Start, field, ports.End)
}

func (e *EgressIPController) egressTrafficMatch(name string, ipv6 bool) (string, error) {
	if !config.OVNKubernetesFeature.EnableEgressTrafficClass {
		return "", nil
	}
	eip, err := e.watchFactory.GetEgressIP(name)
	if err != nil {
		return "", err
	}
	if !egressip.HasTrafficSelector(eip.Spec) {
		return "", nil
	}
	value, ok := e.trafficMatches.Load(name)
	if !ok {
		return "", fmt.Errorf("traffic matches for EgressIP %s are not ready", name)
	}
	match := value.(egressTrafficMatches)
	if ipv6 {
		return match.v6, nil
	}
	return match.v4, nil
}

func (e *EgressIPController) deleteEgressTraffic(name string) error {
	ids := getEgressIPDestNetworksAddrSetDbIDs(name, e.controllerName)
	if err := e.addressSetFactory.DestroyAddressSet(ids); err != nil {
		return err
	}
	e.trafficMatches.Delete(name)
	return nil
}

func (e *EgressIPController) reconcileEgressTrafficClass(class *trafficv1.EgressTrafficClass) error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	var errs []error
	for _, listed := range eips {
		err = e.egressIPCache.DoWithLock(listed.Name, func(name string) error {
			// An EgressIP update may have completed while this handler waited for the lock.
			eip, err := e.watchFactory.GetEgressIP(name)
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if !egressip.HasTrafficSelector(eip.Spec) {
				return nil
			}
			selector, err := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector)
			if err != nil {
				return nil // Invalid selectors cannot select this class.
			}
			if !selector.Matches(labels.Set(class.Labels)) {
				return nil
			}
			if err = e.resolveEgressTraffic(eip); err != nil {
				return err
			}
			return e.rebuildEgressIPAssignments(eip, eip)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// Caller holds the EgressIP cache lock; deleting one owner must preserve all peers.
func (e *EgressIPController) rebuildEgressIPAssignments(old, new *egressipv1.EgressIP) error {
	// Include partially applied assignments whose status is no longer in the informer.
	statuses := sets.New[egressipv1.EgressIPStatusItem](old.Status.Items...)
	for _, podKey := range e.podAssignment.GetKeys() {
		e.podAssignment.LockKey(podKey)
		if state, ok := e.podAssignment.Load(podKey); ok {
			if owner := state.activeEgressIPs[old.Name]; owner != nil {
				for status := range owner.statusMap {
					statuses.Insert(status)
				}
			}
		}
		e.podAssignment.UnlockKey(podKey)
	}
	if err := e.deleteEgressIPAssignments(old.Name, statuses.UnsortedList()); err != nil {
		return err
	}
	return e.addEgressIPAssignments(new.Name, new.Status.Items, getEgressIPPktMark(new.Name, new.Annotations), new.Spec.NamespaceSelector, new.Spec.PodSelector)
}

func (e *EgressIPController) syncEgressTraffic() error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	for _, eip := range eips {
		if err = e.resolveEgressTraffic(eip); err != nil {
			return err
		}
	}
	return nil
}

// Remove address sets only after stale policies and NATs have been reconciled.
func (e *EgressIPController) pruneEgressTraffic() error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	expected := sets.New[string]()
	for _, eip := range eips {
		if egressip.HasTrafficSelector(eip.Spec) {
			expected.Insert(egressIPDestinationNetworksPrefix + eip.Name)
		}
	}
	var stale []*libovsdbops.DbObjectIDs
	err = e.addressSetFactory.ProcessEachAddressSet(e.controllerName, libovsdbops.AddressSetEgressIP, func(ids *libovsdbops.DbObjectIDs) error {
		name := ids.GetObjectID(libovsdbops.ObjectNameKey)
		if strings.HasPrefix(name, egressIPDestinationNetworksPrefix) && !expected.Has(name) {
			stale = append(stale, ids)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, ids := range stale {
		if err = e.addressSetFactory.DestroyAddressSet(ids); err != nil {
			return err
		}
	}
	return nil
}

func isEgressIPReroutePriority(priority int) bool {
	return priority == types.EgressIPReroutePriority || priority == types.EgressIPTrafficReroutePriority
}

func isEgressIPMarkPriority(priority int) bool {
	return priority == types.EgressIPSNATMarkPriority || priority == types.EgressIPTrafficSNATMarkPriority
}

func egressIPMarkPriority(trafficMatch string) int {
	if trafficMatch != "" {
		return types.EgressIPTrafficSNATMarkPriority
	}
	return types.EgressIPSNATMarkPriority
}

func egressPodIPFromMatch(match string) net.IP {
	fields := strings.Fields(match)
	if len(fields) < 3 || (fields[0] != "ip4.src" && fields[0] != "ip6.src") || fields[1] != "==" {
		return nil
	}
	return net.ParseIP(fields[2])
}

func (e *EgressIPController) migrateEgressIPReroutePriorities() error {
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(e.nbClient, func(*nbdb.LogicalRouter) bool {
		return true
	})
	if err != nil {
		return err
	}
	for _, router := range routers {
		policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(e.nbClient, router.Name, func(policy *nbdb.LogicalRouterPolicy) bool {
			return ((isEgressIPReroutePriority(policy.Priority) && policy.Action == nbdb.LogicalRouterPolicyActionReroute) ||
				(isEgressIPMarkPriority(policy.Priority) && policy.Action == nbdb.LogicalRouterPolicyActionAllow)) &&
				policy.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
				policy.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType)
		})
		if err != nil {
			return err
		}
		groups := map[string]*nbdb.LogicalRouterPolicy{}
		var ops []ovsdb.Operation
		for _, policy := range policies {
			name, _ := getEIPLRPObjK8MetaData(policy.ExternalIDs)
			eip, lookupErr := e.watchFactory.GetEgressIP(name)
			if lookupErr != nil {
				if !apierrors.IsNotFound(lookupErr) {
					return lookupErr
				}
				ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, router.Name, policy)
				if err != nil {
					return err
				}
				continue
			}
			ip := egressPodIPFromMatch(policy.Match)
			if ip == nil {
				ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, router.Name, policy)
				if err != nil {
					return err
				}
				continue
			}
			priority := policy.Priority
			if isEgressIPMarkPriority(priority) {
				priority = types.EgressIPSNATMarkPriority
				if egressip.HasTrafficSelector(eip.Spec) {
					priority = types.EgressIPTrafficSNATMarkPriority
				}
			} else {
				priority = types.EgressIPReroutePriority
				if egressip.HasTrafficSelector(eip.Spec) {
					priority = types.EgressIPTrafficReroutePriority
				}
			}
			ids, idErr := libovsdbops.NewDbObjectIDsFromExternalIDs(libovsdbops.LogicalRouterPolicyEgressIP, policy.ExternalIDs)
			if idErr != nil {
				return idErr
			}
			ids = ids.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.PriorityKey: fmt.Sprint(priority)})
			key := ids.String() + "/" + ip.String()
			if kept := groups[key]; kept != nil {
				hops := sets.New[string](kept.Nexthops...)
				hops.Insert(policy.Nexthops...)
				kept.Nexthops = sets.List(hops)
				ops, err = libovsdbops.DeleteLogicalRouterPoliciesOps(e.nbClient, ops, router.Name, policy)
				if err != nil {
					return err
				}
				continue
			}
			updated := *policy
			updated.Priority = priority
			updated.ExternalIDs = ids.GetExternalIDs()
			updated.Match = fmt.Sprintf("%s.src == %s", ipFamilyName(ip.To4() == nil), ip)
			if isEgressIPMarkPriority(priority) {
				updated.Match += " && pkt.mark == 0"
			}
			trafficMatch, matchErr := e.egressTrafficMatch(name, ip.To4() == nil)
			if matchErr != nil {
				return matchErr
			}
			if trafficMatch != "" {
				updated.Match += " && " + trafficMatch
			}
			groups[key] = &updated
		}
		for _, policy := range groups {
			ops, err = libovsdbops.UpdateLogicalRouterPoliciesOps(e.nbClient, ops, policy)
			if err != nil {
				return err
			}
		}
		if _, err = libovsdbops.TransactAndCheck(e.nbClient, ops); err != nil {
			return err
		}
	}
	nats, err := libovsdbops.FindNATsWithPredicate(e.nbClient, func(nat *nbdb.NAT) bool {
		return nat.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == e.controllerName &&
			nat.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.EgressIPOwnerType)
	})
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, nat := range nats {
		name, _ := getEIPLRPObjK8MetaData(nat.ExternalIDs)
		if _, err = e.watchFactory.GetEgressIP(name); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		match, matchErr := e.egressTrafficMatch(name, net.ParseIP(nat.LogicalIP).To4() == nil)
		if matchErr != nil {
			return matchErr
		}
		updated := *nat
		updated.Match = match
		updated.Priority = 0
		if match != "" {
			updated.Priority = types.EgressIPTrafficNATPriority
		}
		ops, err = libovsdbops.UpdateNATOps(e.nbClient, ops, &updated)
		if err != nil {
			return err
		}
	}
	_, err = libovsdbops.TransactAndCheck(e.nbClient, ops)
	return err
}
