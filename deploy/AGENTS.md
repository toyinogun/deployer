# deploy

The Kubernetes manifests for the control plane, applied by ArgoCD into `deployer-system`. Governing spec: [docs/specs/0003-cluster-foundation/index.md](../docs/specs/0003-cluster-foundation/index.md). File by file detail, the first apply steps, and the hand run checks live in [README.md](README.md).

## Layout

- [kustomization.yaml](kustomization.yaml): the apply order. A new manifest is invisible to ArgoCD until it is listed here.
- [namespace.yaml](namespace.yaml), [rbac.yaml](rbac.yaml), [admission-policy.yaml](admission-policy.yaml): the namespace, the service account rights, and the fence around them.
- [pvc.yaml](pvc.yaml), [configmap.yaml](configmap.yaml), [deployment.yaml](deployment.yaml), [service.yaml](service.yaml), [ingress.yaml](ingress.yaml): the pod and everything it needs.
- [hello-world.yaml](hello-world.yaml): a hand applied app in the exact shape the control plane will generate. Not part of the kustomization.
- [gitops/](gitops/): reference copies for the separate `k3sprox-gitops` repository. Nothing here applies them.

## Conventions

- `deployment.yaml` holds an image digest, and the `publish` job in [.github/workflows/ci.yml](../.github/workflows/ci.yml) rewrites that line on every push to `main`. Do not hand edit the digest, and never put a `ko://` reference or a mutable tag there: ArgoCD applies this file verbatim, so it must always hold something a kubelet can pull.
- The two cluster wide rights in `rbac.yaml` (namespaces, rolebindings) are fenced by `admission-policy.yaml`, because RBAC has no way to say "only namespaces named `app-*`". Widening one without checking the other reopens the escalation the pair exists to close. Everything the control plane does inside an app namespace belongs in `ClusterRole/deployer-app`, which is never bound cluster wide.
- The `AppProject` in `gitops/argocd-application.yaml` is scoped to `deployer-system` alone. That scope is what stops a sync with `prune: true` from reaching an `app-<slug>` namespace created at runtime, so it is load bearing, not boilerplate.
- Apps are served on the shared wildcard certificate through the `nginx` ingress class, so an app Ingress carries no `tls` block and no app namespace ever holds a private key. The `tailscale` class is for the control plane's own Ingress only.
- Every `DEPLOYER_*` value here has a matching validation in [internal/config](../internal/config/config.go). Adding a key to `configmap.yaml` without adding it there means the pod reads nothing; adding it there without adding it here fails the boot.

_Drafted by /sync from the introducing change, worth a quick human pass._
