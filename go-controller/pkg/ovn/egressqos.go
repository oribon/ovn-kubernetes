package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/pkg/errors"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type egressQos struct {
	sync.Mutex
	name      string
	namespace string
	rules     []*egressQosRule
}

type egressQosRule struct {
	priority    int
	dscp        int
	destination string
}

const (
	EgressQoSFlowStartPriority = 1000
)

// cloneEgressQoS shallow copies the egressqosapi.EgressQoS object provided.
func cloneEgressQoS(raw *egressqosapi.EgressQoS) *egressQos {
	eq := &egressQos{
		name:      raw.Name,
		namespace: raw.Namespace,
		rules:     make([]*egressQosRule, 0),
	}
	return eq
}

// implement destination validation
func cloneEgressQoSRule(raw egressqosapi.EgressQoSRule, priority int) (*egressQosRule, error) {
	_, _, err := net.ParseCIDR(raw.DstCIDR)
	if err != nil {
		return nil, err
	}

	eqr := &egressQosRule{
		priority:    priority,
		dscp:        raw.DSCP,
		destination: raw.DstCIDR,
	}

	return eqr, nil
}

func (oc *Controller) addEgressQoS(eqObj *egressqosapi.EgressQoS) error {
	klog.Infof("Adding EgressQoS %s in namespace %s", eqObj.Name, eqObj.Namespace)

	eq := cloneEgressQoS(eqObj)
	// there should not be an item already in the egressQoses map for the given Namespace
	if _, loaded := oc.egressQoses.LoadOrStore(eq.namespace, eq); loaded {
		return fmt.Errorf("error attempting to add egressQos %s to namespace %s when it already has a EgressQoS",
			eq.name, eq.namespace)
	}
	eq.Lock()
	defer eq.Unlock()

	var addErrors error
	for i, rule := range eqObj.Spec.Egress {
		eqr, err := cloneEgressQoSRule(rule, EgressQoSFlowStartPriority-i)
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create egressqos Rule to destination %s for namespace %s - %v",
				rule.DstCIDR, eq.namespace, err)
			continue
		}
		eq.rules = append(eq.rules, eqr)
	}
	if addErrors != nil {
		return addErrors
	}

	as, err := oc.addressSetFactory.EnsureAddressSet(eq.namespace)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for namespace %s exists %v", eq.namespace, err)
	}
	ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()

	err = oc.createEgressQoS(eq, ipv4HashedAS, ipv6HashedAS)
	if err != nil {
		return err
	}

	return nil
}

func (oc *Controller) createEgressQoS(eq *egressQos, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string) error {
	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	for _, r := range eq.rules {
		opModels := []libovsdbops.OperationModel{}
		match := generateEgressQoSMatch(r, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6)
		qos := nbdb.QoS{
			Direction:   nbdb.QoSDirectionFromLport,
			Match:       match,
			Priority:    r.priority,
			Action:      map[string]int{nbdb.QoSActionDSCP: r.dscp},
			ExternalIDs: map[string]string{"EgressQoS": eq.namespace},
		}
		opModels = append(opModels, libovsdbops.OperationModel{
			Model: &qos,
			ModelPredicate: func(q *nbdb.QoS) bool {
				return strings.Contains(q.Match, qos.Match) && q.Priority == qos.Priority
			},
			OnModelUpdates: []interface{}{
				&qos.Priority,
				&qos.Match,
				&qos.Action,
			},
			DoAfter: func() {
				if qos.UUID != "" {
					for _, sw := range logicalSwitches {
						sw.QOSRules = []string{qos.UUID}
					}
				}
			},
		})

		for _, sw := range logicalSwitches {
			lsn := sw.Name
			opModels = append(opModels, libovsdbops.OperationModel{
				Name:           sw.Name,
				Model:          sw,
				ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == lsn },
				OnModelMutations: []interface{}{
					&sw.QOSRules,
				},
				ErrNotFound: true,
			})
		}

		// TODO: do it in one transaction instead loop?
		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("failed to create qos, err: %s", err)
		}
	}

	return nil
}

func (oc *Controller) updateEgressQoS(old, new *egressqosapi.EgressQoS) error {
	updateErrors := oc.deleteEgressQoS(old)
	if updateErrors != nil {
		return updateErrors
	}
	updateErrors = oc.addEgressQoS(new)
	return updateErrors
}

func (oc *Controller) deleteEgressQoS(eqObj *egressqosapi.EgressQoS) error {
	klog.Infof("Deleting EgressQoS %s in namespace %s", eqObj.Name, eqObj.Namespace)

	obj, loaded := oc.egressQoses.LoadAndDelete(eqObj.Namespace)
	if !loaded {
		return fmt.Errorf("no EgressQoS found in namespace %s",
			eqObj.Namespace)
	}

	eq, ok := obj.(*egressQos)
	if !ok {
		return fmt.Errorf("deleteEgressQoS failed: type assertion to *egressQos"+
			" failed for EgressQoS %s of type %T in namespace %s",
			eqObj.Name, eqObj, eqObj.Namespace)
	}

	eq.Lock()
	defer eq.Unlock()

	as, err := oc.addressSetFactory.EnsureAddressSet(eq.namespace)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for namespace %s exists %v", eq.namespace, err)
	}
	ipv4HashedAS, ipv6HashedAS := as.GetASHashNames()

	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}
	for _, r := range eq.rules {
		opModels := []libovsdbops.OperationModel{}
		match := generateEgressQoSMatch(r, ipv4HashedAS, ipv6HashedAS)
		qos := nbdb.QoS{}
		opModels = append(opModels, libovsdbops.OperationModel{
			Model: &qos,
			ModelPredicate: func(q *nbdb.QoS) bool {
				return strings.Contains(q.Match, match) && q.Priority == r.priority
			},
			DoAfter: func() {
				if qos.UUID != "" {
					for _, sw := range logicalSwitches {
						sw.QOSRules = []string{qos.UUID}
					}
				}
			},
		})

		for _, sw := range logicalSwitches {
			lsn := sw.Name
			opModels = append(opModels, libovsdbops.OperationModel{
				Name:           sw.Name,
				Model:          sw,
				ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == lsn },
				OnModelMutations: []interface{}{
					&sw.QOSRules,
				},
				ErrNotFound: true,
			})
		}

		// TODO: do it in one transaction instead loop?
		if err := oc.modelClient.Delete(opModels...); err != nil {
			return fmt.Errorf("failed to delete qos, err: %s", err)
		}
	}

	return nil
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It will delete QoSes from EgressQoSes which have been deleted while ovnkube-master was down.
func (oc *Controller) syncEgressQoSes(eqs []interface{}) {
	oc.syncWithRetry("syncEgressQoses", func() error {
		nsWithEgressQoS := sets.NewString()
		for _, eq := range eqs {
			egressQos, ok := eq.(*egressqosapi.EgressQoS)
			if !ok {
				continue
			}
			nsWithEgressQoS.Insert(egressQos.Namespace)
		}

		return oc.deleteStaleEgressQoS(nsWithEgressQoS)
	})
}

func (oc *Controller) deleteStaleEgressQoS(nsWithEgressQoS sets.String) error {
	qosRes := []nbdb.QoS{}
	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(q *nbdb.QoS) bool {
				eqNs, ok := q.ExternalIDs["EgressQoS"]
				if !ok { // the QoS is not managed by an EgressQoS
					return false
				}
				if nsWithEgressQoS.Has(eqNs) { // it'll be reconciled later, not stale
					return false
				}

				klog.Infof("deleteStaleEgressQoS will delete qos from stale ns: %s", eqNs)
				return true
			},
			ExistingResult: &qosRes,
			DoAfter: func() {
				uuids := libovsdbops.ExtractUUIDsFromModels(&qosRes)
				for _, sw := range logicalSwitches {
					sw.QOSRules = uuids
				}
			},
			BulkOp: true,
		},
	}

	for _, sw := range logicalSwitches {
		lsn := sw.Name
		opModels = append(opModels, libovsdbops.OperationModel{
			Name:           sw.Name,
			Model:          sw,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == lsn },
			OnModelMutations: []interface{}{
				&sw.QOSRules,
			},
		})
	}

	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("unable to remove stale qoses, err: %v", err)
	}

	return nil
}

func generateEgressQoSMatch(eq *egressQosRule, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string) string {
	var src string
	var dst string

	switch {
	case config.IPv4Mode && config.IPv6Mode:
		src = fmt.Sprintf("(ip4.src == $%s || ip6.src == $%s)", hashedAddressSetNameIPv4, hashedAddressSetNameIPv6)
	case config.IPv4Mode:
		src = fmt.Sprintf("ip4.src == $%s", hashedAddressSetNameIPv4)
	case config.IPv6Mode:
		src = fmt.Sprintf("ip6.src == $%s", hashedAddressSetNameIPv6)
	}

	dst = fmt.Sprintf("ip4.dst == %s", eq.destination)
	if utilnet.IsIPv6CIDRString(eq.destination) {
		dst = fmt.Sprintf("ip6.dst == %s", eq.destination)
	}

	return fmt.Sprintf("(%s) && %s", dst, src)
}

func (oc *Controller) egressQoSSwitches() ([]*nbdb.LogicalSwitch, error) {
	logicalSwitches := []*nbdb.LogicalSwitch{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		nodeLocalSwitches, err := libovsdbops.FindAllNodeLocalSwitches(oc.nbClient)
		if err != nil {
			return nil, fmt.Errorf("unable to fetch local switches for EgressQoS, err: %v", err)
		}
		for _, nodeLocalSwitch := range nodeLocalSwitches {
			s := nodeLocalSwitch
			logicalSwitches = append(logicalSwitches, &s)
		}
		return logicalSwitches, nil
	}

	joinSw := &nbdb.LogicalSwitch{
		Name: types.OVNJoinSwitch,
	}
	logicalSwitches = append(logicalSwitches, joinSw)

	return logicalSwitches, nil
}
