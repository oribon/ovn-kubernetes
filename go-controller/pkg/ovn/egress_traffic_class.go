// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const egressIPDestinationNetworksPrefix = "egressip-dest-nets-"

// egressTrafficMatches is a resolved owner configuration. Its zero value is catch-all.
// Selective owners have nonempty expressions, including false matches for empty selections.
type egressTrafficMatches struct {
	v4 string
	v6 string
}

func (m egressTrafficMatches) selective() bool { return m.v4 != "" || m.v6 != "" }

func (m egressTrafficMatches) match(ipv6 bool) string {
	if ipv6 {
		return m.v6
	}
	return m.v4
}

func getEgressIPDestNetworksAddrSetDbIDs(name, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: egressIPDestinationNetworksPrefix + name,
		libovsdbops.NetworkKey:    "",
	})
}

func (e *EgressIPController) resolveEgressTraffic(eip *egressipv1.EgressIP) error {
	if !egressip.HasTrafficSelector(eip.Spec) || !config.OVNKubernetesFeature.EnableTrafficClass {
		e.trafficMatches.Delete(eip.Name)
		return nil
	}
	classes, err := e.watchFactory.TrafficClassInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}
	resolution, err := egressip.ResolveTraffic(eip, classes)
	if err != nil {
		if _, selectorErr := egressip.ParseTrafficSelector(eip.Spec.TrafficSelector); selectorErr == nil {
			return err
		}
		klog.Errorf("EgressIP %s selects no traffic: %v", eip.Name, err)
	}
	for _, unsupported := range resolution.Unsupported {
		klog.Warningf("EgressIP %s: TrafficClass %s match %d protocol %d is unsupported", eip.Name, unsupported.Class, unsupported.Match, unsupported.Protocol)
	}
	destinations := resolution.Destinations
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

// egressTraffic resolves desired behavior at the API boundary. Programming consumes
// the returned value; applied assignments retain their own snapshot for cleanup.
func (e *EgressIPController) egressTraffic(name string) (egressTrafficMatches, error) {
	eip, err := e.watchFactory.GetEgressIP(name)
	if err != nil {
		return egressTrafficMatches{}, err
	}
	if !egressip.HasTrafficSelector(eip.Spec) {
		return egressTrafficMatches{}, nil
	}
	if !config.OVNKubernetesFeature.EnableTrafficClass {
		return egressTrafficMatches{v4: "0 == 1", v6: "0 == 1"}, nil
	}
	value, ok := e.trafficMatches.Load(name)
	if !ok {
		return egressTrafficMatches{}, fmt.Errorf("traffic matches for EgressIP %s are not ready", name)
	}
	return value.(egressTrafficMatches), nil
}

// addPodEgressIPAssignments arbitrates owners before entering the ordinary programming path.
// Caller holds the pod lock. Withdraw rejected assignments before installing a winner.
func (e *EgressIPController) addPodEgressIPAssignments(ni util.NetInfo, name string, statusAssignments []egressipv1.EgressIPStatusItem, mark util.EgressIPMark, pod *corev1.Pod) error {
	podKey := getPodKey(pod)
	// Ignore completed pods, host networked pods, pods not scheduled
	if !util.PodNeedsSNAT(pod) {
		klog.Infof("Pod %s is not in desired state, skipping egress ip assignment", podKey)
		return nil
	}
	// If statusAssignments is empty just return, not doing this will delete the
	// external GW set up, even though there might be no egress IP set up to
	// perform.
	if len(statusAssignments) == 0 {
		return nil
	}
	traffic, err := e.egressTraffic(name)
	if err != nil {
		return err
	}
	state, exists := e.podAssignment.Load(podKey)
	if !exists {
		state = &podAssignmentState{
			activeEgressIPs:      map[string]*podEgressIPAssignment{},
			standbyEgressIPNames: sets.New[string](),
		}
	}
	// A pod retry may observe a selector edit before the owner's update handler.
	// Retire the old configuration before publishing the replacement snapshot.
	if applied := state.activeEgressIPs[name]; applied != nil && applied.traffic != traffic {
		if err := e.removeRejectedPodOwner(name, pod, state); err != nil {
			return err
		}
	}
	var deferred bool
	if traffic.selective() {
		deferred, err = e.deferToSelectiveEgressIPs(name, pod, state)
	} else {
		deferred, err = e.deferToActiveEgressIP(name, pod, state)
	}
	if err != nil || deferred {
		return err
	}
	return e.programPodEgressIPAssignments(ni, name, statusAssignments, mark, pod, state, traffic)
}

func (e *EgressIPController) deleteEgressTraffic(name string) error {
	ids := getEgressIPDestNetworksAddrSetDbIDs(name, e.controllerName)
	if err := e.addressSetFactory.DestroyAddressSet(ids); err != nil {
		return err
	}
	e.trafficMatches.Delete(name)
	return nil
}

func (e *EgressIPController) reconcileTrafficClass(class *trafficv1.TrafficClass) error {
	affected, err := e.reconcileTrafficClassAssignments(class)
	return utilerrors.Join(err, e.enqueueSelectiveEgressIPPods(affected...))
}

func (e *EgressIPController) reconcileTrafficClassAssignments(class *trafficv1.TrafficClass) ([]*egressipv1.EgressIP, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return nil, err
	}
	var affected []*egressipv1.EgressIP
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
			affected = append(affected, eip)
			if err = e.resolveEgressTraffic(eip); err != nil {
				return err
			}
			return e.rebuildEgressIPAssignments(eip, eip)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return affected, utilerrors.Join(errs...)
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

// Shared destination sets may still be referenced by networks awaiting stale sync.
func (e *EgressIPController) pruneEgressTraffic() error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	expected := sets.New[string]()
	for _, eip := range eips {
		if config.OVNKubernetesFeature.EnableTrafficClass && egressip.HasTrafficSelector(eip.Spec) {
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
		as, err := e.addressSetFactory.GetAddressSet(ids)
		if err != nil {
			return err
		}
		v4, v6 := as.GetASHashNames()
		referencesSet := func(match string) bool {
			return v4 != "" && strings.Contains(match, "$"+v4) || v6 != "" && strings.Contains(match, "$"+v6)
		}
		policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(e.nbClient, func(policy *nbdb.LogicalRouterPolicy) bool {
			return referencesSet(policy.Match)
		})
		if err != nil {
			return err
		}
		nats, err := libovsdbops.FindNATsWithPredicate(e.nbClient, func(nat *nbdb.NAT) bool {
			return referencesSet(nat.Match)
		})
		if err != nil {
			return err
		}
		if len(policies) != 0 || len(nats) != 0 {
			continue
		}
		if err = e.addressSetFactory.DestroyAddressSet(ids); err != nil {
			return err
		}
	}
	return nil
}

func isEgressIPReroutePriority(priority int) bool {
	return priority == types.EgressIPCatchAllReroutePriority || priority == types.EgressIPSelectiveReroutePriority
}

func isEgressIPMarkPriority(priority int) bool {
	return priority == types.EgressIPCatchAllSNATMarkPriority || priority == types.EgressIPSelectiveSNATMarkPriority
}

func egressIPMarkPriority(trafficMatch string) int {
	if trafficMatch != "" {
		return types.EgressIPSelectiveSNATMarkPriority
	}
	return types.EgressIPCatchAllSNATMarkPriority
}

func egressIPReroutePriority(trafficMatch string) int {
	if trafficMatch != "" {
		return types.EgressIPSelectiveReroutePriority
	}
	return types.EgressIPCatchAllReroutePriority
}

// egressIPPolicyMatch keeps live programming and restart repair identical.
// Mark policies apply only before any owner has marked the packet.
func egressIPPolicyMatch(podIP net.IP, trafficMatch string, markPolicy bool) string {
	match := fmt.Sprintf("%s.src == %s", ipFamilyName(podIP.To4() == nil), podIP)
	if markPolicy {
		match += " && pkt.mark == 0"
	}
	if trafficMatch != "" {
		match += " && " + trafficMatch
	}
	return match
}

// Queue peer work after releasing the event owner's lock. Rejected owners have
// no applied assignment to discover when a previous winner stops selecting traffic.
func (e *EgressIPController) enqueueSelectiveEgressIPPods(owners ...*egressipv1.EgressIP) error {
	pods := map[string]*corev1.Pod{}
	for _, owner := range owners {
		if owner == nil || !egressip.HasTrafficSelector(owner.Spec) {
			continue
		}
		namespaces, err := e.watchFactory.GetNamespacesBySelector(owner.Spec.NamespaceSelector)
		if err != nil {
			return err
		}
		for _, namespace := range namespaces {
			selected, err := e.watchFactory.GetPodsBySelector(namespace.Name, owner.Spec.PodSelector)
			if err != nil {
				return err
			}
			for _, pod := range selected {
				pods[getPodKey(pod)] = pod
			}
		}
	}
	var errs []error
	for _, pod := range pods {
		if err := e.addEgressIPPodRetry(pod, "traffic owner change"); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// Caller holds the pod lock. Compute from API state, never the applied-owner map.
func (e *EgressIPController) selectiveOwnersForPod(pod *corev1.Pod) (map[string]*egressip.TrafficConflict, error) {
	if !config.OVNKubernetesFeature.EnableTrafficClass {
		return nil, nil
	}
	ns, err := e.watchFactory.GetNamespace(pod.Namespace)
	if err != nil {
		return nil, err
	}
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return nil, err
	}
	classes, err := e.watchFactory.TrafficClassInformer().Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}
	return egressip.SelectPodTraffic(pod, ns.Labels, eips, classes)
}

func (e *EgressIPController) removeRejectedPodOwner(name string, pod *corev1.Pod, state *podAssignmentState) error {
	owner := state.activeEgressIPs[name]
	if owner == nil {
		return nil
	}
	statuses := make([]egressipv1.EgressIPStatusItem, 0, len(owner.statusMap))
	for status := range owner.statusMap {
		statuses = append(statuses, status)
	}
	return e.deletePodEgressIPAssignments(owner.network, name, statuses, pod, false)
}

// deferToSelectiveEgressIPs withdraws rejected owners before a winner is programmed.
// Caller holds the pod lock; selective owners never enter the catch-all standby set.
func (e *EgressIPController) deferToSelectiveEgressIPs(name string, pod *corev1.Pod, podState *podAssignmentState) (bool, error) {
	selected, err := e.selectiveOwnersForPod(pod)
	if err != nil {
		return false, err
	}
	conflict, eligible := selected[name]
	if !eligible || conflict != nil {
		podState.standbyEgressIPNames.Delete(name)
		return true, e.removeRejectedPodOwner(name, pod, podState)
	}
	// Withdraw every losing applied owner before installing a new winner.
	for peer := range podState.activeEgressIPs {
		if peer == name {
			continue
		}
		if !podState.activeEgressIPs[peer].traffic.selective() {
			continue
		}
		rejected, included := selected[peer]
		if included && rejected == nil {
			continue
		}
		if err = e.removeRejectedPodOwner(peer, pod, podState); err != nil {
			return false, err
		}
	}
	podState.standbyEgressIPNames.Delete(name)
	return false, nil
}
