package healthcheck

import (
	context "context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type Allocator interface {
	Allocate(node *corev1.Node) error
	List() map[string]bool
	IsReachable(node string) bool
	Remove(node string)
}

type clientAllocator struct {
	id string
}

func NewClientAllocator(id string) *clientAllocator {
	return &clientAllocator{id: id}
}

type hcAllocator struct {
	sync.Mutex
	nodes        map[string]*healthNode
	hcPort       int
	totalTimeout int
}

type healthNode struct {
	hc        egressIPHealthClient
	reachable bool
	mgmtIPs   []net.IP
	consumers sets.String
}

var globalAllocator *hcAllocator

// blablabla
func CheckNodesReachability(hcPort, totalTimeout int, stopChan <-chan struct{}) {
	globalAllocator = &hcAllocator{
		nodes:        map[string]*healthNode{},
		hcPort:       hcPort,
		totalTimeout: totalTimeout,
	}

	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			globalAllocator.Lock()
			for _, node := range globalAllocator.nodes {
				node.reachable = globalAllocator.isReachable(node)
			}
			globalAllocator.Unlock()
		case <-stopChan:
			klog.V(5).Infof("Stop channel got triggered: will stop CheckNodesReachability")
			// disconnect all!
			return
		}
	}
}

func (h *clientAllocator) Allocate(node *corev1.Node) error {
	if globalAllocator == nil {
		return fmt.Errorf("could not allocate %s, CheckNodesReachability was not previously called", node.Name)
	}

	globalAllocator.Lock()
	defer globalAllocator.Unlock()

	if _, exists := globalAllocator.nodes[node.Name]; !exists {
		nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return err
		}

		mgmtIPs := make([]net.IP, len(nodeSubnets))
		for i, subnet := range nodeSubnets {
			mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
		}

		globalAllocator.nodes[node.Name] = &healthNode{
			hc: egressIPHealthClient{
				nodeName: node.Name,
			},
			mgmtIPs:   mgmtIPs,
			consumers: sets.NewString(h.id),
			reachable: true,
		}

		return nil
	}

	globalAllocator.nodes[node.Name].consumers.Insert(h.id)
	return nil
}

func (h *clientAllocator) List() map[string]bool {
	if globalAllocator == nil {
		return nil
	}

	globalAllocator.Lock()
	defer globalAllocator.Unlock()

	ret := map[string]bool{}
	for k, v := range globalAllocator.nodes {
		ret[k] = v.reachable
	}

	return ret
}

func (h *clientAllocator) IsReachable(nodeName string) bool {
	if globalAllocator == nil {
		return false
	}

	globalAllocator.Lock()
	defer globalAllocator.Unlock()

	node, exists := globalAllocator.nodes[nodeName]
	if !exists {
		return true // assuming stuff
	}

	return node.reachable
}

func (h *clientAllocator) Remove(node string) {
	if globalAllocator == nil {
		return //?
	}

	globalAllocator.Lock()
	defer globalAllocator.Unlock()

	hn, ok := globalAllocator.nodes[node]
	if !ok {
		return // ?
	}

	hn.consumers.Delete(h.id)
	if hn.consumers.Len() == 0 {
		hn.hc.Disconnect()
		delete(globalAllocator.nodes, node)
	}
}

func (h *hcAllocator) isReachable(hn *healthNode) bool {
	// Check if we need to do node reachability check
	if h.totalTimeout == 0 {
		return true
	}

	if h.hcPort == 0 {
		return h.isReachableLegacy(hn)
	}

	return h.isReachableViaGRPC(hn)
}

func (h *hcAllocator) isReachableViaGRPC(hn *healthNode) bool {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), time.Duration(h.totalTimeout)*time.Second)
	defer dialCancel()

	if !hn.hc.IsConnected() {
		// gRPC session is not up. Attempt to connect and if that suceeds, we will declare node as reacheable.
		return hn.hc.Connect(dialCtx, hn.mgmtIPs, h.hcPort)
	}

	// gRPC session is already established. Send a probe, which will succeed, or close the session.
	return hn.hc.Probe(dialCtx)
}

// LEGACY

type egressIPDialer interface {
	dial(ip net.IP, timeout time.Duration) bool
}

type egressIPDial struct{}

// Blantant copy from: https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by trying to
// open a TCP connection to the "discard" service (port 9); if the node is offline, the
// attempt will either time out with no response, or else return "no route to host" (and
// we will return false). If the node is online then we presumably will get a "connection
// refused" error; but the code below assumes that anything other than timeout or "no
// route" indicates that the node is online.
func (e *egressIPDial) dial(ip net.IP, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "9"), timeout)
	if conn != nil {
		conn.Close()
	}
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Timeout() {
			return false
		}
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok && sysErr.Err == syscall.EHOSTUNREACH {
			return false
		}
	}
	return true
}

var dialer egressIPDialer = &egressIPDial{}

func (h *hcAllocator) isReachableLegacy(hn *healthNode) bool {
	var retryTimeOut, initialRetryTimeOut time.Duration

	numMgmtIPs := len(hn.mgmtIPs)
	if numMgmtIPs == 0 {
		return false
	}

	switch h.totalTimeout {
	// Check if we need to do node reachability check
	case 0:
		return true
	case 1:
		// Using time duration for initial retry with 700/numIPs msec and retry of 100/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(700/numMgmtIPs) * time.Millisecond
		retryTimeOut = time.Duration(100/numMgmtIPs) * time.Millisecond
	default:
		// Using time duration for initial retry with 900/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(900/numMgmtIPs) * time.Millisecond
		retryTimeOut = initialRetryTimeOut
	}

	timeout := initialRetryTimeOut
	endTime := time.Now().Add(time.Second * time.Duration(h.totalTimeout))
	for time.Now().Before(endTime) {
		for _, ip := range hn.mgmtIPs {
			if dialer.dial(ip, timeout) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
		timeout = retryTimeOut
	}
	klog.Errorf("Failed reachability check for %s", hn.hc.nodeName)
	return false
}
