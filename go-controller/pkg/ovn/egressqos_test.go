package ovn

import (
	"context"
	"fmt"
	"net"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func newEgressQoSObject(name, namespace string, egressRules []egressqosapi.EgressQoSRule) *egressqosapi.EgressQoS {
	return &egressqosapi.EgressQoS{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: "1",
		},
		Spec: egressqosapi.EgressQoSSpec{
			Egress: egressRules,
		},
	}
}

// TODO oribon: tables for lgw/sgw
var _ = ginkgo.Describe("OVN EgressQoS Operations for local gateway mode", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeLocal
		config.OVNKubernetesFeature.EnableEgressQoS = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgotable.DescribeTable("reconciles existing and non-existing egressqoses without PodSelectors",
		func(ipv4Mode, ipv6Mode bool, dst1, dst2, match1, match2 string) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = ipv4Mode
				config.IPv6Mode = ipv6Mode
				namespaceT := *newNamespace("namespace1")

				staleQoS := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "some-match",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": "dummyNs"},
					UUID:        "staleQoS-UUID",
				}

				staleAddrSet := &nbdb.AddressSet{
					Name:        "egress-qos-pods-dummyNS",
					ExternalIDs: map[string]string{"name": "egress-qos-pods-dummyNS"},
					UUID:        "staleAS-UUID",
					Addresses:   []string{"1.2.3.4"},
				}

				node1Switch := &nbdb.LogicalSwitch{
					UUID:     libovsdbops.BuildNamedUUID(),
					Name:     node1Name,
					QOSRules: []string{staleQoS.UUID},
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID:     libovsdbops.BuildNamedUUID(),
					Name:     node2Name,
					QOSRules: []string{staleQoS.UUID},
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						staleQoS,
						staleAddrSet,
						node1Switch,
						node2Switch,
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
				)

				// Create one EgressQoS
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{
					{
						DstCIDR: dst1,
						DSCP:    50,
					},
					{
						DstCIDR: dst2,
						DSCP:    60,
					},
				})
				eq.ResourceVersion = "1"
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.InitAndRunEgressQoSController()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match1,
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match2,
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				node1Switch.QOSRules = []string{qos1.UUID, qos2.UUID}
				node2Switch.QOSRules = []string{qos1.UUID, qos2.UUID}
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Update the EgressQoS
				eq.Spec.Egress = []egressqosapi.EgressQoSRule{
					{
						DstCIDR: dst1,
						DSCP:    40,
					},
				}
				eq.ResourceVersion = "2"
				_, err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				qos3 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match1,
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 40},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos3-UUID",
				}
				node1Switch.QOSRules = []string{qos3.UUID}
				node2Switch.QOSRules = []string{qos3.UUID}
				expectedDatabaseState = []libovsdbtest.TestData{
					qos3,
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Delete the EgressQoS
				err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Delete(context.TODO(), eq.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node1Switch.QOSRules = []string{}
				node2Switch.QOSRules = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		ginkgotable.Entry("ipv4", true, false, "1.2.3.4/32", "5.6.7.8/32",
			"(ip4.dst == 1.2.3.4/32) && ip4.src == $a10481622940199974102",
			"(ip4.dst == 5.6.7.8/32) && ip4.src == $a10481622940199974102"),
		ginkgotable.Entry("ipv6", false, true, "2001:0db8:85a3:0000:0000:8a2e:0370:7334/128", "2001:0db8:85a3:0000:0000:8a2e:0370:7335/128",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7334/128) && ip6.src == $a10481620741176717680",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7335/128) && ip6.src == $a10481620741176717680"),
		ginkgotable.Entry("dual", true, true, "1.2.3.4/32", "2001:0db8:85a3:0000:0000:8a2e:0370:7335/128",
			"(ip4.dst == 1.2.3.4/32) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680)",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7335/128) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680)"),
	)

	ginkgotable.DescribeTable("reconciles existing and non-existing egressqoses with PodSelectors",
		func(ipv4Mode, ipv6Mode bool, dst1, dst2, match1, match2 string) {
			app.Action = func(ctx *cli.Context) error {
				config.IPv4Mode = ipv4Mode
				config.IPv6Mode = ipv6Mode
				namespaceT := *newNamespace("namespace1")

				staleQoS := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "some-match",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": "dummyNs"},
					UUID:        "staleQoS-UUID",
				}

				staleAddrSet := &nbdb.AddressSet{
					Name:        "egress-qos-pods-dummyNS",
					ExternalIDs: map[string]string{"name": "egress-qos-pods-dummyNS"},
					UUID:        "staleAS-UUID",
					Addresses:   []string{"1.2.3.4"},
				}

				podT := newPodWithLabels(
					namespaceT.Name,
					"myPod",
					node1Name,
					"10.128.1.3",
					map[string]string{"app": "nice"},
				)

				node1Switch := &nbdb.LogicalSwitch{
					UUID:     node1Name,
					Name:     node1Name,
					QOSRules: []string{staleQoS.UUID},
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID:     node2Name,
					Name:     node2Name,
					QOSRules: []string{staleQoS.UUID},
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						staleQoS,
						staleAddrSet,
						node1Switch,
						node2Switch,
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*podT,
						},
					},
				)

				i, n, _ := net.ParseCIDR("10.128.1.3" + "/23")
				n.IP = i
				fakeOVN.controller.logicalPortCache.add("", util.GetLogicalPortName(podT.Namespace, podT.Name), "", nil, []*net.IPNet{n})

				// Create one EgressQoS
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{
					{
						DstCIDR: dst1,
						DSCP:    50,
						PodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "nice",
							},
						},
					},
					{
						DstCIDR: dst2,
						DSCP:    60,
					},
				})
				eq.ResourceVersion = "1"
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.InitAndRunEgressQoSController()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match1,
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match2,
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				node1Switch.QOSRules = []string{qos1.UUID, qos2.UUID}
				node2Switch.QOSRules = []string{qos1.UUID, qos2.UUID}
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
					node1Switch,
					node2Switch,
				}

				//fakeOVN.asf.ExpectAddressSetExist("egress-qos-pods-namespace1-1000") doesn't work because fake.SetIPs() is noop?
				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Update the EgressQoS
				eq.Spec.Egress = []egressqosapi.EgressQoSRule{
					{
						DstCIDR: dst1,
						DSCP:    40,
						PodSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "nice",
							},
						},
					},
				}
				eq.ResourceVersion = "2"
				_, err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				qos3 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       match1,
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 40},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos3-UUID",
				}
				node1Switch.QOSRules = []string{qos3.UUID}
				node2Switch.QOSRules = []string{qos3.UUID}
				expectedDatabaseState = []libovsdbtest.TestData{
					qos3,
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Delete the EgressQoS
				err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Delete(context.TODO(), eq.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node1Switch.QOSRules = []string{}
				node2Switch.QOSRules = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		ginkgotable.Entry("ipv4", true, false, "1.2.3.4/32", "5.6.7.8/32",
			"(ip4.dst == 1.2.3.4/32) && ip4.src == $a8797969223947225899",
			"(ip4.dst == 5.6.7.8/32) && ip4.src == $a10481622940199974102"),
		ginkgotable.Entry("ipv6", false, true, "2001:0db8:85a3:0000:0000:8a2e:0370:7334/128", "2001:0db8:85a3:0000:0000:8a2e:0370:7335/128",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7334/128) && ip6.src == $a8797971422970482321",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7335/128) && ip6.src == $a10481620741176717680"),
		ginkgotable.Entry("dual", true, true, "1.2.3.4/32", "2001:0db8:85a3:0000:0000:8a2e:0370:7335/128",
			"(ip4.dst == 1.2.3.4/32) && (ip4.src == $a8797969223947225899 || ip6.src == $a8797971422970482321)",
			"(ip6.dst == 2001:0db8:85a3:0000:0000:8a2e:0370:7335/128) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680)"),
	)
})

var _ = ginkgo.Describe("OVN EgressQoS Operations for shared gateway mode", func() {
	var (
		app     *cli.App
		fakeOVN *FakeOVN
	)
	const (
		node1Name    string = "node1"
		node2Name    string = "node2"
		ipv4Tpl      string = "(ip4.dst == %s) && ip4.src == $a10481622940199974102"
		ipv6Tpl      string = "(ip6.dst == %s) && ip6.src == $a10481620741176717680"
		dualv4DstTpl string = "(ip4.dst == %s) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680)"
		dualv6DstTpl string = "(ip6.dst == %s) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680)"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.Gateway.Mode = config.GatewayModeShared
		config.OVNKubernetesFeature.EnableEgressQoS = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})
	ginkgo.Context("no node events", func() {
		ginkgo.It("should create/delete and attach/detach qos objects to/from join switch", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node2Name,
				}

				joinSwitch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: types.OVNJoinSwitch,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
						node2Switch,
						joinSwitch,
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
				)

				// Create one EgressQoS
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{
					{
						DstCIDR: "1.2.3.4/32",
						DSCP:    50,
					},
					{
						DstCIDR: "5.6.7.8/32",
						DSCP:    60,
					},
				})
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.InitAndRunEgressQoSController()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       fmt.Sprintf(ipv4Tpl, "1.2.3.4/32"), // TODO: table
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       fmt.Sprintf(ipv4Tpl, "5.6.7.8/32"), // TODO: table
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				joinSwitch.QOSRules = append(node1Switch.QOSRules, qos1.UUID, qos2.UUID)
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
					node1Switch,
					node2Switch,
					joinSwitch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Delete the EgressQoS
				err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Delete(context.TODO(), eq.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				joinSwitch.QOSRules = []string{}
				expectedDatabaseState = []libovsdbtest.TestData{
					node1Switch,
					node2Switch,
					joinSwitch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should update qos objects on node1 and node2", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node2Name,
				}

				joinSwitch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: types.OVNJoinSwitch,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
						node2Switch,
						joinSwitch,
					},
				}

				fakeOVN.startWithDBSetup(dbSetup,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
				)

				// Create one EgressQoS
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{
					{
						DstCIDR: "1.2.3.4/32",
						DSCP:    50,
					},
					{
						DstCIDR: "5.6.7.8/32",
						DSCP:    60,
					},
				})
				eq.ResourceVersion = "1"
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.InitAndRunEgressQoSController()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       fmt.Sprintf(ipv4Tpl, "1.2.3.4/32"), // TODO: table
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       fmt.Sprintf(ipv4Tpl, "5.6.7.8/32"), // TODO: table
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				joinSwitch.QOSRules = append(node1Switch.QOSRules, qos1.UUID, qos2.UUID)
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
					node1Switch,
					node2Switch,
					joinSwitch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Update the EgressQoS
				eq.Spec.Egress = []egressqosapi.EgressQoSRule{
					{
						DstCIDR: "10.11.12.13/32",
						DSCP:    40,
					},
				}
				eq.ResourceVersion = "2"
				_, err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				qos3 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       fmt.Sprintf(ipv4Tpl, "10.11.12.13/32"), // TODO: table
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 40},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos3-UUID",
				}
				joinSwitch.QOSRules = []string{qos3.UUID}
				expectedDatabaseState = []libovsdbtest.TestData{
					qos3,
					node1Switch,
					node2Switch,
					joinSwitch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

func (o *FakeOVN) InitAndRunEgressQoSController() {
	klog.Warningf("#### [%p] INIT EgressQoS", o)
	o.controller.initEgressQoSController(o.watcher.EgressQoSInformer())
	go func() {
		o.controller.runEgressQoSController(1, o.stopChan)
	}()
}
