# deploy/

The Kubernetes manifests for the Deployer control plane. Governing spec:
[docs/specs/0003-cluster-foundation/index.md](../docs/specs/0003-cluster-foundation/index.md).
Verify steps, including everything a human does by hand:
[verify.md](../docs/specs/0003-cluster-foundation/verify.md).

ArgoCD syncs this directory into `deployer-system`, and only into
`deployer-system`. It never sees the `app-<slug>` namespaces the control plane
creates at runtime.

## What is here

| File | What it is |
|---|---|
| `namespace.yaml` | `deployer-system`, with restricted pod security |
| `rbac.yaml` | The ServiceAccount, and the one ClusterRole listing every right the control plane holds |
| `pvc.yaml` | The 10Gi Longhorn volume the SQLite file lives on |
| `configmap.yaml` | Every `DEPLOYER_*` setting that is not a credential |
| `deployment.yaml` | One replica, `Recreate`, non root, read only root filesystem |
| `service.yaml` | The in cluster address of the control plane |
| `ingress.yaml` | The control plane's own tailnet name, on the `tailscale` class |
| `hello-world.yaml` | A hand applied app in the exact shape the control plane will generate |
| `gitops/` | The pieces that live in `k3sprox-gitops`, not here. Copy them across |

## Before the first apply

1. **Set the domain.** `DEPLOYER_APP_DOMAIN` in `configmap.yaml` is
   `apps.example.com` and must be changed. The same value appears in the Ingress
   host in `hello-world.yaml` and in `gitops/wildcard-certificate.yaml`.

2. **Create the registry Secret.** `deployment.yaml` reads
   `DEPLOYER_REGISTRY_HOST`, `_USER`, and `_PASSWORD` from a Secret named
   `deployer-registry`, and `internal/config` refuses to boot without all three.
   Slice 1 owns the real registry. Until then, a placeholder is enough to get the
   control plane up:

   ```bash
   kubectl -n deployer-system create secret generic deployer-registry \
     --from-literal=DEPLOYER_REGISTRY_HOST=registry.deployer-system.svc:5000 \
     --from-literal=DEPLOYER_REGISTRY_USER=placeholder \
     --from-literal=DEPLOYER_REGISTRY_PASSWORD=placeholder
   ```

   Replace it with a SealedSecret when slice 1 mints the real credentials.

3. **Build the image.** `deployment.yaml` carries a `ko://` reference, so it is
   resolved by `ko` rather than applied raw:

   ```bash
   KO_DOCKER_REPO=<your registry> ko resolve -f deploy/ > /tmp/deployer.yaml
   kubectl apply -f /tmp/deployer.yaml
   ```

## The four things no file here can tell you

These are done once, by hand, outside this repository, and the build does not
work without them. They are step by step in
[verify.md](../docs/specs/0003-cluster-foundation/verify.md).

1. Expose the existing `ingress-nginx-controller` Service through the Tailscale
   operator. A **Service** level exposure, never an Ingress on the `tailscale`
   class: an Ingress there would terminate TLS at Tailscale with a `ts.net`
   certificate and break the whole wildcard design.
2. Point a wildcard DNS record `*.<domain>` at the tailnet address that device is
   given.
3. Seal a Cloudflare API token scoped to `Zone.DNS: Edit` on the one zone into the
   `cert-manager` namespace, which is this cluster's cert-manager cluster resource
   namespace.
4. Point the ingress-nginx `--default-ssl-certificate` flag at
   `ingress-nginx/wildcard-apps-tls`. This restarts the shared controller and
   briefly interrupts TLS for the apps already behind it, so do it deliberately.
