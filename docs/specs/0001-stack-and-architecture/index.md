# 0001. Stack and architecture for the Deployer control plane

**Date**: 2026-08-11
**Status**: Accepted

## Summary

Deployer is one Go program that runs as a single pod inside your k3s cluster. It speaks MCP (Model Context Protocol, the standard AI coding agents use to call tools) over HTTP, accepts an uploaded source tarball, launches a Kubernetes Job to build it into a container image, pushes that image to a registry running in your cluster, and then creates the Kubernetes objects that run the app. All of its own state (accounts, tokens, apps, deployments, releases) lives in one SQLite file on a Longhorn volume.

The shape of the decision is deliberately boring: one binary, no message broker, no database server, no build operator, no framework. Everything that needs to scale or retry is a Kubernetes object, because your cluster already knows how to schedule, retry, and clean those up.

Reasoning, the options weighed, and the sources: see [rationale.md](rationale.md).

## Decision

**Chosen option**: Option 1: A single Go binary using the cluster as its scheduler.

The control plane, the MCP server, and the deployment reconciler are one Go program deployed as a single replica in the cluster. Builds run as Kubernetes Jobs it creates and watches. State lives in SQLite. Nothing else is introduced that the cluster is not already running.

**Implementation skills**: `senior-kubernetes-engineer` (`.claude/skills/senior-kubernetes-engineer/`) · `golang-patterns` (`.claude/skills/golang-patterns/`) · `golang-testing` (`.claude/skills/golang-testing/`) · `mcp-server-patterns` (`.claude/skills/mcp-server-patterns/`) · `mcp-builder` (`anthropics/skills`, `.agents/skills/mcp-builder/`) · `database-migrations` (`.claude/skills/database-migrations/`) · `docker-patterns` (`.claude/skills/docker-patterns/`) · `security-patterns` (`.claude/skills/security-patterns/`)

## Proposed stack

| Layer | Choice | Reason |
|---|---|---|
| Language | Go (latest stable, pinned by a `toolchain` directive in `go.mod`) | `client-go` is the first class Kubernetes client, the binary is static and needs no runtime in the image, and every tool this platform orchestrates is itself Go |
| Agent protocol | Official Go MCP SDK, `github.com/modelcontextprotocol/go-sdk`, streamable HTTP transport | The caller is a remote agent reaching a cluster workload, so stdio is not an option; the SDK plugs into a plain `http.Handler` |
| HTTP layer | Standard library `net/http` with `ServeMux` pattern routing | Method and path routing has been in the stdlib since Go 1.22 and this API has under a dozen routes, so a router is a dependency with no job |
| Process split | One binary, one replica, `Recreate` update strategy | SQLite has one writer and the reconcile loop must not run twice; a second replica would corrupt both assumptions |
| Primary DB | SQLite (`modernc.org/sqlite`, pure Go, no cgo) on a Longhorn volume | One file, no server to operate, and cgo free keeps the static binary and cross compilation intact. One replica means a single writer is not a limit |
| DB access | `sqlc` generating typed Go from hand written SQL | The SQL stays the source of truth, no reflection or runtime ORM, and no hand written `Scan` calls to rot as the schema grows |
| Migrations | `goose`, embedded with `go:embed`, run at process start | Migrations ship inside the binary, so the deployed image and its schema can never disagree. One replica means no migration race |
| Kubernetes access | `k8s.io/client-go` typed clientset, v0.35 to match k3s 1.35 | The work is imperative (create Deployment, Service, Ingress, NetworkPolicy, then watch a Job) and needs no manager, scheme wiring, or reconcile framework |
| Buildpacks build | Cloud Native Buildpacks lifecycle running in a Kubernetes Job | Runs unprivileged, needs no operator or CRDs, and the platform owns the Job spec so isolation defaults cannot be bypassed |
| Dockerfile build | BuildKit rootless in a Kubernetes Job | Same Job shape, different image and args. It is the reference Dockerfile builder and is actively maintained, unlike kaniko |
| Registry | `distribution/registry` v3 in cluster, htpasswd auth, Longhorn volume | The CNCF reference registry, no database, no UI to secure. Builds push with a write credential, app namespaces pull with a read only `imagePullSecret` |
| Source transfer | Plain HTTP upload endpoint returns an upload id; the build Job's init container fetches it with a single use token | Keeps a multi megabyte tarball out of the JSON protocol, and works regardless of which node the Job lands on |
| Background jobs | The `deployments` table is the queue; one in process reconcile loop | State survives a restart because it is on disk, and a broker for single digit concurrent builds is infrastructure with no measured need |
| Auth | Platform issued bearer token in the `Authorization` header, stored hashed | Settled in the scope. The full token model is spec territory for feature 8 |
| Config and secrets | Environment variables; sensitive values from a `SealedSecret` | sealed-secrets already runs in your cluster and is the only way a secret can live in a GitOps repository safely |
| Observability | `log/slog`, JSON to stdout, request id and deployment id on every line | Metrics and alerting are explicitly deferred in the scope, so adding them now builds for a need that does not exist |
| Self deployment | Kustomize manifests in `deploy/`, an ArgoCD `Application` in `k3sprox-gitops` | Reuses the GitOps you already run, so the platform itself gets diff and rollback |
| Self image build | `ko`, distroless static nonroot base | No Dockerfile, no Docker daemon, reproducible image straight from the module |
| Repo and CI | GitHub, single Go module, GitHub Actions running `go test` then `ko build` | ArgoCD needs a remote it can read, and this is the least friction path to one |
| Testing | Standard library `testing`, `client-go` fake clientset, real SQLite in a temp file | The deploy logic is testable without a cluster, and the store is tested against the real engine rather than a mock |

## Architecture

**Topology** (three namespaces; feature 4 owns the final layout, this is the shape the stack assumes):

| Namespace | Contents |
|---|---|
| `deployer-system` | The control plane pod, its Longhorn volume, and the registry |
| `deployer-builds` | One short lived build Job per deployment, nothing long running |
| `app-<slug>` | One user app: Deployment, Service, Ingress, NetworkPolicy, pull secret |

**Request path**: agent → Tailscale or LAN ingress → control plane pod → `/mcp` (MCP tools) or `/v1/uploads` (tarball). One process serves all of it.

**Deploy path**: upload lands on the control plane volume → MCP `deploy` tool writes a `deployments` row in state `queued` → the reconcile loop claims it → creates a build Job in `deployer-builds` → the Job's init container fetches and unpacks the tarball, the build container runs the lifecycle or BuildKit and pushes to the registry → the build container writes the image digest to its `terminationMessagePath`, which Kubernetes surfaces in the pod status for the loop to read → the loop creates or updates the app objects in `app-<slug>` → waits for the pod to be ready → marks the deployment `healthy` and records a release.

**Naming** (one slug is the namespace, the image repository, and the hostname label, so it is derived once and stored on the app row):

- Slug: the app name lowercased, non alphanumeric runs collapsed to a single dash, trimmed to 40 characters, plus a dash and a 6 character hash suffix for uniqueness. Never re derived from a name that may change.
- Image reference on push: `<registry-host>/apps/<slug>:<deployment-id>`. The digest is resolved after the push and stored on the release. Every Deployment references the digest, never the tag.
- Hostname: `<slug>.<DEPLOYER_APP_DOMAIN>`.

**Build handoff contract** (the one place the platform and a build container agree on something):

| Direction | Mechanism |
|---|---|
| Tarball to the build Job | Init container fetches `GET /v1/uploads/<id>` from the control plane service with a single use token, unpacks into an `emptyDir` |
| Upload token | Opaque 32 byte random value, stored on the deployment row, redeemable once (enforced by a conditional SQL update), one hour TTL |
| Digest back to the platform | Build container writes the digest to `terminationMessagePath`; the loop reads it from the pod status after the Job completes |
| Failure reason back | Same channel, a short sanitized string; full build output stays in the Job's pod logs |

**Reconcile loop**:

- Ticks every 2 seconds, with exponential backoff up to 60 seconds after a Kubernetes API error.
- Claims a row with a single conditional `UPDATE ... WHERE state = 'queued' AND claimed_at IS NULL`, setting `claimed_at` and `claimed_by`. Single replica means this is belt and braces, but it makes a second process a bug rather than corruption.
- On every tick, and on start, sweeps non terminal deployments and reconciles them against actual cluster state. A deployment whose build Job no longer exists (node died, evicted, TTL reaped) is marked `failed` with a reason, never left running forever. This is also what picks up or fails out a build that was in flight when the platform pod was recreated.
- Deletes the uploaded tarball once its deployment reaches a terminal state, and sweeps orphaned uploads older than 24 hours.

**Package layout**:

```
cmd/deployer/          the one binary
internal/mcp/          MCP tool definitions and handlers
internal/httpapi/      upload endpoint, health, middleware
internal/store/        sqlc generated code, migrations, SQLite setup
internal/build/        Job construction for the lifecycle and BuildKit paths
internal/deploy/       Deployment, Service, Ingress, NetworkPolicy construction
internal/reconcile/    the deployment state loop
deploy/                Kustomize manifests for the platform itself
```

**Key invariants** (the build must hold these; every one is load bearing):

- Exactly one control plane replica. The Deployment strategy is `Recreate`, never `RollingUpdate`, because two pods would mean two writers on one SQLite file and two reconcile loops claiming the same row.
- The platform composes every workload manifest field by field in Go. No user supplied value is ever merged into a pod spec, and no manifest is built by string templating. This is what makes slice 5 enforceable rather than advisory.
- Every deployment state transition is a database write before it is an action, so a restart mid build can tell a running build from a lost one.
- Images are deployed by digest, never by mutable tag, so a rollback re promotes an exact image and a tag push cannot silently change a running app.
- SQLite runs with WAL journaling, `foreign_keys=ON`, and a busy timeout, set once at open.
- A deployment is never left in a non terminal state without a live Kubernetes object behind it. If the object is gone, the sweep fails the deployment with a reason.
- The control plane mints the read only `imagePullSecret` into `app-<slug>` itself at deploy time. The push credential lives only in `deployer-system` and never reaches an app namespace.

**Configuration required**:

- `DEPLOYER_DB_PATH`: SQLite file path on the mounted volume
- `DEPLOYER_UPLOAD_DIR`: where uploaded tarballs land before a build claims them
- `DEPLOYER_REGISTRY_HOST`: in cluster registry service address
- `DEPLOYER_REGISTRY_USER`, `DEPLOYER_REGISTRY_PASSWORD`: push credential, from the `SealedSecret`
- `DEPLOYER_APP_DOMAIN`: the wildcard domain apps are served under
- `DEPLOYER_BUILD_NAMESPACE`: where build Jobs are created
- `DEPLOYER_MAX_UPLOAD_BYTES`: hard cap on an accepted tarball, default 100 MB
- `DEPLOYER_LOG_LEVEL`: slog level

**Storage**: one 10Gi Longhorn PVC in `deployer-system` holds both the SQLite file and the upload directory, at 2 replicas. Two rather than three keeps write latency down for a single writer database while still surviving one node loss.

**CI credentials**: the `ko` push credential for the platform's own image is a GitHub Actions secret. It is a setup step, not something the code reads, and it is called out here so it does not get discovered during the first deploy.

**Boundaries with other specs** (this spec does not decide these):

| Concern | Owned by |
|---|---|
| Table and column design, the deployment state machine's exact states | Feature 3, platform data model |
| Namespace policy, the control plane's service account rights, wildcard DNS and TLS | Feature 4, cluster foundation |
| The MCP tool surface itself (names, arguments, responses), and the readiness contract for a deployed app (probe type, path, timeout, what happens when an app has no health endpoint) | Feature 5, first deploy end to end |
| Token format, hashing, revocation, ownership checks | Feature 8, accounts and API tokens |
| The exact pod security and NetworkPolicy rules | Feature 9, workload isolation |

## Consequences

**Positive**:

- One image, one deployment, one log stream to read when something breaks at 2am.
- Nothing new is introduced that your cluster does not already run, except the registry and the platform itself. Cilium, Longhorn, ArgoCD, sealed-secrets, and ingress-nginx all get reused rather than duplicated.
- Builds inherit Kubernetes scheduling, resource limits, and cleanup for free, and a build cannot starve the API because it is a different pod.
- The static Go binary on a distroless base means a small image and a small attack surface for the component holding cluster credentials.
- A restart is safe at any point, because the deployment state is on disk rather than in memory.

**Negative / tradeoffs**:

- The control plane is a single point of failure and cannot be scaled horizontally. SQLite plus one reconcile loop is a deliberate ceiling. Raising it means moving to Postgres and adding row claiming with locks, which is a real migration, not a config change.
- Every deploy of the platform is a brief outage, because `Recreate` stops the old pod before starting the new one.
- The platform volume becomes the thing you must back up. If it is lost, the cluster still runs every app, but Deployer no longer knows any of them exist.
- Two build engines means two Job templates, two base images to track, and two sets of failure output to sanitize.
- `sqlc` and `goose` both add a code generation or migration step that a new contributor has to learn before their first schema change.
- Go has a smaller MCP ecosystem than TypeScript. The official Go SDK is real and current, but examples and community middleware are thinner, so more will be hand written.

**Neutral**:

- Buildpacks and BuildKit are both invoked as container images with arguments rather than as libraries, so upgrading either is a tag bump in the Job template.
- The MCP transport being HTTP means the same endpoint serves the tarball upload, so there is exactly one thing to expose and one thing to authenticate.

## Follow-up

- [ ] Connect a Kubernetes MCP server (`containers/kubernetes-mcp-server` was the most credible found) in your MCP settings so the build can read real cluster state rather than assume it. Read only access is the safe default. I cannot connect it for you, it is a client config step.
- [ ] The platform database has no backup story. It is explicitly deferred in the scope, but it is the one piece of state whose loss is not recoverable from the cluster, so decide it before you rely on Deployer.
- [ ] `AGENTS.md` does not exist yet, so none of the conventions from the eight skills above are captured. Feature 2 (`/audit`) should write it once the scaffold is real.
- [ ] Confirm `k3sprox-gitops` is on GitHub. If it is hosted elsewhere, the ArgoCD `Application` source and the CI push target both change.
- [ ] Decide whether the enforced pod security defaults (non root, dropped capabilities, resource ceilings) ship in slice 1's deploy path rather than waiting for slice 5. See the premise note in [rationale.md](rationale.md).

## Rationale

Reasoning, the full stacks weighed, and the sources: see [rationale.md](rationale.md).
