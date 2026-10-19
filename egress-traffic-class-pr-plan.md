# Bring PR #6237 into full compliance with OKEP-6227

## Current handoff — 2026-10-21

**Implementation and both review-fix passes are complete in the working tree;
validation is partial.** The five findings from the first review, its related
startup-cleanup defect, and the two findings from the second review have fixes
and regression coverage. See sections 11–12 for the findings and changes;
section 12 is authoritative for the latest validation evidence and limitations.

Branch: `eip_dest_routing`. HEAD/local WIP savepoint:
`f44571a0e929ce58401c54adaa373927992099f0`. Review fixes are uncommitted.

| Latest revision check | Status |
|---|---|
| EgressTrafficClass admission suite | Passed against a real API server, including new selector cases. |
| Focused Linux parser and traffic-condition tests | Passed. |
| Affected Linux test-binary compilation | Passed for all five packages; no runtime result implied. |
| Affected-package lint | Passed, zero issues. |
| Generated artifacts, API reference, formatting, whitespace | Completed/checked successfully. |
| OVN/node/rule runtime tests and race checks | No fresh confirmed pass; Podman stalled after the first two packages. |
| E2Es with strengthened assertions | Not run; explicitly deferred. |

**Current direction:** review/fix the implementation and maintain this plan;
leave further E2Es deferred and use GitHub CI for remaining validation. No
commit, deployment, VM restart, or PR operation is requested. CI results for
the uncommitted fixes have not been observed.

Next steps:

1. Validate the current fixes on working Linux/CI: outstanding focused OVN and
   node/rule regressions, the concurrent regression under the race detector,
   the broader OVN Egress suite, and remaining repository build gates. Section
   11 retains focused commands; section 12 records what has already passed.
2. Assess those results before claiming full validation. Keep E2Es deferred;
   earlier scenario counts do not validate the strengthened assertions.
3. Commit organization and PR preparation remain separate, unperformed work.

**Environment handoff:** Podman API/SSH access stalled. The disposable
`ovn-traffic-validation` container's stopped state could not be verified after
a bounded cleanup attempt. Historical cluster-health/cleanup records are not
current health checks. Preserve section 10's gateway pin and cluster setup:
`--gateway-interface=eth1 --gateway-nexthop=10.89.3.1`. Preserve unrelated local
files, especially `ovnk-kubeconfig`.

**How to read this document:** sections 1–5 describe the accepted scope, original
gaps, implementation requirements, and acceptance matrix; they are not an open
implementation backlog. Sections 6–10 retain dated execution history, including
superseded stop/resume instructions. Sections 11–12 record the review fixes.
This handoff governs next actions. `/tmp` evidence paths are local artifacts,
not durable CI links; preserve relevant logs before cleaning that directory.

## 1. Basis and scope

Treat [OKEP #6228 at `594aab133`](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/594aab1333435561e1079552eb8ba694ed826d09/docs/okeps/okep-6227-egress-traffic-class.md) as accepted. Complete [implementation #6237 at `4ee8f4354`](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6237) against current repository code, starting from `5ff49b328`.

The original draft implemented an earlier, CIDR-only design. The work retained
its useful address-set integration, destination routes, gateway preservation,
selector-change handling, and informer wiring while completing the missing
behavior and fixing lifecycle ownership.

Agreed interpretation: traffic conflicts follow the OKEP’s formal intersection rule. UDP destination 5060 and UDP source 16384–32767 can overlap because one packet can satisfy both.

Confirmed UDN interpretation: retain the usual EgressIP packet-mark/OVS SNAT pipeline, as reported by the user after clarification with the OKEP author. The OKEP’s gateway-router-SNAT wording does not require a UDN datapath redesign.

Preserve these boundaries:

- TCP, UDP, and SCTP only.
- UDN support covers EgressIPs on the OVN primary network; secondary-host UDN EgressIPs remain future work.
- Story 2 means selective secondary-host EgressIPs plus a default OVN-network EgressIP. The reverse combination remains unsupported.
- Conflicts produce the specified condition. Do not add rejection, fail-closed behavior, longest-prefix precedence, or another winner-selection policy.
- Do not add TrafficClass consumers, finalizers, or TrafficClass status absent from the accepted OKEP.

## 2. Original draft gaps

These were the gaps in the initial draft, before implementation and the review
fixes recorded below. They do not describe the current working tree.

| Area | Draft behavior and required correction |
|---|---|
| Public API | Still exposes `EgressIPTraffic`, `destinationNetworks`, and a value-type selector. Replace these with the OKEP’s `EgressTrafficClass`, structured `destinations`, and pointer selector. |
| L4 matching | Neither datapath implements protocol, source-port, or destination-port matching. Implement identical match semantics throughout steering and SNAT. |
| Secondary-host SNAT | [The generator](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/4ee8f435484bbec30683efb6559a69b34e3d9f7e/go-controller/pkg/node/controllers/egressip/egressip.go#L741) attaches one unscoped source/interface SNAT rule. Two EgressIPs sharing a pod and interface can therefore use the wrong source address. |
| Assignment ownership | [The coexistence branch](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/4ee8f435484bbec30683efb6559a69b34e3d9f7e/go-controller/pkg/ovn/egressip.go#L1085) retains one active EgressIP name and a pooled status map. Updates, failover, deletion, and restart cannot reliably distinguish multiple active owners. |
| Observability | Both EgressIP conditions are absent; the warning event uses the older `NoDestinationNetworks` reason. |
| Disabled gate | The draft interprets a populated selector despite the gate being disabled, producing empty-match/skip behavior. The OKEP requires ignoring the selector and retaining ordinary EgressIP behavior. |
| Repair and upgrade | Recognizing priorities 99 and 100 does not migrate valid old catch-all LRPs to 99. Destination address sets also need startup reconciliation and orphan cleanup. |
| Delivery and tests | Helm CRDs are missing the API changes. Existing additions cover six secondary-host E2E scenarios; controller helper tests do not establish complete reconciliation behavior. |

The latest reported implementation CI checks succeeded, apart from a skipped deployment job. That validates the draft’s tested scope, not compliance with the subsequently expanded OKEP.

## 3. Implementation requirements

### A. Update the API and deployment contract

- Introduce cluster-scoped `k8s.ovn.org/v1 EgressTrafficClass`, short name `etc`, with the exact destination/protocol/port structure from the OKEP.
- Enforce admission constraints: 1–25 destinations, canonical network CIDRs, exactly one protocol per entry, exactly one port number/range, valid port bounds, and ordered ranges.
- Make `EgressIP.spec.trafficSelector` a `*metav1.LabelSelector`; reject a populated field containing no selector entries.
- Add `status.conditions` using `metav1.Condition` and condition-list merge markers. Preserve existing EgressIP fields, including current node-selection behavior.
- Regenerate clients, deepcopy, OpenAPI, apply configurations, informers, listers, and CRDs through current code generation. Include Helm CRD copies.
- Rename the gate consistently to `--enable-egress-traffic-class`, default false, implying EgressIP. Update current deployment tooling, Helm values/documentation, environment wiring, RBAC, and Kind configuration.
- Remove draft-only names rather than maintaining an unreleased parallel API.

### B. Share destination interpretation

Extend the existing EgressIP utility package with the OKEP’s destination parsing and overlap helpers.

- Parse selected resources into structured CIDR/protocol/port matches; preserve OR between destinations/protocol entries and AND between source/destination ports.
- Deduplicate identical matches and route CIDRs. Sort generated output so informer iteration order does not cause configuration churn.
- Keep selector absence distinct from a selector resolving to zero matches.
- Resolve destinations once per EgressIP reconciliation; keep OVN, Linux-rule, and nftables rendering in their respective owners.
- Return errors for invalid inputs rather than silently dropping a restriction.
- Apply the feature gate consistently before interpreting selectors, coexistence, conditions, or destination programming.

This consolidation is worthwhile: the draft currently resolves and parses destinations separately in its two controllers.

### C. Fix active assignment ownership and OVN reconciliation

Replace the single-active-name model with active assignments keyed by EgressIP name. Each owner must retain its own statuses and applied pod/network information.

- Use the same owner-specific add/update/delete path for every active EgressIP. Remove the separate coexistence shortcuts.
- Preserve standby handling for additional catch-all EgressIPs; their mutual selection remains undefined.
- Handle pod IP replacement, namespace/pod selector changes, network changes, pending assignments, reassignment, and deletion independently for each owner.
- Remove shared served-pod state only after the last applicable active assignment is removed. Rebuild all active owners during restart recovery.
- Build one family-specific destination expression: CIDR-only address-set membership OR inline L4 clauses. Reuse that expression for reroute and default-network gateway SNAT.
- For UDNs, apply the same destination/L4 expression to the existing reroute and gateway-marking policies, preserving their topology-specific placement, packet-mark guards, and external-bridge OVS SNAT. Use those same match builders during startup repair so it cannot recreate broad marking policies. A nonmatching packet must not receive this EgressIP’s mark.
- Keep selector LRPs at 100 and every catch-all LRP at 99.
- Reuse one assignment-rebuild path for selector changes and TrafficClass matcher changes. Merely updating an address set cannot update inline L4 expressions.
- Update existing LRP matches explicitly: the current next-hop-add helper only mutates next hops.
- Reconcile TrafficClass additions/deletions and the union of old/new label matches. Destination-only updates need one reconciliation per affected EgressIP.
- Reconcile destination address sets at startup; retry failed cleanup and remove orphaned sets after their referencing policies are removed.

Keep ECMP, existing next-hop selection, and current controller/network ownership conventions.

### D. Complete the current secondary-host datapath

Use current nftables and comparable `IPRule` infrastructure, as agreed; do not restore the draft’s iptables implementation.

- Generate priority-6000 rules for each destination/protocol match, including source and destination port ranges. Catch-all rules use 6001.
- Extend `IPRule` with comparable protocol and port-range fields; update both netlink conversions, identity, diagnostic output, and repair. Use this identity throughout instead of adding a separate string-key convention.
- Retain the draft’s destination-route behavior: unique CIDRs, preserved default gateway, preserved administrator-specific routes, and correct family filtering.
- Retain shared routing tables per interface. Delete routes only when neither the updated EgressIP nor another owner needs them, including interface moves and selector removal.
- Serialize node configuration reconciliation while inspecting other owners’ mutable state. The draft’s unlocked cross-owner cache reads are unsafe.
- Add a dedicated selective-SNAT nftables chain, evaluated before existing catch-all maps. Its rules match pod source, output interface, destination CIDR, protocol, and specified ports.
- Rebuild that owned chain atomically from the current configuration, including startup repair and periodic reconciliation. Retain existing catch-all maps.
- Remove the draft’s “SNAT only on the first destination” workaround. Preserve current connmark and unsNATed-traffic protections.

Explicit selective nftables rules avoid overlapping interval-map complications while retaining the existing catch-all implementation.

### E. Add conditions with one cluster-wide writer

Use the existing cluster-manager EgressIP controller as the single writer of the two global conditions. Its address allocation, placement, and health-check algorithms remain unchanged.

- Compute `TrafficSelectorResolved` from selected TrafficClasses and destinations, using the OKEP’s reasons and messages.
- Emit the specified `NoTrafficClass` warning when a selector resolves to nothing.
- Compute `TrafficConflict` between different selective EgressIPs sharing actual selected pods. Intersect address family/CIDR, protocol, destination-port interval, and source-port interval.
- Do not report redundant matches within one EgressIP or overlapping definitions serving different pods.
- Reevaluate affected peers on EgressIP, TrafficClass, pod, and namespace changes, including deletion and label changes; clear stale conditions.
- Preserve transition times and avoid unchanged writes.
- Change assignment-status patches to preserve conditions. Existing whole-`/status` replacement would otherwise erase them.
- Preserve the existing EgressIP write endpoint for compatibility; introducing a status subresource would require a separate migration of legacy writers.

### F. Finish upgrade handling and documentation

- Reconcile owned catch-all LRPs from 100 to 99, including their priority external IDs, without retaining duplicate old/new policies.
- Repair Linux priorities 6000 and 6001 and remove stale rules using their complete identity.
- Cover changes made while controllers were down: deleted/relabeled TrafficClasses, changed matchers, selector removal, and deleted EgressIPs.
- Document rolling-upgrade behavior and the cleanup required before rollback. Do not claim old binaries understand priority 99 or 6001.
- Replace the draft feature guide with `docs/features/cluster-egress-controls/egress-traffic-class.md`; include all four stories, L4 examples, conditions, troubleshooting, and stated limitations.
- Generate API references and update navigation/API introductions. Treat the OKEP itself as already merged.
- Keep refactoring limited to shared matching, assignment ownership, common rebuild logic, and safe shared-route handling.

## 4. Validation and acceptance

This is the full acceptance target. It is not a record of passing checks or an
instruction to resume E2Es; use the current handoff and dated evidence below.

**Admission:** Add envtest coverage in the existing CRD integration suite for the complete API, invalid unions/ranges/CIDRs, destination limits, absent versus empty selectors, and condition round-tripping.

**Focused product tests:**

- Destination parsing, deduplication, family filtering, and formal overlap semantics.
- Actual controller reconciliation producing the expected LRPs/NATs, including mixed CIDR/L4 matches and zero matching classes.
- Multiple active owners across creation order, owner deletion, pod IP replacement, reassignment, restart, and selector/class changes.
- Conditions becoming set and clearing without losing assignment items or unrelated status.
- Linux L4 rule round-tripping, equality, repair, and independent deletion.
- Exact nftables matches and source addresses for two EgressIPs sharing one pod/interface.
- Shared-route retention/removal, interface moves, failed operations/retries, and concurrent reconciliation under the race detector.
- Migration fixtures containing old priority-100 LRPs and priority-6000 catch-all rules.

Replace tests that repeat token splitting or call only generic NAT builders with tests exercising the production reconciliation paths.

**E2E:** Complete all twelve OKEP scenarios: matching/non-matching traffic; multiple selective EgressIPs; two EgressIPs on one secondary interface; default coexistence; destination update; label update; selector addition; matching port; non-matching port; mixed destinations; destination-port range; source-port range.

Run these in local and shared gateway modes, covering IPv4 and IPv6. Also verify selective primary-network EgressIP behavior and required UDN behavior. Exercise failover and controller restart with multiple active owners. Ensure feature-enabled CI lanes actually run the tests; disabled lanes must skip intentionally.

For supported Layer 2 and Layer 3 UDN topologies, verify that matching traffic receives the selected EgressIP mark and is SNATed by the existing OVS pipeline, while nonmatching traffic follows normal egress. Verify independent selection for multiple EgressIPs and preservation of these matches through live updates and restart repair.

Run focused suites first, then repository-required build/lint/unit checks, `make test-crd`, Helm rendering, documentation build, and relevant networking E2Es. Preserve existing EgressService/no-reroute/QoS precedence and the OKEP’s acknowledged UDN host-CIDR priority limitation.

## 5. Resolved UDN clarification

**Decision: extend the existing UDN packet-mark/OVS SNAT pipeline.**

The user reported confirmation from the OKEP author that the intention is to keep the usual UDN pipeline. The phrase “SNAT on the gateway router” in the OKEP’s UDN goal is a wording discrepancy, not a requirement to introduce a new UDN datapath. This clarification concerns UDN traffic only.

The implementation retains EgressIP-specific packet marks and external-bridge
OVS SNAT, and extends destination/L4 selection through steering, marking, and
repair. Selective UDN tests are implemented; execution evidence and remaining
validation gaps are recorded below. No open OKEP intent questions remain
identified by these reviews.

## 6. Execution record

Implementation was recorded as complete on 2026-10-19. The later reviews in
sections 11–12 found correctness and coverage gaps. At that earlier checkpoint,
the E2E scenarios were implemented and compiled, with execution deferred at the
user's request. Later authorizations and results are recorded in sections 8–10.
The user requested a local WIP commit at this checkpoint; no PR was created.

### Delivered

- API, admission validation, generated clients/informers/apply configurations,
  Helm CRDs, feature gate, deployment environment, RBAC, Kind/CI wiring, and API
  and feature documentation.
- Shared destination parsing, deduplication and formal CIDR/L4 overlap checks.
- Cluster-manager condition reconciliation, relevant event handling, warning
  events, and merge patches that preserve assignments and unrelated conditions.
- Per-owner OVN assignment lifecycle, destination address sets, L4 steering and
  SNAT, catch-all coexistence, startup migration and cleanup, and gate-off repair.
- Existing UDN packet-mark/OVS SNAT pipeline, extended with destination/L4
  matching during creation, updates and startup repair.
- Comparable Linux L4 policy rules, destination routes with gateway and peer
  preservation, serialized node reconciliation, and atomic selective nftables
  SNAT reconciliation before catch-all maps.
- Admission, parser, condition, controller, native Linux datapath and lifecycle
  tests, plus the twelve E2E scenarios and restart/failover coverage.

### Implementation details established during validation

- Selective OVN NAT priority is 100, above fallback NAT. UDN gateway marking
  uses 96 for selective matches and 95 for catch-alls; reroute priorities remain
  100 and 99 respectively. These priorities preserve selective precedence at
  both steering and SNAT/marking stages.
- The installed OVN expression parser rejects the proposal's example range
  notation `udp.src == 16384..32767`. Render equivalent inclusive comparisons
  instead: `(udp.src >= 16384 && udp.src <= 32767)`.
- Retain the legacy EgressIP write endpoint. Conditions intentionally omit
  optional `observedGeneration`: this endpoint increments generation on status
  writes, as verified against a real API server. Publishing it would immediately
  make it stale or cause repeated writes. Assignment and condition writers use
  merge patches; condition patches carry a resource-version precondition.
- Startup gateway-mark repair now recreates missing rules, scopes pod and
  assignment data to each owner/network/IP family, preserves applicable
  topology behavior, and accumulates the operations it transacts. Restart cache
  generation includes all selected namespaces sharing a network.
- Feature environment variables feed CLI configuration directly. Existing user
  deletions under `dist/`, including the old entrypoint script, were preserved.

### Validation completed

| Check | Result |
|---|---|
| Compile all `go-controller` packages | Passed on Linux. |
| `make build` | Passed on Linux. |
| Pinned `golangci-lint` 2.12.2 | Passed with zero issues; existing unknown-`gomnd` warning remains. |
| OVN Egress unit suite | Passed. |
| Selective OVN reconciliation with race detector | Historical pass: default, L2 without transit router, and L3 UDN, separately for IPv4 and IPv6. Sequential tests covered owner lifecycle and selected repair helpers. They did not exercise concurrent handlers, dual-stack, default L2 transit-router behavior, full startup synchronization, or actual node/family reassignment; see section 11. |
| Affected full package suites | Passed: cluster-manager, config, factory, shared EgressIP utilities, node EgressIP controller and IP-rule manager. |
| Focused race checks | Passed for the changed condition, configuration, informer, shared matching, OVN and node/rule paths. |
| Native Linux node lifecycle | Passed under the race detector: shared-interface owners, destination edits, interface move, injected API failure and retry, peer route/SNAT preservation, deletion and restart repair. |
| Native nftables syntax | Passed for IPv4/IPv6, TCP/UDP/SCTP and port ranges. |
| CRD integration via envtest | All 30 admission/status specs passed. |
| Code generation | Regenerated through repository tooling; repeated generation left API and Helm CRD outputs unchanged. |
| Helm and Kind | Normal and DPU Helm rendering and Kind shell syntax checks passed. |
| Documentation | Website build passed with existing repository warnings. |
| E2E compilation | Test binary compiled successfully. No E2E specs executed at this checkpoint; later results are in sections 8–10. |
| Patch whitespace | `git diff --check` passed. |

The full factory suite requires `KUBE_FEATURE_WatchListClient=false` with the
current fake clients: an existing AdminNetworkPolicy fixture otherwise waits
for WatchList bookmark events. The new EgressTrafficClass informer test also
passes without that workaround; no unrelated product behavior was changed.

Validation logs are under `/tmp/ovn-traffic-*.log` in this workspace session.
The full repository unit suite was not run; all packages were compiled and the
affected suites were exercised as listed above.

### Remaining joint validation

The historical instruction at this savepoint was **do not run E2Es
automatically**. The user later authorized the runs in sections 8–10; follow
the latest stop instruction above. Joint validation must establish real packet behavior in
local/shared gateway modes and supported IPv4/IPv6 and UDN topology lanes,
including nonmatching traffic, source-port ranges, failover and restart.
Use the lane-specific results below to determine what remains unverified.

## 7. Savepoint and continuation

The user requested that work stop at this checkpoint and continue in a later
session. Product implementation and non-E2E validation were considered
complete at that historical checkpoint; sections 11–12 supersede that assessment.
No OKEP intent questions remain open. The WIP commit is a local savepoint, not a
claim that cluster-level validation has passed.
The savepoint includes the feature changes and this plan. Pre-existing
`dist/` deletions, `.DS_Store`, `.codegraph/`, `pods/`, `es.yaml`, `p.yaml`
and `pool.yaml` remain outside the feature commit.

Original continuation checklist (superseded by section 10 where applicable):

1. Review the WIP diff together, including the API/deployment contract and
   priority/migration changes. The repository requires the submitting author
   to understand and review AI-assisted changes before submitting them.
2. Agree on and prepare the Kind cluster matrix for local/shared gateways,
   IPv4/IPv6 and supported UDN topologies. Start E2Es only with the user.
3. Run the selective EgressIP scenarios in `test/e2e/egressip.go`, including
   nonmatching traffic, protocol/source/destination ports, shared interfaces,
   default coexistence, live matcher changes, restart and failover. Exercise
   ordinary EgressIP regression scenarios in the same lanes.
4. Fix any failures and rerun the affected checks. Once the cluster results
   support readiness, organize the WIP into reviewable signed-off commits and
   prepare a PR description with validation evidence and AI-use disclosure.

Session resources: validation logs are in `/tmp/ovn-traffic-*.log`; tools are
in `/tmp/ovn-traffic-tools`; the docs virtual environment is
`/tmp/ovn-traffic-docs-venv`. The disposable Linux validation container is
`ovn-traffic-validation`, with source mounted at `/src` and a persistent Go
build cache. It is stopped at the checkpoint; restart or recreate its test
services as needed in the next session. No E2E cluster was created.

## 8. Cluster E2E validation — 2026-10-20

The user supplied `ovnk-kubeconfig` and explicitly authorized running and
iterating on E2Es. This resumes the work paused in section 7.

### Cluster and preparation

- Three healthy, schedulable Kind nodes: `ovn-dev-control-plane`,
  `ovn-dev-worker`, and `ovn-dev-worker2`, running Kubernetes v1.36.4 on arm64
  through Podman.
- IPv4, shared gateways, cluster default network. Multi-network and network
  segmentation are disabled, so this run does not establish UDN behavior.
- Deployed image: `localhost/ovn-daemonset-fedora:dev`; its binary reports
  commit `3df5e75a6fd8b5422f7f68ab7cd3ca2c60cf017f` on `eip_dest_routing`.
- Both CRDs were already installed. Set
  `OVN_EGRESS_TRAFFIC_CLASS_ENABLE=true` on the `ovnkube-controller` container
  in `daemonset/ovnkube-node` and `ovnkube-cluster-manager` in
  `deployment/ovnkube-control-plane`, then verified both rollouts.
- Rebuilt the native E2E binary from the current checkout. Tests use the
  supplied kubeconfig, `CONTAINER_RUNTIME=podman`,
  `KIND_EXPERIMENTAL_PROVIDER=podman`, and `--provider=local --num-nodes=3`.
  Tests ran sequentially with fail-fast and no automatic flake retries.

### Results

All **28 distinct selective scenarios passed**: the twelve OKEP scenarios plus
multi-owner restart and failover, each on the OVN primary interface and a
secondary host interface. This includes external probes verifying the actual
source IP for matching and nonmatching traffic, coexistence, class/selector
changes, L4 matches and ranges, and peer preservation after owner deletion.

Two earlier runs stopped in the common `BeforeEach`, before the scenario's
assertions: a remote Podman `ip6tables --check` returned exit code 1 with empty
output instead of its normal missing-rule diagnostic. Both failures occurred
immediately after successful restart scenarios. The cluster remained healthy;
the primary multi-owner case passed on its individual rerun.

The common rule-check helper in `test/e2e/util.go` now redirects stderr to
stdout inside the node before transport. It continues to fail on unexpected
errors rather than accepting every nonzero status. With that change, a fresh
six-case run passed, including secondary restart immediately followed by the
previously failing setup/case, and all remaining scenarios. No product-code
change was needed for the tested datapaths.

All three existing EgressIP regression cases passed: ordinary catch-all
overlap/standby ownership, host-network destinations, and multiple secondary
EgressIPs sharing an interface. Their final run completed in 101 seconds;
`/tmp/ovn-traffic-e2e-regression.json` contains the report.

Evidence is retained in `/tmp/ovn-traffic-e2e-*.log` and the corresponding
Ginkgo JSON reports. `/tmp/ovn-traffic-e2e-results-oct20.json` lists the 28
distinct passes and both initial setup failures; the successful six-case
report is `/tmp/ovn-traffic-e2e-final-selective.json`.

The only repository changes from this E2E session are the rule-check
diagnostic adjustment and this progress record; no additional commit has been
created. The kubeconfig and unrelated local files remain untracked. The test
cluster is retained with the traffic-class gate enabled.
Post-run verification found all nodes Ready and all OVN workloads healthy,
with no remaining EgressIP/TrafficClass resources or external test containers.

### Outstanding at this checkpoint (historical)

- Run the same selective matrix on IPv6 and local gateways.
- Run supported L2/L3 primary UDN cases with multi-network and segmentation
  enabled, checking the existing packet-mark/OVS SNAT pipeline. Secondary-host
  UDN cases remain explicitly unsupported.
- Review the WIP and runtime results with the submitting author, then organize
  reviewable commits and the PR description with AI-use disclosure.

These results establish the IPv4/shared-gateway/default-network lane only;
they do not imply the remaining cluster configurations have passed.

## 9. UDN deployment and partial validation — 2026-10-20

The user redeployed and authorized UDN testing. Preflight verified three Ready
nodes; healthy Multus and OVN workloads; NAD, UDN and traffic-class CRDs; and
multi-network, network-segmentation and EgressTrafficClass gates enabled on
both controllers. The binary still reports implementation commit `3df5e75a6`.
The cluster remains IPv4 with shared gateways. The runner additionally needs
`ENABLE_NETWORK_SEGMENTATION=true`; without it, the UDN cases skip.

### Fixture adjustments established by this deployment

OVN's annotated primary/gateway network is `underlay` (`10.89.3.0/24`), while
the node InternalIPs and the Kind network use `10.89.0.0/24`. The original
fixture put external servers on Kind, and the first L3 probe observed the
inter-network masquerade address `10.89.0.1` instead of the assigned
`10.89.3.200`. Such a path obscures the source address the E2E must verify.

The EgressIP fixture now discovers the provider network containing the node's
annotated OVN primary address, using existing provider APIs, and places the
external servers there. Selective tests compare nonmatching traffic with the
pod node's annotated egress address, rather than assuming it equals InternalIP.
Both L2 and L3 matching/nonmatching smoke cases then passed without changing
the deployment or feature code.

The empty-output Podman rule-check failure also recurred, proving the earlier
stderr-redirection adjustment insufficient. The helper now returns the
iptables child status explicitly in stdout while the transport command exits
successfully, then validates that status and the original diagnostic in Go.
Unknown errors and missing/malformed status still fail. Direct checks verified
success (0), missing rule (1), and unexpected invalid-option error (2).

### Confirmed results before infrastructure stalled

Five distinct selective UDN scenarios passed:

| Topology | Passed scenarios |
|---|---|
| IPv4 L3 primary | Matching/nonmatching destinations; catch-all coexistence; destination-port range; destination update. |
| IPv4 L2 primary | Matching/nonmatching destinations. |

The current matrix stopped progressing in the L3 label-update `BeforeEach`:
the runner's child command `podman network ls --format {{.Name}}` hung.
Independent `podman ps` and VM SSH (`uptime`) hung too, and a Kubernetes API
query with a ten-second request timeout failed with `context deadline exceeded`.
The VM was listed as running (4 CPUs, 15.62 GiB RAM); its underlying failure
cause has not been established. The agent interrupted its E2E runner and did
not restart the shared VM or claim the remaining scenarios passed.

Evidence:

- `/tmp/ovn-traffic-udn-smoke-initial.json`: original fixture/source mismatch.
- `/tmp/ovn-traffic-udn-smoke-aligned.json`: both topology smoke tests passed.
- `/tmp/ovn-traffic-udn-selective.json`: first matrix stopped on the setup flake.
- `/tmp/ovn-traffic-udn-selective-stable.log` and its JSON report: matrix with
  explicit rule-check status capture, three further L3 passes, then interruption.

### Historical recovery checklist (superseded by section 10)

1. Restore Podman VM/API and Kubernetes API access; verify the kubeconfig's
   forwarded endpoint still matches the cluster.
2. Check nodes, OVN and Multus health, gates and any namespace/resources left
   by the interrupted case. Cleanup could not be verified during the outage.
3. Rerun the topology smoke tests, then the 23 uncompleted selective scenarios.
   L3 label updates and all UDN restart/failover scenarios are still outstanding.
4. Complete the IPv6 and local-gateway lanes, then author review and PR preparation.

UDN validation is **incomplete**. Only test fixtures/helpers and this plan have
changed during the cluster sessions; no feature-code fixes or new commits have
been made. Local kubeconfig and unrelated files remain outside the changes.

## 10. Historical handoff — gateway-pinned UDN rerun, 2026-10-20

### User boundary and working tree

This boundary applied to the October 20 rerun. Later review/fix instructions
supersede its requirement to stop all work; E2Es remain deferred.

The user authorized live testing, supplied the kubeconfig below, and then
requested: **"after this rerun, stahp"**. Finish only this rerun, verify/perform
cleanup of its resources, record its outcome here, and stop. A later agent
must wait for a new user instruction before further tests or fixes.

- Branch: `eip_dest_routing`.
- HEAD/local WIP savepoint: `3df5e75a6fd8b5422f7f68ab7cd3ca2c60cf017f`.
- Uncommitted source changes already present when this session began:
  `test/e2e/egressip.go` (external-probe network selection and annotated egress
  source expectations) and `test/e2e/util.go` (iptables status capture).
  Sections 8–9 explain them. Preserve these changes.
- This session changes this plan and the live Helm gateway configuration;
  it has made no product/test-source edits, commits, or PRs.
- Do not stage unrelated/untracked files, especially the kubeconfig. Existing
  local files include `.DS_Store`, `.codegraph/`, `contrib/kind-ovn-dev.yaml`,
  `es.yaml`, `oom`, `ovnk-kubeconfig`, `p.yaml`, `pods/`, and `pool.yaml`.

### Cluster access and topology

- Kubeconfig: `/Users/obraunsh/projects/ovn-kubernetes/ovnk-kubeconfig`.
  Context: `kind-ovn-dev`; cluster: `ovn-dev`.
  Always pass this kubeconfig explicitly or export its absolute path. The
  inherited `KUBECONFIG` pointed to an unrelated cluster-bot file with no
  current context. Do not rely on `~/.kube/config` either.
- Three nodes, Kubernetes `v1.36.4`, arm64, shared gateways, IPv4 pod networking,
  Podman runtime. No cluster recreation was performed.
- Helm release `ovn-kubernetes`, release namespace `default`; workloads are
  in `ovn-kubernetes`. Image: `localhost/ovn-daemonset-fedora:dev`.
- Multi-network, network segmentation, EgressTrafficClass, and EgressIP are
  enabled on cluster-manager and the combined controller/node container.
  Multus is installed. UDN/CUDN/EgressTrafficClass CRDs are established, and
  the EgressIP schema includes `spec.trafficSelector`.
- Node InternalIPs/Geneve use Kind network `10.89.0.0/24`. OVN's gateway and
  annotated primary addresses must use Podman network `underlay`, subnet
  `10.89.3.0/24`, next hop `10.89.3.1`, physical interface `eth1`, OVS bridge
  `breth1`. Expected primary addresses are `10.89.3.3/24` on
  `ovn-dev-control-plane`, `10.89.3.4/24` on `ovn-dev-worker`, and
  `10.89.3.2/24` on `ovn-dev-worker2`. These two networks are intentional.
- External test servers must be on the annotated OVN egress network;
  `egressip.go` discovers that provider network. Otherwise inter-network
  Podman masquerading hides the source IP being asserted.

### Restart failure diagnosed and environment change applied

The first run after cluster recovery selected all 28 IPv4/shared primary
L2/L3 OVN-network traffic-selector cases. Seven L3 cases passed, two failed,
and the next case was interrupted; the L2 cases were not reached in that run.

1. L3 matching/nonmatching destinations passed its traffic assertions but
   failed in `AfterEach`: `missing ip6tables check status on node
   ovn-dev-worker2: ""`, from `updateIPTablesRulesForNode` in
   `test/e2e/util.go`. Remote Podman can still return completely empty output
   even with the existing child-status wrapper. No further helper fix has
   been made; do not silently treat missing output as success.
2. L3 multi-owner restart timed out expecting source `10.89.3.210`; it saw
   ordinary node egress instead. `OVN_GATEWAY_OPTS` was empty, so restart
   auto-detected the host default route on `eth0`/`10.89.0.0/24`, replacing the
   worker's primary/gateway annotation with `10.89.0.4`. Its old EgressIPs
   `.3.210` and `.3.211` remained on `breth1`; cluster-manager rejected them
   as host-address conflicts and left both EgressIP assignment lists empty.
   Node logs also showed a missing UDN patch port and one controller startup
   restart. This run did not isolate an EgressTrafficClass product defect.

Stopped that run because its allocator caches node subnets at `BeforeSuite`;
continuing after gateway-network drift would give misleading later results.
Cleanup removed its namespaces, EgressIP/TrafficClass objects, labels, external
containers, and secondary network. After verifying no EgressIPs remained,
manually removed only its leaked `10.89.3.210/32` and `.211/32` addresses from
the worker's `breth1`. The automatically created `breth0` was left intact;
the worker's host default route uses it. Do not delete it casually.

First-run evidence:

- `/tmp/ovn-traffic-udn-redeploy-live.log`
- `/tmp/ovn-traffic-udn-redeploy-results.json`
- `/tmp/ovn-traffic-udn-redeploy-junit.xml`

Pinned the original OVN gateway through Helm, preserving other release values:

```sh
helm --kubeconfig /Users/obraunsh/projects/ovn-kubernetes/ovnk-kubeconfig \
  upgrade ovn-kubernetes ./helm/ovn-kubernetes --namespace default \
  --reuse-values \
  --set-string 'global.gatewayOpts=--gateway-interface=eth1 --gateway-nexthop=10.89.3.1' \
  --wait --timeout 5m
```

This is Helm revision 2. Pre-change user values are saved at
`/tmp/ovn-traffic-gateway-before.yaml`. No node-local `ovn-gw-interface` or
`ovn-gw-nexthop` OVS external-ID overrides were present. All three node pods
finished rolling and were 6/6 Ready; all primary annotations returned to the
expected `10.89.3.x/24` addresses before the rerun started.

**Preserve the pin on future deployment.** `contrib/kind.sh --deploy` defaults
`OVN_GATEWAY_OPTS` to empty and can overwrite the Helm value. Supply
`OVN_GATEWAY_OPTS='--gateway-interface=eth1 --gateway-nexthop=10.89.3.1'`
along with the existing cluster name, kubeconfig, Podman provider, UDN and
EgressTrafficClass options. The code paths are `getGatewayNextHops` in
`go-controller/pkg/node/gateway_init.go`, `get_gw_options` in
`dist/images/ovnkube.sh`, and `global.gatewayOpts` wiring in
`contrib/kind-helm.sh` and the single-node-zone Helm template.

### Final rerun result and exact reproduction

The gateway-pinned rerun completed all **28 selected specs in 844.506 seconds**,
with **25 passed and 3 failed**, exit code 1. No retries were configured or run.
The other 1,257 skipped and 2 pending specs are outside the selected lane.

| Topology | Passed | Failed | Result |
|---|---:|---:|---|
| IPv4 L3 primary, shared gateway | 14 | 0 | All twelve scenarios plus multi-owner restart and failover passed. |
| IPv4 L2 primary, shared gateway | 11 | 3 | Restart/failover passed; the three failures below are in the common setup/cleanup helper. |

All three failures report `missing ip6tables check status ...: ""` at
`test/e2e/util.go:1234`, inside `updateIPTablesRulesForNode`:

| L2 scenario | Failed phase | Node | What remains unverified |
|---|---|---|---|
| `catch-all coexistence` | `BeforeEach` | `ovn-dev-worker2` | Scenario body did not run in this rerun. |
| `label update` | `BeforeEach` | `ovn-dev-worker` | Scenario body did not run in this rerun. |
| `two owners on one interface` | `AfterEach` | `ovn-dev-worker2` | Traffic assertions passed, but cleanup did not complete inside the test. |

**No datapath assertion failed in this rerun.** That does not turn the suite
green or establish the two scenarios blocked in setup. The gateway pin
prevented network drift across both topology restart cases; final primary and
gateway annotations remained on `10.89.3.0/24`, bridge `breth1`, next hop
`10.89.3.1`.

Final cleanup/health verification found:

- All three nodes Ready, all OVN node pods 6/6 Ready, and all listed cluster
  pods (including Multus) ready. The restarted worker pod is
  `ovnkube-node-bkwk2` with zero container restarts.
- No EgressIP or EgressTrafficClass objects, no `e2e-framework=egressip`
  namespaces, no external test containers, and no `secondarynetwork` network.
- No test EgressIP addresses left on any node. Only the existing three Kind
  node containers are running; unrelated existing provider networks remain.
- The final cleanup failure left `k8s.ovn.org/egress-assignable=dummy` on
  `ovn-dev-worker`. It was absent before testing; manually removed that
  test-only label and verified no nodes retain it.
- Helm revision 2 and the pinned gateway configuration are retained. The
  cluster and shared Podman VM were not deleted or stopped.

**Work is stopped at the user's requested boundary.** No retries, helper
changes, further matrix runs, commits, or PR work followed this rerun.

The native binary was rebuilt from the current checkout into
`/tmp/ovn-traffic-udn-redeploy.test`. To rebuild if source changes in a later,
authorized session, run from `test/e2e`:

```sh
env GOTOOLCHAIN=local GOCACHE=/tmp/ovn-traffic-gocache \
  GOMODCACHE=/tmp/ovn-traffic-modcache /opt/homebrew/bin/go \
  test -c -o /tmp/ovn-traffic-udn-redeploy.test .
```

Use the explicit Go path: it is Go 1.26.1; a bare `go` routed through the local
command wrapper selected Go 1.22.5 during an attempted build. `go.mod` requires
Go 1.26. The binary was dry-run checked to select exactly 28 relevant specs.
Run the following only with a new user authorization after this checkpoint:

```sh
env KUBECONFIG=/Users/obraunsh/projects/ovn-kubernetes/ovnk-kubeconfig \
  CONTAINER_RUNTIME=podman KIND_EXPERIMENTAL_PROVIDER=podman \
  ENABLE_NETWORK_SEGMENTATION=true OVN_GATEWAY_MODE=shared \
  PLATFORM_IPV4_SUPPORT=true PLATFORM_IPV6_SUPPORT=false \
  /tmp/ovn-traffic-udn-redeploy.test \
  -ginkgo.no-color -ginkgo.v \
  -ginkgo.focus='IPv4 L[23] role primary.*\[OVN network\].*\[traffic-selector\]' \
  -ginkgo.label-filter='Feature:EgressIP' -ginkgo.fail-on-empty \
  -ginkgo.timeout=45m -ginkgo.flake-attempts=1 -test.timeout=50m \
  -ginkgo.json-report=/tmp/ovn-traffic-udn-pinned-results.json \
  -ginkgo.junit-report=/tmp/ovn-traffic-udn-pinned-junit.xml \
  -provider=skeleton \
  -kubeconfig=/Users/obraunsh/projects/ovn-kubernetes/ovnk-kubeconfig \
  -kubectl-path=/usr/local/bin/kubectl \
  -report-path=/tmp/ovn-traffic-udn-pinned-diagnostics \
  > /tmp/ovn-traffic-udn-pinned-live.log 2>&1
```

Fourteen scenarios run per topology: the twelve scenarios in section 4 plus
multi-owner restart and failover. Secondary-host UDN EgressIPs are unsupported
and deliberately excluded. This command runs one process with no flake retries.
The suite reports unrelated pending/skipped specs; count selected `It` results,
not passed `ReportAfterSuite` nodes in the JSON report.

The image-preload step warns that the installed Kind cannot load an archive
into containerd config version 4 (it supports 2/3). That warning did not prevent
the observed tests from creating containers and probing traffic; no Kind
upgrade or unrelated infrastructure fix was attempted.

### What remains, only if the user asks to resume

1. Investigate the recurring empty-output Podman setup/cleanup failure before
   rerunning the three named L2 cases. Relevant call path:
   `test/e2e/util.go:updateIPTablesRulesForNode` →
   `test/e2e/infraprovider/providers/kind/kind.go:ExecK8NodeCommand` →
   `test/e2e/infraprovider/engine/container/engine.go:ExecContainerCommand` →
   `test/e2e/infraprovider/engine/container/ops/ops.go:ExecContainerCommand` →
   `test/e2e/infraprovider/engine/runner/directrunner.go` (uses
   `exec.Command(...).CombinedOutput()`) → remote Podman. The existing helper
   already combines stderr and emits the child status in stdout, yet transport
   occasionally returns the empty string without an error. Do not weaken the
   unknown-error/missing-status checks or classify these as datapath failures.
   No root-cause fix for that transport behavior is established yet.
2. Recheck access/health and the gateway pin before resuming. Preserve unrelated
   local changes. Rebuild the binary if its source has changed. Choose new
   report filenames to retain the two runs recorded here.
3. Rerun the affected L2 cases after the helper/transport issue is resolved;
   rerun a broader lane only if a new change or failure warrants it. Do not
   assume the setup-blocked cases passed based on the other 25 results.

- IPv6 and local-gateway lanes remain unvalidated. Section 8 proves only the
  earlier IPv4/shared/default-network lane; this rerun covers only
  IPv4/shared primary L2/L3 UDNs, not the complete matrix in section 4.
- Author review, commit organization, and PR preparation remain outstanding.
  No additional commit or PR has been created. Stop here until instructed.

## 11. Implementation review fixes — 2026-10-21

This section records the first review pass. Section 12 contains the current
status and supersedes earlier readiness claims and continuation instructions.
Base of this follow-up: local WIP commit `f44571a0e929ce58401c54adaa373927992099f0`.
Changes below are uncommitted. Existing untracked files were preserved.

### Findings, changes, and regression evidence

1. **P1 — TrafficClass reconciliation could replay an older EgressIP.**
   `reconcileEgressTrafficClass` previously listed full objects before acquiring
   owner locks and used those snapshots afterward. A newer selector, workload
   selection, or assignment could be overwritten by the older class work.
   It now uses listed names only and fetches/checks the latest EgressIP under
   its lock, including deletion and current selector matching. Rebuild cleanup
   includes statuses from the applied per-pod owner cache, not only the API
   snapshot, so an interrupted/older assignment is not left behind.
   `TestSelectiveTrafficClassConcurrentEgressIPUpdate` uses channels to stop a
   class reconciliation after its list, completes another owner's selector
   update or deletion, then resumes and checks the resulting address sets.
   This establishes the ordering deliberately rather than relying on a race
   detector to happen to schedule a race.

2. **P2 — Destination routes discarded forwarding preferences and attributes.**
   The old helper chose the last default gateway and constructed an otherwise
   empty route. It could lose metrics, MTU, flags, and alternate defaults, and
   override a covering administrator route with a more-specific route through
   the default gateway. It now copies all alternatives from the most-specific
   original covering prefix, changes only the destination, and retains existing
   non-default routes. Exact destination routes are not duplicated. This is
   ordinary host route preservation, not a new precedence policy between
   overlapping EgressIPs.
   `TestDestinationRoutesPreserveForwarding` checks IPv4/IPv6, reversed default
   enumeration order, different metrics/gateways, MTU/on-link flags, covering
   prefixes, and exact-prefix alternatives. Existing peer-route checks remain.

3. **P2 — Unrelated pod churn caused global scans and live GETs for every EIP.**
   Pod/namespace events now check whether either old or new workload selection
   can affect a selective owner before queuing the global condition pass.
   Catch-all owners are excluded from pairwise overlap computation. Unchanged
   conditions are compared against informer state before making API calls;
   actual writes retain the live GET, UID/spec checks, resource-version guard,
   retry behavior, and preservation of unrelated conditions/assignment items.
   The lifecycle test now includes a catch-all, an unrelated pod, informer
   delivery of status writes, and an explicit assertion of zero client actions
   on unchanged reconciliation. Relevant events still recompute global overlap;
   this change does not claim an incremental conflict index or benchmarked
   scale limit.

4. **P2 — Several E2Es could pass without proving their named behavior.**
   The scenario table now separates names from typed filter/action choices;
   behavior no longer depends on substring matching in a test name.
   - Selector addition checks selected and excluded destinations before/after.
   - The range scenario uses UDP and checks both source-port endpoints plus
     ports immediately outside the range. Its external server enables UDP.
     Initial positive retries rotate interior source ports to avoid stale
     pre-convergence conntrack SNAT; boundary checks use exact source ports.
   - Destination-port ranges check inclusive boundaries and exclusions.
   - Nonmatching-port tests first establish a working matching endpoint.
   - Mixed destinations check L4 exclusion while retaining CIDR-only traffic.
   - Class deletion explicitly verifies that the selected path disappears.
   - Multiple secondary owners use two separately attached networks/interfaces;
     the same-interface case remains separate and checks peer survival.
   - Probe commands return an explicit child exit marker. Missing output,
     kubectl/exec errors, and unexpected command failures cannot satisfy a
     negative assertion. TCP exclusion accepts connection failure/timeout;
     UDP exclusion requires a successfully executed probe with no response.
   No E2E bodies were executed for this follow-up. Earlier results do not
   validate these newly added assertions or the UDP/two-interface setup.

5. **P2 — Unit-test claims exceeded actual reconciliation coverage.**
   The OVN matrix now includes L2 both with and without its transit router,
   L3 and default networks, single-stack families, and dual-stack pods with
   independently assigned IPv4/IPv6 owners. Initial LRP, NAT, and gateway-mark
   expectations contain independently constructed full matches and priorities.
   Tests now invoke full `syncEgressIPs`, change assignment family through the
   EgressIP handler, and move a default-network assignment to a remote node.
   An injected served-pod address-set failure occurs after OVN programming;
   retry must converge while preserving peers. The controlled concurrent
   selector/delete test in item 1 covers the reported stale-snapshot ordering.
   These are focused regression additions, not a claim of exhaustive event
   scheduling or all possible OVN/nftables fault permutations.

**Related inherited restart defect fixed while adding full startup coverage:**
reroute cleanup used to iterate only owner/network pairs with currently selected
namespaces. An owner selecting no namespaces could leave old reroutes behind.
Cleanup now visits each known network/router, verifies EgressIP ownership, and
removes rows without a current owner/network selection. Served-pod address sets
are reset for known networks even when their desired membership is empty. The
matrix includes a full startup after all namespace selectors become unmatched.
This closes the reviewed case; it does not claim a general orphan-network
cleanup redesign.

### Validation for this follow-up

- `gofmt` and `git diff --check`: passed.
- Linux cross-compilation of `pkg/clustermanager`, `pkg/ovn`,
  `pkg/node/controllers/egressip`, and `pkg/node/iprulemanager`: passed.
  Command uses `GOOS=linux CGO_ENABLED=0 go test -exec=true ... -run '^$'`.
  **This compiles test binaries; it does not execute tests.**
  Log: `/tmp/ovn-traffic-review-linux-build.log`.
- Updated native E2E binary: compiled successfully as
  `/tmp/ovn-traffic-review-e2e.test`.
  Log: `/tmp/ovn-traffic-review-e2e-build.log`.
- E2E discovery dry-run: blocked during unrelated suite registration by the
  existing cluster API returning EOF at `127.0.0.1:40245`. No test body ran.
  Log: `/tmp/ovn-traffic-review-e2e-dry-run.log`.
- Native macOS focused unit execution: unavailable because the affected
  packages require Linux netlink/metrics symbols. Podman SSH/API access also
  failed or stalled. No fresh Linux unit/race pass is claimed.
- Repository-pinned `golangci-lint` v2.12.2: passed, **0 issues**, for
  `pkg/clustermanager`, `pkg/ovn`, and `pkg/node/controllers/egressip`, including
  tests, with `GOOS=linux CGO_ENABLED=0`. The installed older linter was
  incompatible; v2.12.2 is under `/tmp/ovn-traffic-review-bin`. An initial run
  exhausted temporary disk space; after clearing this review's disposable
  cache, the final run and the check after the last edits both passed.
  Log: `/tmp/ovn-traffic-review-lint.log`.

No changes were made to Helm values, gateway annotations, Kind nodes, the
shared Podman VM, or API resources. The disposable validation container was
started during the preceding review; its stopped state could not be verified
when Podman became unreachable. Do not interpret the historical cleanup record
in section 10 as a fresh health check.

### Follow-up from the first review (see current handoff for status)

1. On a working Linux validation host, execute the focused regressions and the
   affected OVN Egress suite, then the remaining repository build gates. Include
   race execution of the concurrent regression. Focused lint already passed;
   compilation alone is insufficient runtime evidence. From `go-controller`:

   ```sh
   go test -mod=vendor -race ./pkg/clustermanager ./pkg/ovn \
     ./pkg/node/controllers/egressip ./pkg/node/iprulemanager \
     -run 'TestTrafficCondition|TestSelective|TestDestinationRoutes|TestTrafficRuleRoundTrip' \
     -count=1 -timeout=10m
   go test -mod=vendor ./pkg/ovn -ginkgo.focus=Egress -ginkgo.no-color
   ```
2. Review the resulting diff and CI results before describing the implementation
   as validated. Author review, commit organization, and PR preparation remain
   separate work; none was performed here.
3. Keep further E2E execution deferred as requested. When later authorized,
   execute the stronger assertions on CI/Kind and preserve the section-10
   gateway pin if using the existing local cluster.

## 12. Second review fixes — 2026-10-21

Both findings from the second review are fixed in the working tree, on top of
the five fixes in section 11. Changes remain uncommitted; unrelated local files
are preserved. E2Es remain deferred by request.

### Findings and fixes

1. **Malformed traffic selectors could block unrelated EgressIPs.**
   The CRD previously accepted unknown expression operators and invalid value
   counts. A stored malformed selector could then abort global OVN startup or
   traffic-condition reconciliation. Admission now enforces the Kubernetes
   expression operators and their value-count rules. A shared parser also
   rejects empty selectors and uses Kubernetes validation for label syntax;
   invalid input returns a selector matching nothing, never catch-all.
   OVN and node reconciliation clear that owner's old traffic matches while
   continuing healthy owners. Class events skip invalid owners, and startup
   repair tolerates them. Condition reconciliation records
   `TrafficSelectorResolved=False` and `TrafficConflict=Unknown`, both with
   reason `InvalidTrafficSelection`, instead of aborting the global pass or
   emitting a misleading `NoTrafficClass` warning. Backend errors still fail
   reconciliation and retain normal retries.

   Admission validates expression operators/arity and the existing nonempty
   selector requirement. Label key/value syntax is validated at runtime;
   the CRD does not newly reject every malformed label string. Using CEL format
   validation over the existing unbounded selector maps/lists exceeded the
   API server's static cost budget, so no arbitrary selector-size limits or
   replacement selector type were introduced. Generated EgressIP CRD, clients,
   OpenAPI descriptions, API reference, and feature documentation were updated.

   Regressions cover invalid selector parsing, admission rejection and valid
   expressions, isolation of healthy owners' conditions, and transitions from
   valid to malformed/empty selectors and back. The OVN topology/family matrix
   includes full startup and class events with an invalid stored owner. The
   node lifecycle checks removal of invalid-owner rules/SNAT, peer survival,
   and startup repair. Runtime results are distinguished below from added tests.

2. **Deleting or changing one secondary-host owner could remove a peer's rule.**
   Identical pod/destination/protocol/table selections produce one shared Linux
   policy rule even when the owners have different SNAT addresses. Cleanup
   previously deleted that rule unconditionally; the surviving owner's applied
   cache could then prevent its reinstallation. Cleanup now collects other
   owners' applied rule references once per owner update, under the existing
   configuration mutex, and deletes a rule only when no peer references it.
   Failed applied entries remain references until their normal retry/cleanup.
   Selective SNAT continues using the existing union reconciliation.

   The node lifecycle regression checks exactly one kernel rule for identical
   owners, preservation when one owner changes its match or is deleted, and
   removal when the final owner stops using the rule. This covers both peer
   survival and stale-rule cleanup without changing overlap policy.

### Validation and remaining work

- Real API-server admission regression reproduced the first defect before the
  fix: an unknown expression operator was accepted. The full focused
  EgressTrafficClass admission suite passes after regeneration, including the
  new selector cases. Logs:
  `/tmp/ovn-traffic-review2-admission-before.log` and
  `/tmp/ovn-traffic-review2-admission.log`.
- Focused Linux execution passed `pkg/util/egressip` and
  `pkg/clustermanager`, including the parser and condition regressions.
  Log: `/tmp/ovn-traffic-review2-linux-tests.log`.
- Generated artifacts and API-reference generation completed successfully.
  Logs: `/tmp/ovn-traffic-review2-codegen.log` and
  `/tmp/ovn-traffic-review2-api-docs.log`.
- Repository-pinned `golangci-lint` v2.12.2 passed with **0 issues** for
  `pkg/util/egressip`, `pkg/clustermanager`, `pkg/ovn`, and
  `pkg/node/controllers/egressip`, including tests, using the Linux target on
  macOS. Log: `/tmp/ovn-traffic-review2-lint.log`.
- Linux cross-compilation passed for the five affected test packages:
  `pkg/util/egressip`, `pkg/clustermanager`, `pkg/ovn`,
  `pkg/node/controllers/egressip`, and `pkg/node/iprulemanager`, using
  `GOOS=linux CGO_ENABLED=0 go test -mod=vendor -p 1 -exec=true ... -run '^$'`.
  **This compiles test binaries; it does not execute them.**
  Log: `/tmp/ovn-traffic-review2-linux-build.log`.
- `gofmt` and `git diff --check`: passed.
- The Linux test run stopped yielding results after the two package passes
  above. Independent Podman connections stalled or failed with
  `ssh: handshake failed: EOF`. No fresh OVN/node/iprulemanager runtime pass,
  broader OVN Egress pass, or race pass is claimed. A bounded attempt to stop
  only `ovn-traffic-validation` also timed out; its stopped state and the remote
  test process state could not be verified. The stalled local test client was
  terminated. No Kind container or shared VM was restarted or stopped.

No E2Es, cluster changes, shared VM restarts, commits, or PR operations were
performed for this follow-up. The implementation fixes are complete; this is
not a claim that the full validation matrix has passed. Keep E2Es deferred and
use GitHub CI for the remaining runtime/build gates. In particular, retain the
focused regressions, concurrent regression under the race detector, and the
broader OVN Egress suite in the remaining validation work.
