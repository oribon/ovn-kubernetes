package e2e

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"                                                // TODO: Remove local ref
	egressqosclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/typed/egressqos/v1" // TODO: Remove local ref
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("e2e EgressQoS validation", func() {
	const (
		srcPodName  = "src-dscp-pod"
		dstPod1Name = "dst-dscp-pod1"
		dstPod2Name = "dst-dscp-pod2"
		dscpValue   = 50
	)

	var (
		clientSet   kubernetes.Interface
		dstPod1IPv4 string
		dstPod1IPv6 string
		dstPod2IPv4 string
		dstPod2IPv6 string
		testClient  egressqosclient.K8sV1Interface
		srcNode     string
	)

	f := framework.NewDefaultFramework("egressqos")

	ginkgo.BeforeEach(func() {
		clientSet = f.ClientSet // so it can be used in AfterEach
		clientconfig, err := framework.LoadConfig()
		framework.ExpectNoError(err)
		testClient, err = egressqosclient.NewForConfig(clientconfig)
		framework.ExpectNoError(err)

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(clientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		srcNode = nodes.Items[0].Name

		dstPod1, err := createPod(f, dstPod1Name, nodes.Items[1].Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.Containers[0].Image = "quay.io/obraunsh/iperf3:tcpdump" // TODO: replace this, find better image with tcpdump
			p.Spec.Containers[0].Command = []string{"sleep"}
			p.Spec.Containers[0].Args = []string{"500000"}
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err)
		dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

		dstPod2, err := createPod(f, dstPod2Name, nodes.Items[2].Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.Containers[0].Image = "quay.io/obraunsh/iperf3:tcpdump" // TODO: replace this, find better image with tcpdump
			p.Spec.Containers[0].Command = []string{"sleep"}
			p.Spec.Containers[0].Args = []string{"500000"}
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err)

		dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)
	})

	ginkgo.AfterEach(func() {
	})

	ginkgotable.DescribeTable("Should validate correct DSCP value on packets coming from a pod",
		func(tcpDumpTpl string, dst1IP *string, prefix1 string, dst2IP *string, prefix2 string, beforeCR bool) {
			if beforeCR {
				_, err := createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{}, map[string]string{"app": "test"})
				framework.ExpectNoError(err)
			}

			tcpDumpSync := errgroup.Group{}
			checkPingOnPod := func(pod string, dscp int) error {
				_, err := framework.RunKubectl(f.Namespace.Name, "exec", pod, "--", "timeout", "10",
					"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
				return err
			}

			pingSync := errgroup.Group{}
			pingFromPod := func(pod, dst string) error {
				_, err := framework.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "3", dst)
				return err
			}

			eq := &egressqosapi.EgressQoS{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: f.Namespace.Name,
				},
				Spec: egressqosapi.EgressQoSSpec{
					Egress: []egressqosapi.EgressQoSRule{
						{
							DSCP:    dscpValue - 1,
							DstCIDR: *dst1IP + prefix1,
						},
						{
							DSCP:    dscpValue - 2,
							DstCIDR: *dst2IP + prefix2,
							PodSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "test",
								},
							},
						},
					},
				},
			}

			// Create
			eq, err := testClient.EgressQoSes(f.Namespace.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			if !beforeCR {
				_, err := createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{}, map[string]string{"app": "test"})
				framework.ExpectNoError(err)
			}

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod1Name, dscpValue-1)
			})
			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod2Name, dscpValue-2)
			})

			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst1IP)
			})
			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst2IP)
			})

			err = pingSync.Wait()
			framework.ExpectNoError(err, "Failed to ping dst pod")
			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod")

			// Update
			eq.Spec.Egress = []egressqosapi.EgressQoSRule{
				{
					DSCP:    dscpValue - 10,
					DstCIDR: *dst1IP + prefix1,
				},
				{
					DSCP:    dscpValue - 20,
					DstCIDR: *dst2IP + prefix2,
				},
			}
			eq, err = testClient.EgressQoSes(f.Namespace.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
			framework.ExpectNoError(err)

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod1Name, dscpValue-10)
			})
			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod2Name, dscpValue-20)
			})

			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst1IP)
			})
			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst2IP)
			})

			err = pingSync.Wait()
			framework.ExpectNoError(err, "Failed to ping dst pod")
			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Failed to detect ping with correct DSCP")

			// Delete
			err = testClient.EgressQoSes(f.Namespace.Name).Delete(context.TODO(), eq.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod1Name, 0)
			})
			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPod2Name, 0)
			})

			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst1IP)
			})
			pingSync.Go(func() error {
				return pingFromPod(srcPodName, *dst2IP)
			})

			err = pingSync.Wait()
			framework.ExpectNoError(err, "Failed to ping dst pod")
			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Ping detected with a DSCP value")
		},
		// tcpdump args: http://darenmatthews.com/blog/?p=1199 , https://www.tucny.com/home/dscp-tos
		ginkgotable.Entry("ipv4 pod before CR", "icmp and (ip and (ip[1] & 0xfc) >> 2 == %d)", &dstPod1IPv4, "/32", &dstPod2IPv4, "/32", true),
		ginkgotable.Entry("ipv4 pod after CR", "icmp and (ip and (ip[1] & 0xfc) >> 2 == %d)", &dstPod1IPv4, "/32", &dstPod2IPv4, "/32", false),
		ginkgotable.Entry("ipv6 pod before CR", "icmp6 and (ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)", &dstPod1IPv6, "/128", &dstPod2IPv6, "/128", true),
		ginkgotable.Entry("ipv6 pod after CR", "icmp6 and (ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)", &dstPod1IPv6, "/128", &dstPod2IPv6, "/128", false))
})
