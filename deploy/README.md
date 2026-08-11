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

1. **Create the registry Secret.** `deployment.yaml` reads
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

2. **The image.** You do not build it by hand. `deployment.yaml` carries a real
   `ghcr.io/toyinogun/deployer@sha256:...` digest, and the `publish` job in
   [ci.yml](../.github/workflows/ci.yml) rewrites that line on every push to
   `main`, then commits it. ArgoCD applies the file as written, so it must always
   hold a digest a kubelet can pull: never a `ko://` reference, which nothing in
   the ArgoCD path resolves, and never a mutable tag.

   Once per repository, after the first publish, set the `deployer` package on
   GitHub to **public**. A ghcr.io package starts private, and a private one needs
   an imagePullSecret in `deployer-system` that nothing here creates.

   To publish from your laptop instead, for a change you do not want to push yet:

   ```bash
   KO_DOCKER_REPO=ghcr.io/toyinogun/deployer ko build --bare --platform=linux/amd64 ./cmd/deployer
   # paste the printed digest into deploy/deployment.yaml
   ```

## The four things no file here can tell you

These are done once, by hand, outside this repository, and the build does not
work without them. They are step by step in
[verify.md](../docs/specs/0003-cluster-foundation/verify.md).

1. Advertise `172.16.70.40/32` from the pfSense subnet router, approve the route
   in the Tailscale admin console, grant `172.16.70.40/32:443` in the tailnet
   policy file, and add the matching pfSense firewall pass rule on `tailscale0`.
   All four, the ACL and the firewall are separate gates. Do **not** expose the
   controller Service through the Tailscale operator: it was tried and does not
   work on this cluster, see the spec's rationale.
2. Point a wildcard DNS record `*.<domain>`, and the apex, at `172.16.70.40`, as
   DNS only records rather than proxied ones.
3. Seal a Cloudflare API token scoped to `Zone.DNS: Edit` on the one zone into the
   `cert-manager` namespace, which is this cluster's cert-manager cluster resource
   namespace.
4. Point the ingress-nginx `--default-ssl-certificate` flag at
   `ingress-nginx/wildcard-apps-tls`. This restarts the shared controller and
   briefly interrupts TLS for the apps already behind it, so do it deliberately.
