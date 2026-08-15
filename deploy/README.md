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
| `builds-namespace.yaml` | `deployer-builds`, where build Jobs run, plus the control plane's RoleBinding into it |
| `registry.yaml` | The in cluster `distribution` registry, its Longhorn volume, and its ClusterIP Service. No Ingress, on purpose |
| `rbac.yaml` | The ServiceAccount, and the one ClusterRole listing every right the control plane holds |
| `pvc.yaml` | The 10Gi Longhorn volume the SQLite file lives on |
| `configmap.yaml` | Every `DEPLOYER_*` setting that is not a credential |
| `deployment.yaml` | One replica, `Recreate`, non root, read only root filesystem |
| `service.yaml` | The in cluster address of the control plane |
| `ingress.yaml` | The control plane's own tailnet name, on the `tailscale` class |
| `hello-world.yaml` | A hand applied app in the exact shape the control plane will generate |
| `gitops/` | The pieces that live in `k3sprox-gitops`, not here. Copy them across |

## Before the first apply

1. **Mint the registry credential, twice sealed.** One password, in two shapes:
   an htpasswd file the registry checks logins against, and a plain user and
   password the control plane presents. Neither may be committed in the clear, so
   both go through `kubeseal`. Do this before the first sync, because
   `registry.yaml` will not start without the htpasswd Secret and
   `internal/config` refuses to boot without the credential Secret.

   ```bash
   PASSWORD="$(openssl rand -base64 30)"

   # 1. The htpasswd file the registry reads. bcrypt, which is what
   #    distribution v3 accepts.
   htpasswd -nbB deployer "$PASSWORD" > /tmp/htpasswd
   kubectl -n deployer-system create secret generic deployer-registry-htpasswd \
     --from-file=htpasswd=/tmp/htpasswd --dry-run=client -o yaml \
     | kubeseal --format yaml > deploy/registry-htpasswd-sealedsecret.yaml

   # 2. The same credential as the control plane's own environment.
   kubectl -n deployer-system create secret generic deployer-registry \
     --from-literal=DEPLOYER_REGISTRY_HOST=deployer-registry.deployer-system.svc:5000 \
     --from-literal=DEPLOYER_REGISTRY_USER=deployer \
     --from-literal=DEPLOYER_REGISTRY_PASSWORD="$PASSWORD" \
     --dry-run=client -o yaml \
     | kubeseal --format yaml > deploy/registry-credential-sealedsecret.yaml

   rm /tmp/htpasswd
   ```

   Add both generated files to `kustomization.yaml`. They are sealed to this
   cluster's key, so they are safe to commit and useless anywhere else. Rotating
   the password means regenerating both together: the registry and the control
   plane must never hold different halves of it.

   The registry is served over plain HTTP inside the cluster, so this credential
   crosses the pod network in a Basic auth header. That is acceptable only
   because the registry has no Ingress and Cilium enforces policy on that
   network; it is not a credential to reuse anywhere else.

2. **Mint the bootstrap API token.** The one credential an agent presents, on
   both the upload endpoint and the MCP tool. The platform stores only its
   SHA-256 hash, so this printed value is the only copy: keep it where your agent
   sessions can read it, because nothing can recover it later.

   ```bash
   TOKEN="dpl_$(openssl rand -hex 32)"
   echo "$TOKEN"   # the only time you will see it

   kubectl -n deployer-system create secret generic deployer-bootstrap \
     --from-literal=DEPLOYER_BOOTSTRAP_TOKEN="$TOKEN" \
     --dry-run=client -o yaml \
     | kubeseal --format yaml > deploy/bootstrap-sealedsecret.yaml
   ```

   Add it to `kustomization.yaml`. Rotating it is the same two commands: the
   seeding revokes the previous token before minting the new one, so exactly one
   credential works at a time. Leaving the Secret out entirely is supported and
   the pod still starts, it just refuses every call and says so in its log.

   This is the whole auth model until feature 8. There is no revocation path you
   can reach without editing the sealed secret, no minting, and no per app
   ownership check beyond the fact that only one account exists.

3. **The image.** You do not build it by hand. `deployment.yaml` carries a real
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

4. **Set up backups.** Spec 0020's prerequisites. Do these before `/develop`
   wires the config, or the pod starts with variables nothing reads. The order
   that matters is the key pair before the ConfigMap, and the retention rule
   before the first backup ever runs.

   **The age key pair, first.** It is local and free, and the ConfigMap needs its
   public half. Generate it outside the repository:

   ```bash
   brew install age
   cd "$(mktemp -d)"
   age-keygen -o deployer-backup-identity.txt   # prints "Public key: age1..."
   ```

   The file holds the private half, the `AGE-SECRET-KEY-1` line. Put the whole
   file in your password manager under a title panicking future you will find,
   write the secret line out on paper as well, keep the `age1...` public key for
   the ConfigMap below, then delete the temp directory. It is never committed,
   never sealed, and never given to the cluster in any form. Losing this and only
   this makes every backup you hold permanently unreadable, and nothing tells you
   until you try to restore.

   **The bucket.** Cloudflare dashboard, R2. Enabling R2 wants a payment method
   even though the first 10 GB a month is free; a database this size costs
   nothing, the card is a gate rather than a bill. Create a bucket named
   `deployer-backups`, location hint nearest you, everything else default. Note
   the account ID in the R2 sidebar: the endpoint is
   `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` and the region is the literal
   string `auto`.

   **The API token.** R2, Manage R2 API Tokens, Create API token. Permission
   **Object Read and Write**, not either Admin option, which can create and
   destroy buckets. Apply to specific buckets, `deployer-backups` only. Copy the
   Access Key ID and the Secret Access Key into your password manager now; the
   secret is shown once. If you give the token a TTL, put its expiry in your
   calendar, because an expired token is a silently broken backup until the alert
   mail lands.

   **The retention rule, which is doing the job a narrower token cannot.** R2's
   token presets have no per verb editor and Object Read and Write includes
   `DeleteObject`, so the credential the pod holds can delete objects and there is
   no way to take that away. Protection sits on the bucket instead: Settings,
   Bucket Lock, a retention rule of **7 days** over the whole bucket. Nothing,
   the platform included, can then delete a backup in its first week. Seven and
   not thirty because retention and the expiry below have to coexist, and a lock
   as long as the expiry leaves the two contending at the boundary.

   **The lifecycle rule.** Same bucket, Settings, Object lifecycle rules. Name it
   `expire-daily-backups`, prefix **`db/`**, action delete 30 days after creation.
   The prefix is load bearing: the Longhorn registry volume backups land in this
   same bucket under a different prefix and must not be swept by it.

   **Then get the values in.** Credentials sealed, everything else in clear:

   ```bash
   kubectl -n deployer-system create secret generic deployer-backup \
     --from-literal=DEPLOYER_BACKUP_S3_ACCESS_KEY_ID="<access key id>" \
     --from-literal=DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY="<secret access key>" \
     --dry-run=client -o yaml \
     | kubeseal --format yaml > deploy/backup-sealedsecret.yaml
   ```

   Add it to `kustomization.yaml` and give the Deployment a `secretRef` for
   `deployer-backup` marked `optional: true`, the way the Resend key already is.
   That flag is what lets an unconfigured platform still boot.

   The rest goes in `configmap.yaml` as plain values. The age recipient is a
   public key and belongs there in clear; sealing it would only teach a future
   reader that it is sensitive, which is the wrong lesson about how age works.

   ```yaml
   DEPLOYER_BACKUP_AGE_RECIPIENT: "age1..."
   DEPLOYER_BACKUP_S3_ENDPOINT: "https://<ACCOUNT_ID>.r2.cloudflarestorage.com"
   DEPLOYER_BACKUP_S3_BUCKET: "deployer-backups"
   DEPLOYER_BACKUP_S3_REGION: "auto"
   DEPLOYER_BACKUP_ALERT_EMAIL: "<where failure mail goes>"
   ```

   Those six, the recipient through the alert email, are all or nothing:
   `internal/config` refuses to start on a partial set and starts happily on an
   empty one. The interval and the region are the two optional ones, defaulting
   to 86400 and `auto`.

5. **Export the sealed secrets controller key.** One command, and without it a
   rebuilt cluster cannot decrypt a single SealedSecret in the GitOps repository,
   so it cannot boot from git even though every manifest is sitting right there.

   ```bash
   kubectl -n kube-system get secret \
     -l sealedsecrets.bitnami.com/sealed-secrets-key=active \
     -o yaml > sealed-secrets-key.yaml
   ```

   Store it beside the age identity in your password manager, then delete the
   local copy. Repeat whenever the controller rotates its key. The platform is
   granted no permission to do this for you: its RBAC is scoped per app namespace
   and does not reach `kube-system`.

6. **Back up the registry volume with Longhorn.** The database is only one of the
   platform's three stores. The registry volume holds every app image that was
   ever built, and no Go code in this repository touches it: it is a Longhorn
   recurring job, configured in the `k3sprox-gitops` repository, targeting the
   same bucket. Point Longhorn's backup target at the R2 bucket, then add a
   recurring job of type `backup` on the registry volume with a retention of 30.
   Both are in `manifests/longhorn/` there, with the one step no manifest can do
   for itself: a volume joins a recurring job by carrying a label, and a
   dynamically provisioned volume is named after its PV uuid, so labelling it is
   a command rather than a file. Redo it if the registry PVC is ever recreated.

   The job is in its own group rather than in Longhorn's `default` group, and
   that is not tidiness. Every volume in the cluster is born labelled into
   `default`, including the one this database lives on, so a job placed there
   would start shipping this file off site unencrypted from a live WAL mode
   volume, which is the consistency coin flip this whole spec exists to avoid.

   Two things about it are worth knowing rather than discovering. Its objects
   land outside the `db/` prefix, which is exactly why the lifecycle rule above
   carries that prefix rather than sweeping the whole bucket. And Longhorn
   encrypts at the volume level rather than the backup, so these blobs sit in the
   bucket unencrypted. They are user application images rather than platform
   secrets, which is why that was accepted; changing it means migrating the
   volume to an encrypted StorageClass, not setting a flag.

7. **Know what is deliberately not backed up.** `/data/uploads` is out of scope
   and stays that way. Source tarballs there are short lived and replaceable by
   the agent that uploaded them, and putting unbounded user supplied content in a
   daily object is a cost with no matching recovery value. A restore therefore
   brings back accounts, apps, releases, configuration and the run record, and
   brings back no pending upload. An upload interrupted by whatever caused the
   restore is re uploaded, not recovered.


## Restoring the database

This is the whole path, and it is worth walking once on a scratch instance before
you ever need it (spec 0020, AC-25). Nothing here can be done without the private
age identity, which is not in this cluster and never will be. If you have lost
that, no step below helps and none of your backups are readable.

Everything runs from your own machine. `deployer restore` is a subcommand of the
same binary the pod runs, and it opens no database and loads none of the control
plane's configuration.

1. **Pick an object.** The admin backups page at `/admin/backups` lists every run
   with its object key. Copy the key of the newest `succeeded` one, which looks
   like `db/20260815T030000Z-bkp_01J....age`.

2. **Fetch and decrypt it.** The identity is a file path, never a variable and
   never an argument, so it stays out of your shell history and out of a process
   listing.

   ```bash
   export DEPLOYER_BACKUP_S3_ENDPOINT="https://<ACCOUNT_ID>.r2.cloudflarestorage.com"
   export DEPLOYER_BACKUP_S3_BUCKET="deployer-backups"
   export DEPLOYER_BACKUP_S3_ACCESS_KEY_ID="..."
   export DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY="..."

   go run ./cmd/deployer restore \
     -key db/20260815T030000Z-bkp_01J....age \
     -identity ./deployer-backup-identity.txt \
     -out ./restored.db
   ```

   It refuses to write to a path that already exists, and it runs
   `PRAGMA integrity_check` on the result before it tells you it worked. It
   restores to a file and stops there: putting that file on the volume is the
   next step, deliberately, because that step destroys whatever is there now.

3. **Look at it before you trust it.** A restore you have not read is a guess.

   ```bash
   sqlite3 ./restored.db "SELECT COUNT(*) FROM accounts; SELECT COUNT(*) FROM apps;"
   ```

4. **Scale the control plane down.** One replica and a `Recreate` strategy mean
   nothing else is writing, but the file must not be swapped under a running
   process.

   ```bash
   kubectl -n deployer-system scale deploy/deployer --replicas=0
   kubectl -n deployer-system rollout status deploy/deployer --timeout=120s
   ```

   If ArgoCD is watching this, pause it first, or it will scale the Deployment
   straight back up underneath you.

5. **Place the file.** The pod is gone, so copy through a throwaway pod that
   mounts the same volume. The database is in WAL mode, so the two sidecar files
   have to go with it: leaving a stale `-wal` beside a restored `.db` is how a
   restore silently reverts part of itself.

   ```bash
   kubectl -n deployer-system run restore-shell --restart=Never --image=busybox \
     --overrides='{"spec":{"containers":[{"name":"restore-shell","image":"busybox","command":["sleep","3600"],"volumeMounts":[{"name":"data","mountPath":"/data"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"deployer-data"}}]}}'
   kubectl -n deployer-system wait --for=condition=Ready pod/restore-shell --timeout=120s

   kubectl -n deployer-system exec restore-shell -- sh -c \
     'mv /data/deployer.db /data/deployer.db.before-restore 2>/dev/null; \
      rm -f /data/deployer.db-wal /data/deployer.db-shm'
   kubectl -n deployer-system cp ./restored.db restore-shell:/data/deployer.db
   kubectl -n deployer-system delete pod restore-shell
   ```

   The old file is moved aside rather than deleted. If the restore turns out to
   be the wrong object you still have what you had.

6. **Scale back up.**

   ```bash
   kubectl -n deployer-system scale deploy/deployer --replicas=1
   kubectl -n deployer-system rollout status deploy/deployer --timeout=180s
   ```

7. **Check it, in this order.**
   - The pod is `Ready`, which means migrations ran against the restored file.
   - You can sign in at `/login` with an account that existed when the backup was
     taken. This is the real check, and the one AC-25 asks for: the rest proves
     the file arrived, this proves it is the platform.
   - `/admin/backups` lists the runs the restored copy knew about, and any run
     left `running` by the pod you scaled away is now `failed: stranded`, which is
     the startup sweep working rather than a problem.
   - Your apps are still running. Nothing about a database restore touches them:
     they are their own namespaces and their own Deployments, and the platform
     reconciles from what it finds.

8. **Clean up.** Delete `./restored.db` and the local copy of the identity if you
   copied it out of your password manager. Remove
   `/data/deployer.db.before-restore` once you are confident, and resume ArgoCD.

## The five things no file here can tell you

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
5. Tell k3s the in cluster registry is served over plain HTTP. The registry has
   no certificate on purpose (it has no Ingress and is reachable only on the pod
   network), but containerd will refuse to pull from it until it is told so.
   Without this every app deploy fails at the pull, not at the build, which is a
   confusing place to discover it.

   On **every** node, create or extend `/etc/rancher/k3s/registries.yaml`:

   ```yaml
   mirrors:
     "deployer-registry.deployer-system.svc:5000":
       endpoint:
         - "http://deployer-registry.deployer-system.svc:5000"
   ```

   Then restart k3s on that node (`systemctl restart k3s` on the server,
   `systemctl restart k3s-agent` on the workers). Missing it on one worker means
   deploys succeed or fail depending on where the pod is scheduled, which is the
   worst version of this failure, so do all four.

   The build side needs no equivalent: the Buildpacks lifecycle is told the
   registry is insecure through `CNB_INSECURE_REGISTRIES` on the build container,
   which the control plane composes.
