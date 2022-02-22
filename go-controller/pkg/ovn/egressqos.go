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
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type egressQos struct {
	sync.Mutex
	name            string
	namespace       string
	rules           []*egressQosRule
	logicalSwitches sets.String
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
		name:            raw.Name,
		namespace:       raw.Namespace,
		rules:           make([]*egressQosRule, 0),
		logicalSwitches: sets.NewString(),
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
	// egressqos created -> map[namespace]nodes , update map, send all nodes to createEgressQoS
	// egressqos modified -> delete current, create again.
	// deleted -> delete relavant qos and detach from logical switches
	existingPods, err := oc.watchFactory.GetPods(eq.namespace)
	if err != nil {
		return fmt.Errorf("failed to get all the pods (%v)", err)
	}
	nodes := nodesFromPods(existingPods)
	err = oc.createEgressQoS(eq, ipv4HashedAS, ipv6HashedAS, nodes)
	if err != nil {
		return err
	}
	eq.logicalSwitches = nodes

	return nil
}

func (oc *Controller) createEgressQoS(eq *egressQos, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string, swNames sets.String) error {
	nbdbsw := []*nbdb.LogicalSwitch{}
	for s := range swNames {
		nbdbsw = append(nbdbsw, &nbdb.LogicalSwitch{
			Name: s,
		})
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
				return strings.Contains(q.Match, match)
			},
			OnModelUpdates: []interface{}{
				&qos.Priority,
				&qos.Match,
				&qos.Action,
			},
			DoAfter: func() {
				if qos.UUID != "" {
					for _, sw := range nbdbsw {
						sw.QOSRules = []string{qos.UUID}
					}
				}
			},
		})

		for _, sw := range nbdbsw {
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

		if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
			return fmt.Errorf("tell me why: %s", err)
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

	nbdbsw := []*nbdb.LogicalSwitch{}
	for s := range eq.logicalSwitches {
		nbdbsw = append(nbdbsw, &nbdb.LogicalSwitch{
			Name: s,
		})
	}
	for _, r := range eq.rules {
		opModels := []libovsdbops.OperationModel{}
		match := generateEgressQoSMatch(r, ipv4HashedAS, ipv6HashedAS)
		qos := nbdb.QoS{}
		opModels = append(opModels, libovsdbops.OperationModel{
			Model: &qos,
			ModelPredicate: func(q *nbdb.QoS) bool {
				return strings.Contains(q.Match, match)
			},
			DoAfter: func() {
				if qos.UUID != "" {
					for _, sw := range nbdbsw {
						sw.QOSRules = []string{qos.UUID}
					}
				}
			},
		})

		for _, sw := range nbdbsw {
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

		if err := oc.modelClient.Delete(opModels...); err != nil {
			return fmt.Errorf("delete tell me why: %s", err)
		}
	}

	return nil
}

func generateEgressQoSMatch(eq *egressQosRule, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string) string {
	var match string
	switch {
	case config.IPv4Mode:
		//match = fmt.Sprintf(`ip4.src == $%s && ip4.dst == %s`, hashedAddressSetNameIPv4, eq.destination)
		match = fmt.Sprintf(`ip4.src == $%s && ip4.dst == %s`, hashedAddressSetNameIPv4, eq.destination)
	case config.IPv6Mode:
		match = fmt.Sprintf(`ip6.src == $%s && ip6.dst == %s`, hashedAddressSetNameIPv6, eq.destination)
	}

	return match
}

func nodesFromPods(pods []*corev1.Pod) sets.String {
	nodes := sets.NewString()
	for _, p := range pods {
		nodes.Insert(p.Spec.NodeName)
	}
	return nodes
}
