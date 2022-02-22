package ovn

import (
	"context"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newEgressQoSObject(name, namespace string, egressRules []egressqosapi.EgressQoSRule) *egressqosapi.EgressQoS {
	return &egressqosapi.EgressQoS{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: egressqosapi.EgressQoSSpec{
			Egress: egressRules,
		},
	}
}

// TODO: tables for lgw/sgw
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

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})
	ginkgo.Context("no pod events", func() {
		ginkgo.It("should not create qos objects for empty namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
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
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{})
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressQoS()

				expectedDatabaseState := []libovsdbtest.TestData{
					node1Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should create and attach qos object to node1", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				t := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						node1Switch,
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
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)

				// Create one EgressQoS
				eq := newEgressQoSObject("default", namespaceT.Name, []egressqosapi.EgressQoSRule{
					{
						DstCIDR: "1.2.3.4/32",
						DSCP:    50,
					},
				})
				_, err := fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Create(context.TODO(), eq, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOVN.controller.WatchEgressQoS()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 1.2.3.4/32",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				node1Switch.QOSRules = append(node1Switch.QOSRules, qos1.UUID)
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					node1Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should create and attach qos objects to node1 and node2", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				t1 := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t2 := newTPod(
					node2Name,
					"10.128.2.0/24",
					"10.128.2.2",
					"10.128.2.1",
					"myPod2",
					"10.128.2.3",
					"0a:58:0a:80:02:03",
					namespaceT.Name,
				)

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node2Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
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
							*newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP),
							*newPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP),
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

				fakeOVN.controller.WatchEgressQoS()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 1.2.3.4/32",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 5.6.7.8/32",
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				node1Switch.QOSRules = append(node1Switch.QOSRules, qos1.UUID, qos2.UUID)
				node2Switch.QOSRules = append(node2Switch.QOSRules, qos1.UUID, qos2.UUID)
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
					node1Switch,
					node2Switch,
				}

				gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should delete and detach qos objects from node1 and node2", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				t1 := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t2 := newTPod(
					node2Name,
					"10.128.2.0/24",
					"10.128.2.2",
					"10.128.2.1",
					"myPod2",
					"10.128.2.3",
					"0a:58:0a:80:02:03",
					namespaceT.Name,
				)

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node2Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
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
							*newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP),
							*newPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP),
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

				fakeOVN.controller.WatchEgressQoS()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 1.2.3.4/32",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 5.6.7.8/32",
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				node1Switch.QOSRules = append(node1Switch.QOSRules, qos1.UUID, qos2.UUID)
				node2Switch.QOSRules = append(node2Switch.QOSRules, qos1.UUID, qos2.UUID)
				expectedDatabaseState := []libovsdbtest.TestData{
					qos1,
					qos2,
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
		})
		ginkgo.It("should update qos objects on node1 and node2", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				t1 := newTPod(
					node1Name,
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t2 := newTPod(
					node2Name,
					"10.128.2.0/24",
					"10.128.2.2",
					"10.128.2.1",
					"myPod2",
					"10.128.2.3",
					"0a:58:0a:80:02:03",
					namespaceT.Name,
				)

				node1Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node1Name,
				}

				node2Switch := &nbdb.LogicalSwitch{
					UUID: libovsdbops.BuildNamedUUID(),
					Name: node2Name,
				}

				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
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
							*newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP),
							*newPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP),
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

				fakeOVN.controller.WatchEgressQoS()

				qos1 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 1.2.3.4/32",
					Priority:    EgressQoSFlowStartPriority,
					Action:      map[string]int{nbdb.QoSActionDSCP: 50},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos1-UUID",
				}
				qos2 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 5.6.7.8/32",
					Priority:    EgressQoSFlowStartPriority - 1,
					Action:      map[string]int{nbdb.QoSActionDSCP: 60},
					ExternalIDs: map[string]string{"EgressQoS": namespaceT.Name},
					UUID:        "qos2-UUID",
				}
				node1Switch.QOSRules = append(node1Switch.QOSRules, qos1.UUID, qos2.UUID)
				node2Switch.QOSRules = append(node2Switch.QOSRules, qos1.UUID, qos2.UUID)
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
						DstCIDR: "10.11.12.13/32",
						DSCP:    40,
					},
				}
				_, err = fakeOVN.fakeClient.EgressQoSClient.K8sV1().EgressQoSes(namespaceT.Name).Update(context.TODO(), eq, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				qos3 := &nbdb.QoS{
					Direction:   nbdb.QoSDirectionFromLport,
					Match:       "ip4.src == $a10481622940199974102 && ip4.dst == 10.11.12.13/32",
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

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
