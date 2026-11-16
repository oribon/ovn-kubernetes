# EgressIP traffic-selection demo

A short, interactive demo on an existing `contrib/kind.sh` cluster. Use an
IPv4-capable cluster running the feature image with EgressIP and TrafficClass
enabled (`--enable-traffic-class`). This demo uses the cluster default network.

Requires Bash, `kubectl`, `jq`, Python 3, and access to the container runtime that
hosts the KinD nodes. All scripts use the environment's `KUBECONFIG` directly.

```bash
export KUBECONFIG=/path/to/kubeconfig
./egress-ip-demo/setup.sh
./egress-ip-demo/demo.sh
```

For Podman, also set `OCI_BIN=podman` (or `KIND_EXPERIMENTAL_PROVIDER=podman`).
If the cluster uses a different container network, set `KIND_NETWORK` for setup
and the demo. `AGNHOST_IMAGE` can override the setup image; the default matches the
repository's E2E agnhost version, `registry.k8s.io/e2e-test-images/agnhost:2.63.0`.

## Setup

`setup.sh` creates or reuses `egress-ip-demo-backend-a` and
`egress-ip-demo-backend-b`, attaches them to the KinD network, and starts them if
stopped. Each runs agnhost netexec on HTTP/TCP 8080 and UDP 9090. It applies the
`egress-ip-demo` namespace and `client` pod, waits for readiness, and checks that
the pod can reach both backends. Running setup again preserves those resources.

## Presentation

Open [`slides.html`](slides.html) directly in a browser for a title slide followed
by the four-slide introduction. Use the arrow keys or Space to navigate, and F for fullscreen.
The last slide hands off to the terminal demo. No server, build step, or internet
connection is needed; browser printing also includes all five slides.

`demo.sh` prints each manifest before applying it and pauses for Enter between
configuration changes and traffic probes. The opening diagram shows the current
context, the client's actual IPv4 pod address, and both external backends, with a
color-coded legend for the source IPs introduced during the demo:

1. A catch-all EgressIP supplies one source address for both backends.
2. Two TrafficClasses select the backend destination addresses; two selective
   EgressIPs give the same pod a different source address for each backend.
3. Narrow A's class to TCP destination port 8080. HTTP keeps A's source address;
   UDP to the same backend uses the catch-all address. B remains unchanged.

Each probe shows its actual `kubectl exec` command and raw reply: `curl` for
HTTP/TCP and `nc` for UDP. Retries and unexpected replies remain visible while
forwarding converges. A summary table follows all three successful requests,
showing expected and observed source IPs with source ports omitted.
Default, A, and B use consistent yellow, cyan, and magenta highlighting in the
manifests and results. Request destinations use the backend's color; replies use
the source EgressIP's color, so A's UDP fallback has a cyan destination and a
yellow reply. Resource creation/update messages remain visible; successful
readiness checks and node-label updates are quiet. Set `NO_COLOR=1` to disable
formatting; redirected output is also plain text.

Probes retry while forwarding converges and fail if the expected
source is not observed; UDP uses a fresh source port for each attempt.

The script discovers the backend addresses, chooses a Ready KinD node, and picks
three unused addresses from its primary IPv4 subnet, excluding container and
other EgressIP allocations. Existing demo addresses are reused when possible.
It labels that node as egress-assignable; the controller chooses the assignments
from eligible nodes without an explicit node selector.
Only the demo's selective EgressIPs are removed at the start of each replay.

Ctrl-C stops the presentation; resources stay available for inspection. To remove
the demo resources afterward:

```bash
./egress-ip-demo/cleanup.sh
```

Cleanup can be rerun and honors the same container-runtime settings. The node
remains egress-assignable.
