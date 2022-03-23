package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/informers/externalversions/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	v1coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	maxEgressQoSRetries        = 10
	defaultEgressQosName       = "default"
	EgressQoSFlowStartPriority = 1000
)

type egressQos struct {
	sync.RWMutex
	name      string
	namespace string
	rules     []*egressQosRule
	stale     bool
}

type egressQosRule struct {
	priority      int
	dscp          int
	destination   string
	addrSet       addressset.AddressSet
	podsInAddrSet *sync.Map
	podSelector   labels.Selector
}

// shallow copies the EgressQoS object provided.
func (oc *Controller) cloneEgressQoS(raw *egressqosapi.EgressQoS) (*egressQos, error) {
	eq := &egressQos{
		name:      raw.Name,
		namespace: raw.Namespace,
		rules:     make([]*egressQosRule, 0),
	}

	addErrors := errors.New("")
	for i, rule := range raw.Spec.Egress {
		eqr, err := oc.cloneEgressQoSRule(rule, eq.namespace, EgressQoSFlowStartPriority-i)
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create egressqos Rule to destination %s for namespace %s - %v",
				rule.DstCIDR, eq.namespace, err)
			continue
		}
		eq.rules = append(eq.rules, eqr)
	}

	if addErrors.Error() == "" {
		addErrors = nil
	}

	return eq, addErrors
}

// shallow copies the EgressQoSRule object provided.
func (oc *Controller) cloneEgressQoSRule(raw egressqosapi.EgressQoSRule, namespace string, priority int) (*egressQosRule, error) {
	_, _, err := net.ParseCIDR(raw.DstCIDR)
	if err != nil {
		return nil, err
	}

	selector, err := metav1.LabelSelectorAsSelector(&raw.PodSelector)
	if err != nil {
		return nil, err
	}

	var addrSet addressset.AddressSet
	podNames := sync.Map{}
	if !selector.Empty() {
		pods, err := oc.watchFactory.GetPodsBySelector(namespace, raw.PodSelector)
		if err != nil {
			return nil, err
		}

		addrSet, err = oc.addressSetFactory.EnsureAddressSet(fmt.Sprintf("%s%s-%d", types.EgressQoSRulePrefix, namespace, priority))
		if err != nil {
			return nil, err
		}

		podsIps := []net.IP{}
		for _, pod := range pods {
			if util.PodWantsNetwork(pod) { // we don't handle HostNetworked pods
				logicalPort, err := oc.logicalPortCache.get(util.GetLogicalPortName(pod.Namespace, pod.Name))
				if err != nil {
					return nil, err
				}
				podNames.Store(pod.Name, "")
				podsIps = append(podsIps, createIPAddressSlice(logicalPort.ips)...)
			}
		}
		err = addrSet.SetIPs(podsIps)
		if err != nil {
			return nil, err
		}
	} else {
		addrSet, err = oc.addressSetFactory.EnsureAddressSet(namespace)
		if err != nil {
			return nil, fmt.Errorf("cannot ensure that addressSet for namespace %s exists %v", namespace, err)
		}
	}

	eqr := &egressQosRule{
		priority:      priority,
		dscp:          raw.DSCP,
		destination:   raw.DstCIDR,
		addrSet:       addrSet,
		podsInAddrSet: &podNames,
		podSelector:   selector,
	}

	return eqr, nil
}

// initEgressQoSController initializes the EgressQoS controller.
func (oc *Controller) initEgressQoSController(
	eqInformer egressqosinformer.EgressQoSInformer,
	podInformer v1coreinformers.PodInformer,
	nodeInformer v1coreinformers.NodeInformer) {
	klog.Info("Setting up event handlers for EgressQoS")
	oc.egressQoSLister = eqInformer.Lister()
	oc.egressQoSSynced = eqInformer.Informer().HasSynced
	oc.egressQoSQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressqos",
	)
	eqInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSAdd,
		UpdateFunc: oc.onEgressQoSUpdate,
		DeleteFunc: oc.onEgressQoSDelete,
	})

	oc.egressQoSPodLister = podInformer.Lister()
	oc.egressQoSPodSynced = podInformer.Informer().HasSynced
	oc.egressQoSPodQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressqospods",
	)
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSPodAdd,
		UpdateFunc: oc.onEgressQoSPodUpdate,
		DeleteFunc: func(obj interface{}) {}, // Deletes are handled in deleteLogicalPort
	})

	oc.egressQoSNodeLister = nodeInformer.Lister()
	oc.egressQoSNodeSynced = nodeInformer.Informer().HasSynced
	oc.egressQoSNodeQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressqosnodes",
	)
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSNodeAdd,
		UpdateFunc: func(o, n interface{}) {}, // Updates/Deletes do not matter here as
		DeleteFunc: func(obj interface{}) {},  // the node's ls will be deleted
	})
}

func (oc *Controller) runEgressQoSController(threadiness int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting EgressQoS Controller")

	if !cache.WaitForNamedCacheSync("egressqosnodes", stopCh, oc.egressQoSNodeSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressqospods", stopCh, oc.egressQoSPodSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	if !cache.WaitForNamedCacheSync("egressqos", stopCh, oc.egressQoSSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}

	klog.Infof("Repairing EgressQoSes")
	err := oc.repairEgressQoSes()
	if err != nil {
		klog.Errorf("Failed to delete stale EgressQoS entries: %v", err)
	}

	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSPodWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSNodeWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	// wait until we're told to stop
	<-stopCh

	klog.Infof("Shutting down EgressQoS controller")
	oc.egressQoSQueue.ShutDown()
	oc.egressQoSPodQueue.ShutDown()
	oc.egressQoSNodeQueue.ShutDown()

	wg.Wait()
}

// onEgressQoSAdd queues the EgressQoS for processing.
func (oc *Controller) onEgressQoSAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding EgressQoS %s", key)
	oc.egressQoSQueue.Add(key)
}

// onEgressQoSUpdate queues the EgressQoS for processing.
func (oc *Controller) onEgressQoSUpdate(oldObj, newObj interface{}) {
	oldEQ := oldObj.(*egressqosapi.EgressQoS)
	newEQ := newObj.(*egressqosapi.EgressQoS)

	if oldEQ.ResourceVersion == newEQ.ResourceVersion ||
		!newEQ.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		oc.egressQoSQueue.Add(key)
	}
}

// onEgressQoSDelete queues the EgressQoS for processing.
func (oc *Controller) onEgressQoSDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting EgressQoS %s", key)
	oc.egressQoSQueue.Add(key)
}

func (oc *Controller) runEgressQoSWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSWorkItem(wg) {
	}
}

func (oc *Controller) processNextEgressQoSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := oc.egressQoSQueue.Get()
	if quit {
		return false
	}

	defer oc.egressQoSQueue.Done(key)

	err := oc.syncEgressQoS(key.(string))
	if err == nil {
		oc.egressQoSQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if oc.egressQoSQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSQueue.AddRateLimited(key)
		return true
	}

	oc.egressQoSQueue.Forget(key)
	return true
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It deletes all QoSes and Address Sets from OVN that belong to EgressQoSes.
func (oc *Controller) repairEgressQoSes() error {
	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for egressqos")
	defer func() {
		klog.V(4).Infof("Finished repairing loop for egressqos: %v", time.Since(startTime))
	}()

	qosRes := []nbdb.QoS{}
	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(q *nbdb.QoS) bool {
				_, ok := q.ExternalIDs["EgressQoS"]
				return ok // determines if the QoS is managed by an EgressQoS
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
			ErrNotFound: true,
		})
	}

	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("unable to remove stale qoses, err: %v", err)
	}

	addrSetList := []nbdb.AddressSet{}
	addrSetOpModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(as *nbdb.AddressSet) bool {
				return strings.Contains(as.ExternalIDs["name"], types.EgressQoSRulePrefix)
			},
			ExistingResult: &addrSetList,
			BulkOp:         true,
		},
	}
	if err := oc.modelClient.Delete(addrSetOpModels...); err != nil {
		return fmt.Errorf("failed to remove stale egress qos address sets, err: %v", err)
	}

	return nil
}

func (oc *Controller) syncEgressQoS(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for EgressQoS %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	eq, err := oc.egressQoSLister.EgressQoSes(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if name != defaultEgressQosName {
		klog.Errorf("EgressQoS name %s is invalid, must be %s", name, defaultEgressQosName)
		return nil // Return nil to avoid requeues
	}

	err = oc.cleanEgressQoSNS(namespace)
	if err != nil {
		return fmt.Errorf("unable to delete EgressQoS %s/%s, err: %v", namespace, name, err)
	}

	if eq == nil { // it was deleted no need to process further
		return nil
	}

	klog.V(5).Infof("EgressQoS %s retrieved from lister: %v", eq.Name, eq)

	return oc.addEgressQoS(eq)
}

func (oc *Controller) cleanEgressQoSNS(namespace string) error {
	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded {
		// the namespace is clean
		klog.V(4).Infof("EgressQoS for namespace %s not found in cache", namespace)
		return nil
	}

	eq := obj.(*egressQos)

	eq.Lock()
	defer eq.Unlock()

	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	qosRes := []nbdb.QoS{}
	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(q *nbdb.QoS) bool {
				eqNs, ok := q.ExternalIDs["EgressQoS"]
				if !ok { // the QoS is not managed by an EgressQoS
					return false
				}
				return eqNs == eq.namespace
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
			ErrNotFound: true,
		})
	}

	if err := oc.modelClient.Delete(opModels...); err != nil {
		return fmt.Errorf("failed to delete qos, err: %s", err)
	}

	addrSetList := []nbdb.AddressSet{}
	addrSetOpModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(as *nbdb.AddressSet) bool {
				return strings.Contains(as.ExternalIDs["name"], types.EgressQoSRulePrefix+eq.namespace)
			},
			ExistingResult: &addrSetList,
			BulkOp:         true,
		},
	}
	if err := oc.modelClient.Delete(addrSetOpModels...); err != nil {
		return fmt.Errorf("failed to remove egress qos address sets, err: %v", err)
	}

	// we can delete the object from the cache now.
	// we also mark it as stale to prevent pod processing if RLock
	// acquired after removal from cache.
	oc.egressQoSCache.Delete(namespace)
	eq.stale = true

	return nil
}

func (oc *Controller) addEgressQoS(eqObj *egressqosapi.EgressQoS) error {
	eq, err := oc.cloneEgressQoS(eqObj)
	if err != nil {
		return err
	}

	// there should not be an item in the cache for the given namespace
	// as we first attempt to delete before create.
	// if there is, we might be running concurrent create/updates.
	if _, loaded := oc.egressQoSCache.LoadOrStore(eq.namespace, eq); loaded {
		return fmt.Errorf("error attempting to add egressQos %s to namespace %s when it already has an EgressQoS",
			eq.name, eq.namespace)
	}

	eq.Lock()
	defer eq.Unlock()

	logicalSwitches, err := oc.egressQoSSwitches()
	if err != nil {
		return err
	}

	opModels := []libovsdbops.OperationModel{}
	for _, r := range eq.rules {
		hashedIPv4, hashedIPv6 := r.addrSet.GetASHashNames()
		match := generateEgressQoSMatch(r, hashedIPv4, hashedIPv6)
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
						sw.QOSRules = append(sw.QOSRules, qos.UUID)
					}
				}
			},
		})
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
			ErrNotFound: true,
		})
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("failed to create qos, err: %s", err)
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
	if config.Gateway.Mode == config.GatewayModeLocal {
		logicalSwitches := []*nbdb.LogicalSwitch{}
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

	joinSw, err := libovsdbops.FindSwitchByName(oc.nbClient, types.OVNJoinSwitch)
	if err != nil {
		return nil, err
	}

	return []*nbdb.LogicalSwitch{joinSw}, nil
}

type setOp int

const (
	setInsert setOp = iota
	setDelete
)

type setAndOp struct {
	set *sync.Map
	op  setOp
}

func (oc *Controller) syncEgressQoSPod(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	klog.Infof("Processing sync for EgressQoS pod %s/%s", namespace, name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS pod %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded { // no EgressQoS in the namespace
		return nil
	}

	eq := obj.(*egressQos)
	eq.RLock() // allow multiple pods to sync
	defer eq.RUnlock()
	if eq.stale { // was deleted from cache while we tried to read it, already processed
		return nil
	}

	pod, err := oc.egressQoSPodLister.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// TODO(?): the pod was deleted, we can't process as we can't fetch the ips.
	// for now deletions are part of deleteLogicalPort.
	if pod == nil {
		return nil
	}

	klog.V(5).Infof("Pod %s retrieved from lister: %v", pod.Name, pod)

	if !util.PodWantsNetwork(pod) { // we don't handle HostNetworked pods
		return nil
	}

	logicalPort, err := oc.logicalPortCache.get(util.GetLogicalPortName(pod.Namespace, pod.Name))
	if err != nil {
		return err
	}

	podIps := createIPAddressSlice(logicalPort.ips)
	podLabels := labels.Set(pod.Labels)
	allOps := []ovsdb.Operation{}
	podSetops := []setAndOp{}
	for _, r := range eq.rules {
		if r.podSelector.Empty() {
			continue
		}

		_, loaded := r.podsInAddrSet.Load(pod.Name)
		if r.podSelector.Matches(podLabels) && !loaded {
			ops, err := r.addrSet.AddIPsReturnOps(podIps)
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
			podSetops = append(podSetops, setAndOp{r.podsInAddrSet, setInsert})
			continue
		}

		if !r.podSelector.Matches(podLabels) && loaded {
			ops, err := r.addrSet.DeleteIPsReturnOps(podIps)
			if err != nil {
				return err
			}
			allOps = append(allOps, ops...)
			podSetops = append(podSetops, setAndOp{r.podsInAddrSet, setDelete})
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, allOps)
	if err != nil {
		return err
	}

	for _, setOp := range podSetops {
		switch setOp.op {
		case setInsert:
			setOp.set.Store(pod.Name, "")
		case setDelete:
			setOp.set.Delete(pod.Name)
		}
	}

	return nil
}

// onEgressQoSPodAdd queues the pod for processing.
func (oc *Controller) onEgressQoSPodAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding EgressQoS pod %s", key)
	oc.egressQoSPodQueue.Add(key)
}

// onEgressQoSPodUpdate queues the pod for processing.
func (oc *Controller) onEgressQoSPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*kapi.Pod)
	newPod := newObj.(*kapi.Pod)

	if oldPod.ResourceVersion == newPod.ResourceVersion ||
		!newPod.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", newObj, err))
		return
	}

	oc.egressQoSPodQueue.Add(key)
}

func (oc *Controller) runEgressQoSPodWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSPodWorkItem(wg) {
	}
}

func (oc *Controller) processNextEgressQoSPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	key, quit := oc.egressQoSPodQueue.Get()
	if quit {
		return false
	}
	defer oc.egressQoSPodQueue.Done(key)

	err := oc.syncEgressQoSPod(key.(string))
	if err == nil {
		oc.egressQoSPodQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if oc.egressQoSPodQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSPodQueue.AddRateLimited(key)
		return true
	}

	oc.egressQoSPodQueue.Forget(key)
	return true
}

func (oc *Controller) deleteEgressQoSPod(name, namespace string, ips []net.IP) ([]ovsdb.Operation, error) {
	allOps := []ovsdb.Operation{}
	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded { // no EgressQoS in the namespace
		return allOps, nil
	}

	eq := obj.(*egressQos)
	eq.RLock() // allow multiple pods to delete
	defer eq.RUnlock()
	if eq.stale { // was deleted from cache while we tried to read it, already cleaned
		return allOps, nil
	}

	for _, rule := range eq.rules {
		rule.podsInAddrSet.Delete(name)
		ops, err := rule.addrSet.DeleteIPsReturnOps(ips)
		if err != nil {
			return nil, err
		}
		allOps = append(allOps, ops...)
	}

	return allOps, nil
}

// onEgressQoSAdd queues the node for processing.
func (oc *Controller) onEgressQoSNodeAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding EgressQoS node %s", key)
	oc.egressQoSNodeQueue.Add(key)
}

func (oc *Controller) runEgressQoSNodeWorker(wg *sync.WaitGroup) {
	for oc.processNextEgressQoSNodeWorkItem(wg) {
	}
}

func (oc *Controller) processNextEgressQoSNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	key, quit := oc.egressQoSNodeQueue.Get()
	if quit {
		return false
	}
	defer oc.egressQoSNodeQueue.Done(key)

	err := oc.syncEgressQoSNode(key.(string))
	if err == nil {
		oc.egressQoSNodeQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if oc.egressQoSNodeQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSNodeQueue.AddRateLimited(key)
		return true
	}

	oc.egressQoSNodeQueue.Forget(key)
	return true
}

func (oc *Controller) syncEgressQoSNode(key string) error {
	startTime := time.Now()
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for EgressQoS node %s", name)

	defer func() {
		klog.V(4).Infof("Finished syncing EgressQoS node %s : %v", name, time.Since(startTime))
	}()

	if config.Gateway.Mode == config.GatewayModeShared {
		return nil // in SGW we add the QoSes to the join switch
	}

	n, err := oc.egressQoSNodeLister.Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if n == nil { // we don't process node deletions, its logical switch will be deleted.
		return nil
	}

	klog.V(5).Infof("EgressQoS %s node retrieved from lister: %v", n.Name, n)

	nodeSw, err := libovsdbops.FindSwitchByName(oc.nbClient, n.Name)
	if err != nil {
		return err
	}

	qosRes := []nbdb.QoS{}
	opModels := []libovsdbops.OperationModel{
		{
			ModelPredicate: func(q *nbdb.QoS) bool {
				_, ok := q.ExternalIDs["EgressQoS"]
				return ok // determines if the QoS is managed by an EgressQoS
			},
			ExistingResult: &qosRes,
			DoAfter: func() {
				uuids := libovsdbops.ExtractUUIDsFromModels(&qosRes)
				nodeSw.QOSRules = uuids
			},
			BulkOp: true,
		},
		{
			Name:           nodeSw.Name,
			Model:          nodeSw,
			ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == nodeSw.Name },
			OnModelMutations: []interface{}{
				&nodeSw.QOSRules,
			},
			ErrNotFound: true,
		},
	}

	if _, err := oc.modelClient.CreateOrUpdate(opModels...); err != nil {
		return fmt.Errorf("unable to add existing qoses to new node, err: %v", err)
	}

	return nil
}
