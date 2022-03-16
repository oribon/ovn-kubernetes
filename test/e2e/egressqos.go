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
		srcPodName = "src-dscp-pod"
		dstPodName = "dst-dscp-pod"
		dscpValue  = 50
	)

	var (
		clientSet  kubernetes.Interface
		dstPodIPv4 string
		dstPodIPv6 string
		testClient egressqosclient.K8sV1Interface
	)

	f := framework.NewDefaultFramework("egressqos")

	ginkgo.BeforeEach(func() {
		clientSet = f.ClientSet // so it can be used in AfterEach
		clientconfig, err := framework.LoadConfig()
		framework.ExpectNoError(err)
		testClient, err = egressqosclient.NewForConfig(clientconfig)
		framework.ExpectNoError(err)

		nodes, err := e2enode.GetBoundedReadySchedulableNodes(clientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		_, err = createPod(f, srcPodName, nodes.Items[0].Name, f.Namespace.Name, []string{}, map[string]string{})
		framework.ExpectNoError(err)

		dstPod, err := createPod(f, dstPodName, nodes.Items[1].Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.Containers[0].Image = "quay.io/obraunsh/iperf3:tcpdump" // TODO: remove this, find better image with tcpdump
			p.Spec.Containers[0].Command = []string{"sleep"}
			p.Spec.Containers[0].Args = []string{"500000"}
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err)

		dstPodIPv4, dstPodIPv6 = getPodAddresses(dstPod)
	})

	ginkgo.AfterEach(func() {
	})

	ginkgotable.DescribeTable("Should validate correct DSCP value on packets coming from a pod",
		func(tcpDumpTpl string, dstIP *string, prefix string) {
			tcpDumpSync := errgroup.Group{}
			checkPingOnPod := func(pod string, dscp int) error {
				_, err := framework.RunKubectl(f.Namespace.Name, "exec", pod, "--", "timeout", "5",
					"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
				return err
			}

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPodName, dscpValue)
			})

			eq := &egressqosapi.EgressQoS{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: f.Namespace.Name,
				},
				Spec: egressqosapi.EgressQoSSpec{
					Egress: []egressqosapi.EgressQoSRule{
						{
							DSCP:    dscpValue,
							DstCIDR: *dstIP + prefix,
						},
					},
				},
			}

			// Create
			eq, err := testClient.EgressQoSes(f.Namespace.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", "3", *dstIP)
			framework.ExpectNoError(err, "Failed to ping %s %s from pod %s", dstPodName, dstIP, dstPodName)

			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod %s", dstPodName)

			// Update
			newDSCP := dscpValue - 10
			eq.Spec.Egress[0].DSCP = newDSCP
			eq, err = testClient.EgressQoSes(f.Namespace.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
			framework.ExpectNoError(err)

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPodName, newDSCP)
			})

			_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", "3", *dstIP)
			framework.ExpectNoError(err, "Failed to ping %s %s from pod %s", dstPodName, dstIP, dstPodName)

			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod %s", dstPodName)

			err = testClient.EgressQoSes(f.Namespace.Name).Delete(context.TODO(), eq.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)

			tcpDumpSync.Go(func() error {
				return checkPingOnPod(dstPodName, 0)
			})

			_, err = framework.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "ping", "-c", "3", *dstIP)
			framework.ExpectNoError(err, "Failed to ping %s %s from pod %s", dstPodName, dstIP, dstPodName)

			err = tcpDumpSync.Wait()
			framework.ExpectNoError(err, "Ping detected with a DSCP value on pod %s", dstPodName)
		},
		// tcpdump args: http://darenmatthews.com/blog/?p=1199 , https://www.tucny.com/home/dscp-tos
		ginkgotable.Entry("ipv4", "icmp and (ip and (ip[1] & 0xfc) >> 2 == %d)", &dstPodIPv4, "/32"),
		ginkgotable.Entry("BBBB ipv6", "icmp6 and (ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)", &dstPodIPv6, "/128"))
})
