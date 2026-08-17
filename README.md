# nubuluscloud-operator

Expose a service running in your Kubernetes cluster on a public hostname,
without opening a port, without a public IP, and without a load balancer.

You declare two objects. The operator creates the tunnel on Nubulus Cloud, runs
the tunnel agent beside your workloads, and points the routes at your Services.

```yaml
apiVersion: tunnel.nubulusnetwork.es/v1alpha1
kind: Tunnel
metadata:
  name: production
spec:
  credentials:
    name: nubulus-api
---
apiVersion: tunnel.nubulusnetwork.es/v1alpha1
kind: TunnelRoute
metadata:
  name: web
spec:
  tunnelRef: production
  hostname: app.example.com
  service:
    name: web
    port: 8080
```

Traffic arrives at Nubulus over HTTPS, travels an outbound WireGuard tunnel your
cluster opened, and reaches your Service. Nothing listens on the public internet
from your side, and the certificate is not yours to manage.

## Install

```sh
helm install nubulus oci://ghcr.io/nubulus-network/charts/nubuluscloud-operator \
  --namespace nubulus-system --create-namespace \
  --set agent.image=<the tunnel agent image>
```

See [`charts/nubuluscloud-operator/README.md`](charts/nubuluscloud-operator/README.md)
for the values that matter, how to upgrade the custom resource definitions,
and the order to uninstall in.

Then create a credential in the namespace where the Tunnel will live. It comes
from an application token created in the Nubulus panel:

```sh
kubectl -n apps create secret generic nubulus-api \
  --from-literal=client_id=<client id> \
  --from-literal=client_secret=<client secret>
```

## Things worth knowing before you rely on it

**A route needs DNS, and the operator cannot do it.** After creating a Tunnel,
read `status.cnameTarget` and publish it as a CNAME for your hostname, wherever
that zone lives. Until then the route exists and carries nothing.

**A credential belongs to a namespace.** A Tunnel reads its Secret from its own
namespace, a TunnelRoute may only reference a Tunnel and a Service in its own
namespace. That is what stops anyone who can create objects in one namespace
from publishing another team's workload, or from routing traffic through
somebody else's account.

**One agent per tunnel, and only one.** A tunnel is a single WireGuard peer
holding one address, so a second replica would not share the load. Run two
tunnels instead. The chart does not offer a replica count for this.

**The agent needs no privileges.** WireGuard here is a userspace
implementation, so the pod runs with no `NET_ADMIN`, no `privileged`, no kernel
module, a read-only root filesystem and every capability dropped.

**Set `clusterDomain` if your cluster is not `cluster.local`.** It is how a
Service becomes the name the agent resolves. Getting it wrong publishes routes
pointing at names that do not exist, and the symptom is a 502 on your public
hostname rather than anything visible in Kubernetes.

## Reference

### Tunnel

| Field | | |
|---|---|---|
| `spec.credentials.name` | required | Secret in this namespace with `client_id` and `client_secret` |
| `spec.displayName` | optional | A label for the tunnel in the panel. Cosmetic |
| `spec.agent` | optional | `image`, `logLevel`, `resources`, `nodeSelector`, `tolerations`, `imagePullSecrets` |
| `status.cnameTarget` | | What to point your hostname at |
| `status.onlineStatus` | | `online`, `degraded` or `offline`, as the platform sees it |

### TunnelRoute

| Field | | |
|---|---|---|
| `spec.tunnelRef` | required | A Tunnel in this namespace |
| `spec.hostname` | required | The public name. Unique across the whole platform |
| `spec.type` | `host` | `host` takes everything for the hostname; `path` takes one prefix |
| `spec.pathPrefix` | | Required for `type: path`, and it cannot be `/` |
| `spec.service` | | `name` and `port`, a number or a declared port name |
| `spec.upstream` | | `host` and `port`, for a target outside the cluster. Mutually exclusive with `service` |
| `spec.scheme` | `http` | How the agent reaches the upstream. The public side is always HTTPS |
| `spec.stripPrefix` | `false` | Remove `pathPrefix` before the request reaches the upstream |
| `spec.priority` | `100` | Lower wins when more than one route could match |
| `spec.enabled` | `true` | A disabled route keeps its hostname claimed |

Both carry `Ready` conditions. `kubectl describe` explains a refusal in words,
including the ones that look like a permissions problem and are not.

## Development

```sh
make test          # unit tests plus an envtest suite against a real API server
make lint
make docker-build IMG=nubuluscloud-operator:dev
```

The tests need no container runtime: `envtest` runs an API server and etcd as
plain binaries, downloaded by `make setup-envtest`.

To run against a cluster from your machine:

```sh
kubectl apply -f config/crd/bases/
go run ./cmd --agent-image=<the tunnel agent image>
```

`--agent-image` has no default and the operator refuses to start without one. A
wrong default would not be an error anybody sees, it would be an unexpected
image pulled into a cluster and handed a credential.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
