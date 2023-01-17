package node

import (
	"context"
	"fmt"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Egress Service Operations", func() {
	var (
		app         *cli.App
		fakeOvnNode *FakeOVNNode
		fExec       *ovntest.FakeExec
		iptV4       util.IPTablesHelper
		netlinkMock *mocks.NetLinkOps
	)

	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		netlinkMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkMock)

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		fExec = ovntest.NewFakeExec()
		fakeOvnNode = NewFakeOVNNode(fExec)

		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressService = true
		_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr4, 24}}

		iptV4, _ = util.SetFakeIPTablesHelpers()
	})

	AfterEach(func() {
		fakeOvnNode.shutdown()
		util.SetNetLinkOpMockInst(origNetlinkInst)
	})

	Context("on egress service resource changes", func() {
		It("manages iptables rules for LoadBalancer egress service backed by ovn-k pods", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				epPortName := "https"
				epPortValue := int32(443)

				egressService := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service1",
						Namespace: "namespace1",
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: fakeNodeName,
					},
				}
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"10.128.0.3"},
				}
				epPort := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}

				// host-networked endpoint, should not have an SNAT rule created
				ep2 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"},
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2},
					[]discovery.EndpointPort{epPort})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							endpointSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							egressService,
						},
					},
				)

				wf := fakeOvnNode.watcher.(*factory.WatchFactory)
				egressServiceController := egressservice.NewController(fakeOvnNode.stopChan, ovnKubeNodeSNATMark, fakeOvnNode.nc.name,
					wf.EgressServiceInformer(), wf.ServiceInformer(), wf.EndpointSliceInformer())
				fakeOvnNode.wg.Add(1)
				go func() {
					defer fakeOvnNode.wg.Done()
					egressServiceController.Run(1)
				}()

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESS-SVC": []string{
							"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN",
							"-s 10.128.0.3 -m comment --comment namespace1/service1 -j SNAT --to-source 5.5.5.5",
						},
					},
					"filter": {},
					"mangle": {
						"OVN-KUBE-EGRESS-SVC": []string{
							"-m mark --mark 0x3f0 -m comment --comment Do not mark -j RETURN",
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				Eventually(func() error {
					return f4.MatchState(expectedTables)
				}).ShouldNot(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESS-SVC": []string{"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN"},
					},
					"filter": {},
					"mangle": {
						"OVN-KUBE-EGRESS-SVC": []string{"-m mark --mark 0x3f0 -m comment --comment Do not mark -j RETURN"},
					},
				}

				err := fakeOvnNode.fakeClient.EgressServiceClient.K8sV1().EgressServices("namespace1").Delete(context.TODO(), "service1", metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() error {
					return f4.MatchState(expectedTables)
				}).ShouldNot(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules for LoadBalancer egress service backed by ovn-k pods with routing table", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ip -4 rule del prio 5000",
					Err: nil,
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ip -4 rule add prio 5000 fwmark 5000 table mynetwork",
					Err: nil,
				})
				epPortName := "https"
				epPortValue := int32(443)

				egressService := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "service1",
						Namespace: "namespace1",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						Network: "mynetwork",
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: fakeNodeName,
					},
				}

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"10.128.0.3"},
				}
				epPort := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}

				// host-networked endpoint, should not have an SNAT rule created
				ep2 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"},
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2},
					[]discovery.EndpointPort{epPort})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							endpointSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							egressService,
						},
					},
				)

				wf := fakeOvnNode.watcher.(*factory.WatchFactory)
				egressServiceController := egressservice.NewController(fakeOvnNode.stopChan, ovnKubeNodeSNATMark, fakeOvnNode.nc.name,
					wf.EgressServiceInformer(), wf.ServiceInformer(), wf.EndpointSliceInformer())
				fakeOvnNode.wg.Add(1)
				go func() {
					defer fakeOvnNode.wg.Done()
					egressServiceController.Run(1)
				}()

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESS-SVC": []string{
							"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN",
							"-s 10.128.0.3 -m comment --comment namespace1/service1 -j SNAT --to-source 5.5.5.5",
						},
					},
					"filter": {},
					"mangle": {
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
				}
				epFirst := []string{
					"-m mark --mark 0x3f0 -m comment --comment Do not mark -j RETURN",
					"-s 10.128.0.3 -m comment --comment namespace1/service1 -j MARK --set-mark 5000",
					"-s 10.129.0.2 -m comment --comment namespace1/service1 -j MARK --set-mark 5000",
				}
				cipFirst := []string{
					"-m mark --mark 0x3f0 -m comment --comment Do not mark -j RETURN",
					"-s 10.129.0.2 -m comment --comment namespace1/service1 -j MARK --set-mark 5000",
					"-s 10.128.0.3 -m comment --comment namespace1/service1 -j MARK --set-mark 5000",
				}
				f4 := iptV4.(*util.FakeIPTables)
				Eventually(func() error {
					expectedTables["mangle"]["OVN-KUBE-EGRESS-SVC"] = epFirst
					if f4.MatchState(expectedTables) == nil {
						return nil
					}

					expectedTables["mangle"]["OVN-KUBE-EGRESS-SVC"] = cipFirst
					return f4.MatchState(expectedTables)
				}).ShouldNot(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EGRESS-SVC": []string{"-m mark --mark 0x3f0 -m comment --comment Do not SNAT to SVC VIP -j RETURN"},
					},
					"filter": {},
					"mangle": {
						"OVN-KUBE-EGRESS-SVC": []string{"-m mark --mark 0x3f0 -m comment --comment Do not mark -j RETURN"},
					},
				}

				err := fakeOvnNode.fakeClient.EgressServiceClient.K8sV1().EgressServices("namespace1").Delete(context.TODO(), "service1", metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() error {
					return f4.MatchState(expectedTables)
				}).ShouldNot(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
