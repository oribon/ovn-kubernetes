package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("Egress Services", func() {
	const (
		externalContainerName = "external-container-for-egress-service"
		serviceV4IP           = "5.5.5.5"
		serviceV6IP           = "5555:5555:5555:5555:5555:5555:5555:5555"
		podHTTPPort           = 8080
		serviceName           = "test-egress-service"
	)

	command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)}
	pods := []string{"pod1", "pod2", "pod3"}
	podsLabels := map[string]string{"egress": "please"}

	var (
		externalIPv4 string
		externalIPv6 string
		nodes        []v1.Node
	)

	f := wrappedTestFramework("egress-services")

	ginkgo.BeforeEach(func() {
		var err error
		clientSet := f.ClientSet
		n, err := e2enode.GetBoundedReadySchedulableNodes(clientSet, 3)
		framework.ExpectNoError(err)
		if len(n.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(n.Items))
		}
		nodes = n.Items
		ginkgo.By("Creating an external container to send the traffic to/from")
		externalIPv4, externalIPv6 = createClusterExternalContainer(externalContainerName, agnhostImage,
			[]string{"--privileged", "--network", "kind"}, []string{"netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)})
	})

	ginkgo.AfterEach(func() {
		deleteClusterExternalContainer(externalContainerName)
	})

	ginkgotable.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip without selectors",
		func(svcIP string, dstIP *string) {
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service without node selectors")
			createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, svcIP,
				map[string]string{"k8s.ovn.org/egress-service": "{}"}, podsLabels, podHTTPPort)

			ginkgo.By("Getting the IPs of the node in charge of the service")
			_, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)

			ginkgo.By("Setting the static route on the external container for the service via the egress host ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Resetting the service's annotations the backend pods should exit with their node's IP")
			svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get service")
			svc.Annotations = map[string]string{}
			_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to reset service's annotations")

			for i, pod := range pods {
				node := &nodes[i]
				v4, v6 := getNodeAddresses(node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgotable.Entry("ipv4 pods", serviceV4IP, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", serviceV6IP, &externalIPv6),
	)

	ginkgotable.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip with selectors",
		func(svcIP string, dstIP *string) {
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			index := 0
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					_, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					return err
				})
				index++
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service selecting the first node")
			firstNode := nodes[0].Name
			createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, svcIP,
				map[string]string{"k8s.ovn.org/egress-service": fmt.Sprintf("{\"nodeSelector\":{\"matchLabels\":{\"kubernetes.io/hostname\": \"%s\"}}}", firstNode)}, podsLabels, podHTTPPort)

			ginkgo.By("Verifying the first node was picked for handling the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			framework.ExpectEqual(node.Name, firstNode, "the wrong node got selected for egress service")

			ginkgo.By("Setting the static route on the external container for the service via the first node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the service to select the second node")
			secondNode := nodes[1].Name
			svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get service")
			svc.Annotations = map[string]string{"k8s.ovn.org/egress-service": fmt.Sprintf("{\"nodeSelector\":{\"matchLabels\":{\"kubernetes.io/hostname\": \"%s\"}}}", secondNode)}
			_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to update service's annotations")

			ginkgo.By("Verifying the second node now handles the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP = getEgressSVCHost(f.ClientSet, svc.Namespace, svc.Name)
			framework.ExpectEqual(node.Name, secondNode, "the wrong node got selected for egress service")
			nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", svc.Namespace, svc.Name)})
			framework.ExpectNoError(err, "failed to list nodes")
			framework.ExpectEqual(len(nodeList.Items), 1, fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the second node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the service to select no node")
			svc, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get service")
			svc.Annotations = map[string]string{"k8s.ovn.org/egress-service": "{\"nodeSelector\":{\"matchLabels\":{\"perfect\": \"match\"}}}"}
			_, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to update service's annotations")
			gomega.Eventually(func() error {
				nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", svc.Namespace, svc.Name)})
				if err != nil {
					return err
				}
				if len(nodeList.Items) != 0 {
					return fmt.Errorf("expected no nodes to be labeled for the service, got %v", nodeList.Items)
				}
				return nil
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Verifying the backend pods exit with their node's IP")
			for i, pod := range pods {
				node := nodes[i]
				v4, v6 := getNodeAddresses(&node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}

			ginkgo.By("Updating the third node to match the service's selector")
			thirdNode := nodes[2].Name
			node, err = f.ClientSet.CoreV1().Nodes().Get(context.TODO(), thirdNode, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get node")
			oldLabels := map[string]string{}
			for k, v := range node.Labels {
				oldLabels[k] = v
			}
			node.Labels["perfect"] = "match"
			_, err = f.ClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to update node's labels")
			defer func() {
				node, err = f.ClientSet.CoreV1().Nodes().Get(context.TODO(), thirdNode, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to get node")
				node.Labels = oldLabels
				_, err = f.ClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
				framework.ExpectNoError(err, "failed to revert node's labels")
			}()

			ginkgo.By("Verifying the third node now handles the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP = getEgressSVCHost(f.ClientSet, svc.Namespace, svc.Name)
			framework.ExpectEqual(node.Name, thirdNode, "the wrong node got selected for egress service")
			nodeList, err = f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", svc.Namespace, svc.Name)})
			framework.ExpectNoError(err, "failed to list nodes")
			framework.ExpectEqual(len(nodeList.Items), 1, fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the third node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)
		},
		ginkgotable.Entry("ipv4 pods", serviceV4IP, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", serviceV6IP, &externalIPv6),
	)
})

// Creates a LoadBalancer service with the given IP and verifies it was set correctly.
func createLBServiceWithIngressIP(cs kubernetes.Interface, namespace, name, ip string, annotations, selector map[string]string, port int32) *v1.Service {
	ipFamily := v1.IPv4Protocol
	if utilnet.IsIPv6String(ip) {
		ipFamily = v1.IPv6Protocol
	}
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: annotations,
		},
		Spec: v1.ServiceSpec{
			Selector: selector,
			Ports: []v1.ServicePort{
				{
					Protocol: v1.ProtocolTCP,
					Port:     port,
				},
			},
			Type:       v1.ServiceTypeLoadBalancer,
			IPFamilies: []v1.IPFamily{ipFamily},
		},
	}

	svc, err := cs.CoreV1().Services(namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create loadbalancer service")

	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: ip}}
	_, err = cs.CoreV1().Services(namespace).UpdateStatus(context.TODO(), svc, metav1.UpdateOptions{})
	framework.ExpectNoError(err, "failed to set loadbalancer's ip")

	gomega.Eventually(func() error {
		svc, err = cs.CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if len(svc.Status.LoadBalancer.Ingress) != 1 {
			return fmt.Errorf("expected 1 lb ingress ip, got %v as ips", svc.Status.LoadBalancer.Ingress)
		}

		if svc.Status.LoadBalancer.Ingress[0].IP != ip {
			return fmt.Errorf("expected lb ingress to be %s, got %v", ip, svc.Status.LoadBalancer.Ingress[0].IP)
		}

		return nil
	}, 5*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred(), "failed to set loadbalancer's ingress ip")

	return svc
}

// Returns the node in charge of the egress service's traffic and its v4/v6 addresses.
func getEgressSVCHost(cs kubernetes.Interface, svcNamespace, svcName string) (*v1.Node, string, string) {
	egressHost := &v1.Node{}
	egressHostV4IP := ""
	egressHostV6IP := ""
	gomega.Eventually(func() error {
		var err error
		svc, err := cs.CoreV1().Services(svcNamespace).Get(context.TODO(), svcName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		svcEgressHost, found := svc.Annotations["k8s.ovn.org/egress-service-host"]
		if !found {
			return fmt.Errorf("egress-service-host annotation missing from service, got: %v", svc.Annotations)
		}

		egressHost, err = cs.CoreV1().Nodes().Get(context.TODO(), svcEgressHost, metav1.GetOptions{})
		if err != nil {
			return err
		}

		_, found = egressHost.Labels[fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s", svc.Namespace, svc.Name)]
		if !found {
			return fmt.Errorf("node %s does not have the label for egress service %s/%s, labels: %v",
				egressHost.Name, svc.Namespace, svc.Name, egressHost.Labels)
		}

		egressHostV4IP, egressHostV6IP = getNodeAddresses(egressHost)

		return nil
	}, 5*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred(), "failed to get egress service host")

	return egressHost, egressHostV4IP, egressHostV6IP
}

// Sets the route to the service via the egress host on the container.
// In a real cluster an external client gets a route for the LoadBalancer service
// from the LoadBalancer provider.
func setSVCRouteOnContainer(container, svcIP, v4Via, v6Via string) {
	if utilnet.IsIPv4String(svcIP) {
		out, err := runCommand("docker", "exec", container, "ip", "route", "replace", svcIP, "via", v4Via)
		framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
		return
	}

	out, err := runCommand("docker", "exec", container, "ip", "-6", "route", "replace", svcIP, "via", v6Via)
	framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
}

// Sends a request to an agnhost destination's /clientip which returns the source IP of the packet.
// Returns an error if the expectedIP is different than the response.
func curlAgnHostClientIPFromPod(namespace, pod, expectedIP, dstIP string, containerPort int) error {
	dst := net.JoinHostPort(dstIP, fmt.Sprint(containerPort))
	curlCmd := fmt.Sprintf("curl -s --retry-connrefused --retry 5 --max-time 1 http://%s/clientip", dst)
	out, err := framework.RunHostCmd(namespace, pod, curlCmd)
	if err != nil {
		return fmt.Errorf("failed to curl agnhost on %s from %s, err: %w", dstIP, pod, err)
	}
	ourip, _, err := net.SplitHostPort(out)
	if err != nil {
		return fmt.Errorf("failed to split agnhost's clientip host:port response, err: %w", err)
	}
	if ourip != expectedIP {
		return fmt.Errorf("reached agnhost %s with ip %s from %s instead of %s", dstIP, ourip, pod, expectedIP)
	}
	return nil
}

// Tries to reach all of the backends of the given service from the container.
func reachAllServiceBackendsFromExternalContainer(container, svcIP string, svcPort int32, svcPods []string) {
	backends := map[string]bool{}
	for _, pod := range svcPods {
		backends[pod] = true
	}

	dst := net.JoinHostPort(svcIP, fmt.Sprint(svcPort))
	for i := 0; i < 10*len(svcPods); i++ {
		out, err := runCommand("docker", "exec", container, "curl", "-s", fmt.Sprintf("http://%s/hostname", dst))
		framework.ExpectNoError(err, "failed to curl service ingress IP")
		out = strings.ReplaceAll(out, "\n", "")
		delete(backends, out)
		if len(backends) == 0 {
			break
		}
	}

	framework.ExpectEqual(len(backends), 0, fmt.Sprintf("did not reach all pods from outside, missed: %v", backends))
}
