# 0009. Dockerfile build path: rationale

Reasoning and options behind [index.md](index.md). Not read during a build.

## Context

Every deploy today runs the Paketo lifecycle over the uploaded source. That covers the ordinary case well and covers nothing else: an app with a compiled asset step the buildpacks do not detect, a base image the project needs for a native dependency, or any language outside the builder simply has no route onto the platform. The scope settled this at the start (basis: `docs/scope/scope.md`, "Builds use Buildpacks with no config, and fall back to the project's Dockerfile when one is present"), and this is the slice that pays for it.

Three earlier decisions constrain how. Spec 0001 already chose rootless BuildKit as the Dockerfile engine and said why, so the engine is not reopened here. Spec 0003 deliberately left `deployer-builds` without a pod security level, writing down that rootless BuildKit might not fit `restricted` and that finding out mid build would be worse than deciding it with the build path in front of you. That decision is now due. Spec 0002 put a `build_path` column on `deployments` and a `BuildPath` field on the store's build result, both unused, waiting for this feature to fill them.

The shape of a Kubernetes Job forces the hardest constraint. A Job's container image is fixed when the Job is created, and the source archive is fetched and unpacked by the Job's own init container, which runs after that. So at the moment the platform must decide which engine to run, it has not yet looked inside the archive. Something has to give: either the decision moves earlier, onto the control plane, or the Job has to be able to run either engine after the fact.

There is also a refusal waiting. Spec 0004 wrote `image_runs_as_root` and the registry config read behind it, and `/check verify` could never prove either, because Paketo will not produce a root running image. Spec 0005 deferred the same projection for the same reason. A Dockerfile can trivially produce one, so this slice is where both stop being untestable.

## Options considered

### Option 1: a second Job shape, selected on the control plane, namespace at `baseline`

The control plane walks the stored archive's tar headers, picks the engine, records it, and composes the matching Job. Rootless BuildKit runs as one `buildctl-daemonless.sh` container with the native snapshotter. `deployer-builds` drops to `baseline` so BuildKit can carry the unconfined seccomp profile it needs.

**Pros**:

- One Job, one engine, one attempt. The Job stays the same shape it is today, with one field varying.
- The path is known before the Job exists, so `build_path` is a database write before it is an action, and a failed build still reports its engine.
- Detection is a pure function over tar headers, which is cheap to test exhaustively without a cluster.
- One namespace, one network policy, one RBAC surface, one set of deviations to document.

**Cons**:

- The build namespace loses enforced `restricted`, so the composer becomes the only guard on the Buildpacks pod's shape.
- The control plane now parses caller supplied bytes it previously only stored and hashed.
- If a project's structure is unusual, detection can disagree with what the engineer expected, and there is no override to correct it.

### Option 2: one Job shape, detection inside the build pod

`fetch-source` unpacks and detects, and the build container branches on what it found. Either a single image carries both engines, or the pod runs two build containers and one no-ops.

**Pros**:

- Detection sits next to the extractor, so one component owns everything that reads the archive.
- The control plane never opens the archive at all.

**Cons**:

- Needs either a bespoke image carrying both the lifecycle and BuildKit, which is a third artifact to build, pin and patch, or a pod that always schedules a container it will not use, pulling an image for nothing.
- The platform learns which path ran only by reading it back out of the pod, so a pod that dies before reporting leaves a deployment that cannot say what it tried.
- The pod security question does not go away. Whatever image runs BuildKit still needs the looser profile, so now the Buildpacks path inherits it too, with no way to tell them apart.

### Option 3: keep `restricted` and make BuildKit fit

Run rootless BuildKit with no process sandbox and the native snapshotter, under a `RuntimeDefault` seccomp profile, and accept whatever limitations follow.

**Pros**:

- No pod security deviation anywhere, and no manifest change in `deploy/`.
- Cheapest possible diff if it works.

**Cons**:

- `restricted` permits only `RuntimeDefault` or `Localhost` seccomp, and rootless BuildKit's documented Kubernetes setup asks for an unconfined profile precisely because the syscalls its worker needs are filtered otherwise.
- The failure mode is a build that fails deep inside a pod with an opaque error, which is the exact outcome spec 0003 wrote its follow up to avoid.
- Discovering it does not work costs a day, and the fallback is Option 1 anyway.

### Option 4: a shared, long lived buildkitd

One persistent builder Deployment serves every build over gRPC, holding a warm layer cache.

**Pros**:

- Real cache reuse across deploys, which is the single largest speedup available here.
- The build Job becomes a thin client with no privileges at all.

**Cons**:

- Every account's untrusted Dockerfile runs through one shared process holding one shared cache, on a platform whose whole isolation story is per app namespaces and per build credentials. That is a tenancy boundary going backwards.
- A stateful, long lived component to operate, patch and size, on a homelab that has deliberately avoided adding operators.
- Cache eviction, cache poisoning between apps, and a cold start after every restart all become yours to answer.

## Rationale

The Job shape decides this. A Job's image is fixed at creation and the archive is opened after, so unless the platform is willing to ship a bespoke dual engine image or waste a container per build, the decision has to move onto the control plane. Once it moves there it gets better rather than worse: the path becomes known before the Job exists, which lets `build_path` follow the project's own rule that a state change is a database write before it is an action, and which makes a failed build able to say what failed. Option 2 buys one component owning archive reads and pays for it with a third artifact to maintain and a deployment that cannot report its own path. That is the wrong trade.

Relaxing `deployer-builds` to `baseline` rather than forking a second namespace rests on what is actually protecting these pods. Every field of both build pods is composed in Go, field by field, and the project rule that no caller supplied value reaches a pod spec means the composer, not admission, is the primary control here. Pod security is defence in depth behind it. Given that, a second namespace buys a stronger guarantee on the path that was never the risk, and charges a second network policy, a second RBAC surface, and a fork in the composer for it. The honest move is one namespace, two named deviations, written down, plus a test to replace the guarantee that was lost. Option 3 was tempting for exactly one reason, the empty diff, and it heads straight at the failure spec 0003 predicted in writing.

The remaining choices all follow from the same instinct, which is to keep this slice one decision wide. The native snapshotter over `fuse-overlayfs` costs speed and saves a device mount, so the pod security deviation stays a short named list rather than a growing one (four fields, as the correction below records, not the two first claimed here). No cache, no build arguments and no secrets keeps a build a pure function of the archive, and keeps values out of published image layers until slice 7 decides what configuration means. Reusing `build_failed` rather than adding a thirteenth reason code holds the closed set closed: the caller already learns which engine ran from `build_path`, so the code carries no information the caller lacks, and only its message needed fixing, since it currently tells every failed build to check that the app builds with Cloud Native Buildpacks. Option 4's cache is genuinely the biggest win available and is still wrong now, because it trades the platform's tenancy story for speed on a cluster deploying throwaway apps.

## The deviation correction

_Added 2026-08-13, during the build, after build step 3 ran against the pinned image on the cluster._

This spec first claimed the BuildKit pod deviates from `restricted` in exactly two fields, seccomp and AppArmor, and held `allowPrivilegeEscalation` false as a field to prove rather than assume. It said in writing that a third deviation would be a decision to reopen, not a field to flip. Build step 3 reopened it.

Four probe pods ran the pinned image `moby/buildkit@sha256:5047…` on the cluster, each changing one thing:

| Pod security | Result |
|---|---|
| escalation false, drop `ALL`, seccomp and AppArmor unconfined (this spec's original shape) | `rootlesskit: newuidmap … operation not permitted` |
| the same, plus `ROOTLESSKIT=""` to skip the remapper entirely | `buildkitd: rootless mode requires to be executed as the mapped root in a user namespace` |
| escalation **true**, drop `ALL` | `newuidmap … operation not permitted` |
| escalation **true**, drop `ALL`, add `SETUID` and `SETGID` | the build completed: base image pulled, `RUN` executed |

A fifth probe tested the escape route worth testing before accepting a widening: Kubernetes pod user namespaces (`hostUsers: false`, available here on k8s 1.35, containerd 2.2, kernel 6.8). It did not help. `newuidmap` still could not set caps with escalation disabled, because a pod user namespace does not put the two capabilities into the exec'd helper's set.

The third probe is the interesting one, and it is what turns this from "the spec was wrong" into a better statement of the same risk. Allowing escalation on its own changed nothing. The build only worked once the two capabilities were in the bounding set. So the bounding set, not the escalation flag, is what bounds this pod, and `SETUID` plus `SETGID` is a far more precise ceiling than "escalation allowed" sounds like. The spec now pins the bounding set narrowly (AC-10a) instead of pinning a flag that was never the control.

Three shapes were weighed on that evidence:

- **Accept four deviations, one namespace** (chosen). The Buildpacks pod is unchanged and still composes every `restricted` field; only the BuildKit pod widens, and it widens to a ceiling that can be written down exactly and pinned in a test.
- **Split the namespaces**, keeping `deployer-builds` at enforced `restricted` for Paketo and giving BuildKit its own `baseline` namespace. This buys an admission enforced guarantee on the path that was never the risk, and charges a second network policy, a second RBAC binding, a new configuration variable, and a namespace fork through job create, job watch, log read and the per Job credential. It is the fallback if the deviation ever grows past four.
- **Build a BuildKit image with no subuid entries**, so RootlessKit maps a single uid and needs no setuid helper at all, keeping the strict context. Rejected for the reason this spec already rejected Option 2: a third artifact to build, pin and patch. It also fails quietly, since a single mapped uid cannot unpack a base image whose files are owned by more than one user, so common base images would break in a way the agent could not read.

Two operational findings came out of the same probes and are now in the design section rather than left to be rediscovered. RootlessKit stats its state directory under `TMPDIR`, and the pinned image points `TMPDIR` inside the home directory, so the state volume mounted there makes it disappear; `TMPDIR` has to be set to something that survives the mount. And `buildctl-daemonless.sh` calls `mktemp` in `/tmp` directly, so that container's root filesystem cannot be read only.

## References

**Project sources** (verifiable, in this repo):

- `docs/scope/scope.md`, the settled MVP decision that builds fall back to the project's Dockerfile when one is present, and feature 10's "done when"
- spec [0001](../0001-stack-and-architecture/index.md), which chose rootless BuildKit in a Kubernetes Job for this path and named the two engine cost
- spec [0002](../0002-platform-data-model/index.md), which added `deployments.build_path` and the store's `BuildPath` field for this feature
- spec [0003](../0003-cluster-foundation/index.md), whose follow up left `deployer-builds` pod security undecided until the build path was in front of you
- spec [0004](../0004-first-deploy-end-to-end/index.md), the existing build Job, the registry image user read, and the deferred AC-10 root image refusal
- spec [0008](../0008-workload-isolation-network-policy/index.md) and `deploy/builds-networkpolicy.yaml`, whose public egress allowance already covers base image pulls
- `AGENTS.md`, the rules that every manifest is composed field by field, that builder uids are read off pinned images rather than assumed, that deploys go by digest, and that a failure a caller sees is a closed reason code
- `internal/source/extract.go`, which performs no prefix stripping, so an archive entry named `Dockerfile` is what lands at the root of the build context

**Practices & standards**:

- Kubernetes Pod Security Standards, the `restricted` and `baseline` levels, and what each permits for seccomp, AppArmor, privilege escalation and capabilities
- Linux capability bounding sets and the no new privileges bit, and why the bounding set rather than the escalation flag is what bounds a setuid binary
- RootlessKit's uid mapping, which uses the setuid `newuidmap` and `newgidmap` helpers when subuid entries exist and a single uid map otherwise
- Kubernetes pod user namespaces (`hostUsers: false`), tested here and found not to remove the need for those capabilities
- Rootless container builds without a daemon or a privileged pod, the reason this path exists at all rather than mounting a Docker socket
- Defence in depth: admission policy as a second guard behind a composer that is the real control, and a test where an enforced guarantee is given up
- Deploy by immutable digest, and repin an image together with the identity it declares
