# nubuluscloud-operator

Declare Nubulus Cloud tunnels and routes as Kubernetes objects. The operator
creates them on the platform and runs the tunnel agent beside them, so a service
in this cluster becomes reachable on a public hostname without opening a port.

## Install

```sh
helm install nubulus oci://ghcr.io/nubulus-network/charts/nubuluscloud-operator \
  --namespace nubulus-system --create-namespace \
  --set agent.image=<the tunnel agent image>
```

`agent.image` has no default and the install fails without it. That is
deliberate: a wrong value would pull an unexpected image into your cluster and
hand it a credential, and that failure is much quieter than this one.

If your cluster does not use `cluster.local`, set `clusterDomain` too. It is
used to turn a Service into the name the agent resolves, so getting it wrong
publishes routes pointing at names that do not exist. The failure shows up as a
502 on a public hostname rather than as anything visible in Kubernetes.

## Using it

Two things have to exist that this chart does not create.

**A credential.** Create an application token in the panel and store it in the
namespace where the Tunnel will live:

```sh
kubectl -n apps create secret generic nubulus-api \
  --from-literal=client_id=<client id> \
  --from-literal=client_secret=<client secret>
```

It is read from the Tunnel's own namespace and from nowhere else. That is the
tenancy model: a namespace brings its own credential, and therefore its own
account, so one team cannot route traffic through another team's account.

**DNS.** A route carries traffic only once its hostname points at the tunnel:

```sh
kubectl -n apps get tunnel production -o jsonpath='{.status.cnameTarget}'
```

Publish that as a CNAME wherever the hostname's zone lives. The operator cannot
do it, because that zone is usually somewhere else entirely.

Then:

```yaml
apiVersion: tunnel.nubulusnetwork.es/v1alpha1
kind: Tunnel
metadata:
  name: production
  namespace: apps
spec:
  credentials:
    name: nubulus-api
---
apiVersion: tunnel.nubulusnetwork.es/v1alpha1
kind: TunnelRoute
metadata:
  name: web
  namespace: apps
spec:
  tunnelRef: production
  hostname: app.example.com
  service:
    name: web
    port: 8080          # a number, or a port name the Service declares
```

A `TunnelRoute` may only name a Service in its own namespace, and a Tunnel in
its own namespace. Anything else would let whoever can create one object publish
another team's workload on the internet.

## Upgrading

**Helm installs the custom resource definitions once and never touches them
again.** That is Helm's behaviour for the `crds/` directory, not a choice this
chart makes, and it means a `helm upgrade` that should have added a field to a
schema silently does not. Apply them by hand as part of any upgrade:

```sh
helm show crds oci://ghcr.io/nubulus-network/charts/nubuluscloud-operator \
  --version <new version> | kubectl apply -f -
```

The chart version and the operator version move together, because the schemas
shipped here are generated from the operator's own types.

## Uninstalling

**Delete every Tunnel and TunnelRoute first, and wait for them to go.**

```sh
kubectl delete tunnelroutes --all --all-namespaces
kubectl delete tunnels --all --all-namespaces
helm uninstall nubulus -n nubulus-system
```

Those objects carry finalizers, and the operator is what runs them. Remove the
operator first and you get objects that cannot be deleted, plus tunnels still
alive on the platform with nothing in this cluster pointing at them. Helm does
not delete the definitions on uninstall, which is what stops that from becoming
unrecoverable, but the order above avoids the mess entirely.

## What the operator is allowed to do

The ClusterRole includes `get`, `list` and `watch` on Secrets **cluster-wide**.
That is not convenience: a Tunnel names its credential Secret in its own
namespace, so which namespaces hold one cannot be known in advance.

If that is too broad, set `rbac.create=false` and bind the operator yourself
with Roles in the namespaces that actually hold Tunnels. Everything else it
needs is narrow: Services read-only, Deployments and Secrets it owns, and the
custom resources.

## Values

| Key | Default | |
|---|---|---|
| `agent.image` | *(required)* | The tunnel agent image deployed for every Tunnel |
| `clusterDomain` | `cluster.local` | DNS suffix used to address Services |
| `image.repository` | `ghcr.io/nubulus-network/nubuluscloud-operator` | |
| `image.tag` | chart `appVersion` | |
| `replicaCount` | `1` | Extras idle unless leader election is on |
| `leaderElection.enabled` | `true` | |
| `logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `rbac.create` | `true` | See above before turning it off |
| `metrics.enabled` | `false` | |
| `resources` | 10m / 64Mi, limit 256Mi | |
