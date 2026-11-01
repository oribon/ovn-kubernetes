# TrafficClass review guide

This stack implements [OKEP-6227](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6228),
using proposal revision `c91c643a09d855a54f9e7e104fa3dba6bb825226`.
It separates the assignment representation refactor from selective traffic behavior.

## Commit order

| Commit | Review contract |
|---|---|
| `refactor(egressip): track assignments by owner` | Generic owner-keyed applied state. Preserve existing inline add/delete flows, single-active arbitration, logging, validation and error handling. Only update comments that refer to the replaced fields. No lifecycle helper extraction or feature logic. |
| `refactor(egressip): isolate status removal` | Extract status teardown without changing owner retirement, standby promotion, or failure behavior. |
| `fix(egressip): repair state by network and owner` | Union served addresses and validate persisted policies against their actual network and owner before adding selective behavior. |
| `feat(egressip): define traffic class API` | Review handwritten types, generation wiring and admission tests without generated output. |
| `chore(egressip): generate traffic class API` | Generated clients, informers, deepcopy/OpenAPI/apply code, CRDs and API references for the preceding definitions. |
| `refactor(iprule): represent protocol and ports` | Rule identity and netlink conversions support protocol/port fields. Existing rules retain their zero-valued fields and priorities. |
| `feat(egressip): wire traffic class matching` | Shared destination semantics, the feature gate and informer/client plumbing are available to consumers. |
| `refactor(egressip): name catch-all priorities` | Rename existing priorities without changing their values or behavior. |
| `feat(egressip): program selective OVN traffic` | Multiple selective owners coexist with one catch-all. Reroute, NAT and UDN marking use destination/L4 matches; startup repair handles persisted state and priority changes. |
| `feat(egressip): program selective host traffic` | Linux rules, shared interface routes and nftables SNAT implement the same matches. Shared rules/routes and partially applied updates remain tracked. |
| `feat(egressip): report traffic conditions` | OVN controllers report complete global resolution/conflict results. Resource-version-protected patches preserve assignments; cluster-manager writes items and its mark annotation. |
| `feat(helm): expose traffic class configuration` | Helm/Kind enable the gate and grant resource permissions. |
| `test(e2e): cover selective egress lifecycle` | Traffic, class updates, coexistence, restart and failover scenarios; CI feature enablement. |
| `docs: explain selective egress traffic` | Configuration, supported arrangements, troubleshooting and upgrade/rollback. |

The first commit does not introduce TrafficClass selection or new datapath priorities.
Each feature commit carries the tests and fixture updates for its behavior.
The handwritten API commit depends on the immediately following generation commit
to build. The pair together has the same API tree as before this review split.

## Component map

```text
EgressIP namespace/pod selectors -> selected workloads
EgressIP trafficSelector -> TrafficClasses -> normalized Destinations
  OVN controller: reroute + default-network NAT / UDN packet marking
                  global resolution and overlap conditions
  Node controller: Linux rules + shared interface routes + nftables SNAT
  Cluster-manager: address assignment and packet-mark allocation
```

These controllers independently read informer state. They do not call one another.

| Responsibility | Starting point |
|---|---|
| Admission and API | `go-controller/pkg/crd/trafficclass/v1/types.go`, `pkg/crd/egressip/v1/types.go`, `test/crd-integration/trafficclass_test.go` |
| Normalized matches | `go-controller/pkg/util/egressip/destination.go` |
| Applied ownership | `go-controller/pkg/ovn/egressip.go`: `podAssignmentState`, `podEgressIPAssignment` |
| Replacement versus removal | `addPodEgressIPAssignments`, `deletePodEgressIPAssignmentStatuses`, `finishPodAssignmentRemoval` |
| OVN matching and events | `go-controller/pkg/ovn/egress_traffic_class.go` |
| OVN startup repair | `go-controller/pkg/ovn/egress_traffic_class_sync.go` |
| Host steering and SNAT | `go-controller/pkg/node/controllers/egressip/traffic.go`, `egressip.go`, `nftables.go` |
| Comparable Linux rules | `go-controller/pkg/node/iprulemanager/ip_rule_manager.go` |
| Conditions and status ownership | `go-controller/pkg/ovn/egress_traffic_conditions.go`, `go-controller/pkg/clustermanager/egressip_controller.go` |

## Invariants to review

- A missing traffic selector means catch-all. A selector resolving to nothing
  means no matching traffic. With the gate disabled, selective owners withdraw
  their configuration and never become catch-alls.
- Multiple selective owners can coexist; catch-all owners retain active/standby
  arbitration. A status replacement must not promote a standby halfway through.
- Per-owner state records applied IPs/network/statuses for cleanup after API changes.
- Shared served-pod addresses, host routes and Linux rules survive while a peer
  still needs them. Successful partial updates remain recorded for retry.
- OR combines destinations and protocol entries; source/destination ports within
  an entry are ANDed. Wildcard ports participate in overlap analysis.
- A shared pure selection orders candidates by creation timestamp, then name.
  Rejected candidates do not block later candidates. Losers withdraw their whole
  assignment for the affected pod and recover when overlap disappears.
- Live OVN programming and repair share `egressIPPolicyMatch` and priority helpers.
  UDNs retain their existing packet-mark and OVS SNAT pipeline.
- Individual OVN/nftables transactions are atomic; the cross-controller update is not.
- Lifecycle lock ordering and host configuration serialization remain in place.
- Both mixed-interface orientations work on the cluster default network,
  including filtered OVN steering for remote pods in the egress zone.
  Secondary-host EgressIPs on UDNs and no-overlay networks remain unsupported.

| Mechanism | Selective | Catch-all |
|---|---|---|
| OVN reroute | 100 | 99 |
| UDN gateway marking | 96 | 95 |
| OVN NAT | 100 | 0 |
| Linux rule | 6000 | 6001 |

The priority changes belong to the feature commits, including migration and repair.
Disabling the gate does not restore old priority numbers. See the feature guide's
upgrade/rollback procedure before downgrading.

## Validation and CI selection

Focused Linux unit tests exercise shared matching, rule identity, OVN topology
and IP-family lifecycle, host state, conditions, and legacy assignment/restart
behavior. The condition tests include remote pods, multiple controller instances,
conflict retries, and startup cancellation. Pod-event coverage includes empty
namespace selectors matching unlabeled namespaces. Kernel checks accept the
generated IPv4/IPv6 CIDR-only and TCP/UDP/SCTP nftables rules. All 28 focused
TrafficClass admission specs pass against a local API server, including a
conditions-only patch before assignments. Full unit suites and E2Es have not
been run; compilation and unit fixtures do not prove live datapath behavior.

Static CI wiring is `.github/workflows/test.yml` -> `test/Makefile:control-plane`
-> `test/scripts/e2e-cp.sh` -> `test/e2e/egressip.go`:

- `control-plane` lanes enable `OVN_TRAFFIC_CLASS_ENABLE`, including
  shared IPv4/IPv6 and local IPv4/IPv6 configurations.
- `network-segmentation*` lanes enable the gate and select
  `Feature:NetworkSegmentation` through `WHAT="Network Segmentation"`.
- The traffic specs inherit `Feature:EgressIP`; they have no `Serial` label.
  They require three Ready nodes and a supported network configuration.
- Secondary-host cases intentionally skip UDNs. Cluster-default dual-stack setup
  chooses one address family; explicit UDN entries select their own family.
  The single-family control-plane lanes cover both cluster-default families.

These are selection paths, not evidence of actual CI execution. Check reports for
individual specs and skips before claiming CI runtime coverage.

The generic owner-refactor commit remains unchanged. Public API names and wire
fields follow the November OKEP. UDN host-CIDR policies migrate from 99 to 98 in
the OVN feature commit; host matching no longer generates destination routes.

Condition reporting follows the OKEP: ovnkube-controller uses cluster-wide
informer snapshots, including remote pods. Equivalent global results skip writes;
resource-version guards preserve concurrent status updates. Programming does not
depend on the condition boolean: both dataplanes use the same per-pod selection
utility. Admission defaults required assignment items to an empty array when a
conditions-only patch first creates status, preserving the writers' separate
field ownership.

No-transit L2 behavior has focused database fixtures, but both production
controller managers select the transit topology and expose no switch to create
the older topology. There is no corresponding E2E lane. Rolling upgrade,
gate-rollout and rollback restoration require live-environment validation.
