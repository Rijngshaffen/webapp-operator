# webapp-operator

A Kubernetes operator, written in Go with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime),
that runs stateless web applications from a single custom resource.

You write a `WebApp`:

```yaml
apiVersion: apps.rijngshaffen.github.io/v1alpha1
kind: WebApp
metadata:
  name: shop
spec:
  image: nginxinc/nginx-unprivileged:1.29
  replicas: 3
  port: 8080
  ingress:
    host: shop.example.com
    tlsSecretName: shop-tls
```

The operator creates and keeps in sync a **Deployment**, a **Service** and
(optionally) an **Ingress**, and reports back on the resource:

```
$ kubectl get webapps -o wide
NAME   IMAGE                              DESIRED   READY   AVAILABLE   URL                         AGE
shop   nginxinc/nginx-unprivileged:1.29   3         3       True        https://shop.example.com/   2m
```

## Install

Each [release](https://github.com/Rijngshaffen/webapp-operator/releases) has an
`install.yaml` containing the CRD, the RBAC rules and the operator Deployment,
pinned to that release's image
(`ghcr.io/rijngshaffen/webapp-operator:<version>`, for amd64 and arm64):

```bash
kubectl apply -f https://github.com/Rijngshaffen/webapp-operator/releases/download/v0.1.0/install.yaml
kubectl -n webapp-operator-system rollout status deployment/webapp-operator-controller-manager
kubectl apply -f https://raw.githubusercontent.com/Rijngshaffen/webapp-operator/master/config/samples/webapp_minimal.yaml
kubectl get webapps
```

**Upgrade** by applying the `install.yaml` of the newer release.

**Uninstall** in this order. WebApps carry a finalizer that only the running
operator can release, so removing the operator first leaves them, and the CRD
deletion, stuck in `Terminating`:

```bash
kubectl delete webapps --all --all-namespaces
kubectl delete -f https://github.com/Rijngshaffen/webapp-operator/releases/download/v0.1.0/install.yaml
```

If the operator is already gone, release a stuck WebApp by hand with
`kubectl patch webapp <name> --type=merge -p '{"metadata":{"finalizers":[]}}'`.

## The WebApp resource

| Field | Default | Description |
|---|---|---|
| `spec.image` | required | Container image to run. |
| `spec.replicas` | `1` | Number of pods. Also settable with `kubectl scale` or a HorizontalPodAutoscaler. |
| `spec.port` | `8080` | Port the container listens on. |
| `spec.servicePort` | `80` | Port the Service exposes. |
| `spec.serviceType` | `ClusterIP` | `ClusterIP`, `NodePort` or `LoadBalancer`. |
| `spec.healthPath` | – | HTTP path for readiness and liveness probes. Without it, readiness is a TCP check and there is no liveness probe. |
| `spec.env` | – | Environment variables (standard `EnvVar` list). |
| `spec.resources` | – | CPU/memory requests and limits. |
| `spec.ingress.host` | – | Setting `spec.ingress` creates an Ingress for this host; removing it deletes the Ingress. |
| `spec.ingress.path` | `/` | Path prefix routed to the app. |
| `spec.ingress.className` | cluster default | IngressClass to use. |
| `spec.ingress.tlsSecretName` | – | TLS Secret; enables HTTPS for the host. |

Status:

| Field | Description |
|---|---|
| `status.conditions[Available]` | `True` when the Deployment has its minimum number of ready replicas. |
| `status.conditions[Progressing]` | `True` while a rollout or scale operation is in flight. |
| `status.conditions[Degraded]` | `True` when the operator could not reconcile a child resource; the message says why. |
| `status.readyReplicas` | Number of ready pods. |
| `status.url` | The Ingress URL if there is one, otherwise the in-cluster Service URL. |
| `status.observedGeneration` | The `metadata.generation` the status was computed for. |

Defaults and validation live in the CRD schema (generated from the markers in
[api/v1alpha1/webapp_types.go](api/v1alpha1/webapp_types.go)), so invalid
resources are rejected by the API server before the operator ever sees them.

## How it works

```
            watch                     create / update / delete
 WebApp ───────────▶  Reconcile  ───────────────────────────────▶  Deployment
   ▲                     ▲   │                                      Service
   │  status             │   └──────────────────────────────────▶  Ingress
   └─────────────────────┘        watch (owner reference)              │
                         └─────────────────────────────────────────────┘
```

- **Level-triggered.** [`Reconcile`](internal/controller/webapp_controller.go)
  is handed only a name. It reads the WebApp, computes the full desired state
  and converges the cluster to it. It never depends on *what* changed, so it is
  safe to call any number of times.
- **Owned objects.** Every child has the WebApp as its controller owner
  reference. That gives garbage collection on delete, and it is how the
  operator is notified when a child changes: delete the Service or edit the
  Deployment by hand and it is put back.
- **Field-level ownership.** The `mutate*` functions in
  [resources.go](internal/controller/resources.go) set only the fields the
  operator manages and leave the rest of the live object alone, so API server
  defaults and other controllers' fields are not fought over.
- **No adoption.** An existing object with the same name that is not
  controlled by the WebApp is never modified or deleted; the WebApp goes
  `Degraded`.
- **Bounded memory.** Children carry the label
  `app.kubernetes.io/managed-by=webapp-operator` and the operator caches only
  objects with that label, not every Deployment, Service and Ingress in the
  cluster. If the label is removed from a child, the operator puts it back.
- **Finalizer.** On delete the operator removes the Ingress and Service first,
  taking the app out of rotation, and only then lets the garbage collector
  remove the Deployment and its pods.
- **Status from observation.** Conditions are derived from the Deployment's
  status, and the status is only written when it actually changed.

## Development

Requirements: Go 1.27+, `make`, and for the cluster targets `kubectl` and a
cluster (kind, minikube, …). `controller-gen`, `kustomize`, `golangci-lint`,
`govulncheck` and the envtest control plane are downloaded into `./bin` at
pinned versions on first use; nothing is installed globally.

```bash
make test          # integration tests against a local kube-apiserver/etcd
make test-smoke    # run the built binary with the rendered install manifest
make lint          # golangci-lint
make vulncheck     # known vulnerabilities in the code and its dependencies
make install       # install the CRD into the cluster in your kubeconfig
make run           # run the operator on your machine against that cluster
```

To build and deploy your own image:

```bash
make docker-build docker-push IMG=registry.example.com/webapp-operator:dev
make deploy IMG=registry.example.com/webapp-operator:dev
```

Run `make help` for all targets. After changing the API types or the
`+kubebuilder:rbac` markers, run `make manifests generate` to regenerate the
CRD, the ClusterRole and the DeepCopy code; CI fails if they are stale.

### Releasing

Push a tag:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

The [release workflow](.github/workflows/release.yml) runs the tests, pushes a
multi-arch image to `ghcr.io/rijngshaffen/webapp-operator:v0.1.0` and creates a
GitHub release with the matching `install.yaml`.

## Project layout

```
api/v1alpha1/          WebApp types; kubebuilder markers drive the CRD schema
cmd/main.go            Manager setup: flags, metrics, health probes, leader election
internal/controller/   The reconciler, the desired-state builders, and their tests
config/crd/            Generated CRD                       (make manifests)
config/rbac/           Generated operator ClusterRole + hand-written bindings
config/manager/        The operator's own Deployment
config/default/        Kustomize entry point tying the above together
config/samples/        Example WebApps
test/smoke/            Runs the real binary with dist/install.yaml
.github/               CI, release workflow, Dependabot
```

## Testing

`make test` starts a real kube-apiserver and etcd (envtest) and runs the
controller through a manager, authenticated as a user that holds **only** the
generated ClusterRole — a permission missing from the RBAC markers fails the
tests. There is no kubelet and no garbage collector in envtest, so tests write
the Deployment status themselves and check owner references rather than
cascading deletion.

`make test-smoke` builds the manager binary, applies `dist/install.yaml` to
envtest, and runs the binary as the operator's ServiceAccount: it checks leader
election, a reconcile, that `/metrics` rejects unauthenticated and unauthorised
callers, and a clean exit on SIGTERM.

## Operations

| Flag | Default | Description |
|---|---|---|
| `--leader-elect` | `false` (on in `install.yaml`) | Only one replica reconciles at a time; the lease is released on shutdown. |
| `--leader-election-namespace` | the operator's namespace | Namespace of the Lease. Needed only when running outside a cluster. |
| `--max-concurrent-reconciles` | `1` | Number of WebApps reconciled in parallel. |
| `--metrics-bind-address` | `:8443` | Metrics endpoint; `0` disables it. |
| `--metrics-secure` | `true` | Serve metrics over HTTPS with authentication and authorization. |
| `--health-probe-bind-address` | `:8081` | `/healthz` and `/readyz`. |
| `--zap-log-level`, `--zap-encoder`, … | `info`, `json` | Logging. |
| `--version` | | Print the version and exit. |

- **Metrics** require a bearer token whose subject may `GET /metrics`: bind
  the `webapp-operator-metrics-reader` ClusterRole to your Prometheus
  ServiceAccount. The serving certificate is self-signed, so scrape with
  `insecureSkipVerify` or mount your own certificate.
- **High availability**: leader election is on in the shipped Deployment, so
  running more than one replica is safe.
- **Operator permissions** are the generated
  [ClusterRole](config/rbac/role.yaml): WebApps, plus Deployments, Services and
  Ingresses in every namespace. It cannot read Secrets or delete Deployments.
- **User permissions**: the `webapp-operator-webapp-editor-role` and
  `webapp-operator-webapp-viewer-role` ClusterRoles are aggregated into the
  built-in `admin`/`edit` and `view` roles.
- The operator's own pod satisfies the `restricted` Pod Security Standard.
- **Troubleshooting**: `kubectl describe webapp <name>` shows the conditions
  and the events the operator recorded for it.

## Limitations

- The API is `v1alpha1` and may change incompatibly.
- The operator watches all namespaces; it cannot be restricted to some.
- Pods get no `securityContext` from the operator; apply your Pod Security
  settings at the namespace level.
- There is no admission webhook; validation is limited to what the CRD schema
  can express.

## License

Apache License 2.0, see [LICENSE](LICENSE).
