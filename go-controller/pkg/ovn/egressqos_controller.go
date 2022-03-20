package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/informers/externalversions/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
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
	sync.Mutex
	name      string
	namespace string
	rules     []*egressQosRule
}

type egressQosRule struct {
	priority    int
	dscp        int
	destination string
	addrSet     addressset.AddressSet
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
			logicalPort, err := oc.logicalPortCache.get(util.GetLogicalPortName(pod.Namespace, pod.Name))
			if err != nil {
				return nil, err
			}

			podsIps = append(podsIps, createIPAddressSlice(logicalPort.ips)...)
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
		priority:    priority,
		dscp:        raw.DSCP,
		destination: raw.DstCIDR,
		addrSet:     addrSet,
	}

	return eqr, nil
}

// initEgressQoSController initializes the EgressQoS controller.
func (oc *Controller) initEgressQoSController(eqInformer egressqosinformer.EgressQoSInformer) {
	klog.Info("Setting up event handlers for EgressQoS")
	eqInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    oc.onEgressQoSAdd,
		UpdateFunc: oc.onEgressQoSUpdate,
		DeleteFunc: oc.onEgressQoSDelete,
	})
	oc.egressQoSLister = eqInformer.Lister()
	oc.egressQoSSynced = eqInformer.Informer().HasSynced
	oc.egressQoSQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"egressqos",
	)

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

	// don't process resync on objects that are marked for deletion
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

func (oc *Controller) runEgressQoSController(threadiness int, stopCh <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()

	klog.Infof("Starting EgressQoS Controller")

	// wait for your caches to fill before starting your work
	if !cache.WaitForCacheSync(stopCh, oc.egressQoSSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		klog.Infof("Synchronization failed")
		return
	}
	// run the repair controller
	klog.Infof("Repairing EgressQoSes")
	err := oc.repairEgressQoSes()
	if err != nil {
		klog.Errorf("failed to delete stale EgressQoS entries: %v", err)
	}

	// start up your worker threads based on threadiness.  Some controllers
	// have multiple kinds of workers
	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		// runEgressQoSWorker will loop until "something bad" happens.  The .Until will
		// then rekick the worker after one second
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runEgressQoSWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	// wait until we're told to stop
	<-stopCh

	klog.Infof("Shutting down EgressQoS controller")
	// make sure the work queue is shutdown which will trigger workers to end
	oc.egressQoSQueue.ShutDown()
	// wait for workers to finish
	wg.Wait()
}

func (oc *Controller) runEgressQoSWorker(wg *sync.WaitGroup) {
	// hot loop until we're told to stop.  processNextWorkItem will
	// automatically wait until there's work available, so we don't worry
	// about secondary waits
	for oc.processNextEgressQoSWorkItem(wg) {
	}
}

// processNextEgressQoSWorkItem deals with one key off the queue.  It returns false
// when it's time to quit.
func (oc *Controller) processNextEgressQoSWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	// pull the next work item from queue.  It should be a key we use to lookup
	// something in a cache
	key, quit := oc.egressQoSQueue.Get()
	if quit {
		return false
	}
	// you always have to indicate to the queue that you've completed a piece of
	// work
	defer oc.egressQoSQueue.Done(key)

	// do your work on the key.  This method will contain your "do stuff" logic
	err := oc.syncEgressQoS(key.(string))
	if err == nil {
		// if you had no error, tell the queue to stop tracking history for your
		// key. This will reset things like failure counts for per-item rate
		// limiting
		oc.egressQoSQueue.Forget(key)
		return true
	}

	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	// since we failed, we should requeue the item to work on later.
	// but only if we've not exceeded max retries. This method
	// will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	if oc.egressQoSQueue.NumRequeues(key) < maxEgressQoSRetries {
		oc.egressQoSQueue.AddRateLimited(key)
		return true
	}

	// if we've exceeded MaxRetries, remove the item from the queue
	oc.egressQoSQueue.Forget(key)
	return true
}

// This takes care of syncing stale data which we might have in OVN if
// there's no ovnkube-master running for a while.
// It deletes all QoSes and Address Sets from OVN that belong to EgressQoSes.
func (oc *Controller) repairEgressQoSes() error {
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

	// Get current EgressQoS from the cache
	eq, err := oc.egressQoSLister.EgressQoSes(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if name != defaultEgressQosName {
		klog.Errorf("EgressQoS name %s is invalid, must be %s", name, defaultEgressQosName)
		return nil // Return nil to avoid requeue
	}

	err = oc.cleanEgressQoSNS(namespace)
	if err != nil {
		return fmt.Errorf("unable to delete EgressQoS %s/%s, err: %v", namespace, name, err)
	}

	if eq == nil { // it was deleted no need to process further
		return nil
	}

	klog.Infof("EgressQoS %s retrieved from lister: %v", eq.Name, eq)

	return oc.addEgressQoSNS(eq)
}

func (oc *Controller) cleanEgressQoSNS(namespace string) error {
	obj, loaded := oc.egressQoSCache.Load(namespace)
	if !loaded {
		// the namespace is clean
		klog.Infof("EgressQos for namespace %s not found in cache", namespace)
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

	// we can delete the object from the cache now
	oc.egressQoSCache.Delete(namespace)

	return nil
}

func (oc *Controller) addEgressQoSNS(eqObj *egressqosapi.EgressQoS) error {
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

	err = oc.createEgressQoS(eq)
	if err != nil {
		return err
	}

	return nil
}

// Creates the necessary OVN QoS objects for an EgressQoS
func (oc *Controller) createEgressQoS(eq *egressQos) error {
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
