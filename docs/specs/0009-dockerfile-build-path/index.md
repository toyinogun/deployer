# 0009. Dockerfile build path: the escape hatch for apps Buildpacks cannot handle

**Date**: 2026-08-13
**Status**: In Progress

## Summary

When an uploaded project ships a `Dockerfile` at its root, the platform builds that instead of running Buildpack detection. The control plane decides which path to take by reading the entry names inside the stored archive, before it composes the build Job, because a Job's container image is fixed the moment it is created. The Dockerfile path runs rootless BuildKit (a container image builder that needs no Docker daemon) as a single throwaway container, which does not fit the `restricted` pod security level, so the build namespace drops to `baseline` with four named deviations. A Dockerfile can finally produce an image that runs as root, so the refusal spec 0004 wrote and could never reach becomes live here.

## Requirements

**User stories**:

- As an agent that just wrote an app with a `Dockerfile`, I want the platform to build it as written, so an app Buildpacks cannot detect still deploys.
- As an agent whose project has no `Dockerfile`, I want nothing to change, so the zero configuration path stays zero configuration.
- As an agent whose build failed, I want to know which build engine ran, so I know whether to fix my `Dockerfile` or my project layout.
- As the operator, I want the looser pod security this needs to be a short list of named deviations on one pod in one namespace, not a general relaxation.

**Acceptance criteria** (the contract, each independently checkable):

- **AC-1**: An uploaded archive containing an entry whose cleaned name is exactly `Dockerfile` builds through BuildKit, and the app it produces is reachable on its hostname.
- **AC-2**: An archive with no such entry builds through the Paketo lifecycle exactly as it does today, with no observable change.
- **AC-3**: The path is chosen on the control plane, by reading the stored archive's tar entry names before the build Job is composed. The control plane never extracts the archive to its own filesystem, and the walk is bounded by the same `DEPLOYER_MAX_UPLOAD_FILES` limit the extractor enforces, so a header only archive cannot make the control plane work without limit.
- **AC-4**: `deployments.build_path` is written as `dockerfile` or `buildpacks` at the moment the path is chosen, before the Job exists, so a deployment that fails still reports which engine ran.
- **AC-5**: `deployment_status` reports `build_path` for any deployment that has reached `building` or beyond, and omits it before that.
- **AC-6**: An archive that cannot be read as a gzip tar during detection, or that exceeds the entry limit while being walked, fails the deployment with `source_rejected`, before any Job or credential is created.
- **AC-7**: The Dockerfile build is one container running `buildctl-daemonless.sh` from the digest pinned rootless BuildKit image, using the native snapshotter, pushing straight to the in cluster registry over plain HTTP, with no cache export, no build arguments, no secrets, and no entitlements granted.
- **AC-7b**: The BuildKit container's writable state is supplied as configuration the composer sets, not left to the image's own defaults, because a state volume mounted over the image's tree hides directories the daemon's startup depends on. `HOME` and `XDG_RUNTIME_DIR` resolve inside the mounted volume, `TMPDIR` resolves somewhere that exists after the mount, and the container's root filesystem stays writable, since `buildctl-daemonless.sh` uses `/tmp` directly.
- **AC-7a**: Only a regular file entry counts as the root `Dockerfile`. A directory or link entry whose name cleans to `Dockerfile` does not select the Dockerfile path.
- **AC-8**: The BuildKit container runs as the uid and gid its own pinned image declares, supplied as `DEPLOYER_BUILDKIT_UID` and `DEPLOYER_BUILDKIT_GID`, never a Go constant and never the Paketo pair.
- **AC-9**: CI fails when either pinned build image's declared user or group drifts from the pair configured for it. The Paketo pair is read from the image's `CNB_USER_ID` and `CNB_GROUP_ID`; the BuildKit pair is read from the OCI config's `Config.User`, through the same parse `registry.ImageUser` already performs.
- **AC-10**: `deployer-builds` enforces `baseline` pod security. The Buildpacks pod still composes every field `restricted` asks for, including `allowPrivilegeEscalation` false and all capabilities dropped. BuildKit's deviations are exactly four and all four are named: an unconfined seccomp profile, an unconfined AppArmor profile, `allowPrivilegeEscalation` true, and `SETUID` plus `SETGID` added back on top of dropping all capabilities. Nothing else differs.
- **AC-10a**: The BuildKit container's capability bounding set is exactly `SETUID` and `SETGID`. That set, not `allowPrivilegeEscalation`, is what bounds the privilege a setuid binary in this pod can reach, so it is the field that has to be pinned narrowly rather than the one that has to be false.
- **AC-11**: A unit test pins that the Buildpacks pod still satisfies every `restricted` field, and that the BuildKit pod deviates in exactly those four named fields and no other, with its added capability list equal to `SETUID` and `SETGID`.
- **AC-12**: An image produced by a Dockerfile build that declares no non root user is refused with `image_runs_as_root`, before a single app object is composed. This closes spec 0004's AC-10.
- **AC-13**: A failed Dockerfile build ends the deployment with `build_failed`, and that code's message no longer names Cloud Native Buildpacks.
- **AC-14**: Build output stays in the Job's pod logs on both paths. It never reaches the MCP response, the database, or the platform log at info level.
- **AC-15**: Both build pods carry an ephemeral storage request and limit, so a build copying a large base image cannot fill a node's disk.
- **AC-16**: `deploy_app`'s tool description states that a root `Dockerfile` is built when present and Buildpacks otherwise, in the same commit as the behaviour.
- **AC-17**: `testdata/sample-dockerfile` exists and deploys through the Dockerfile path, while `testdata/sample-go` still deploys through Buildpacks, both provable in one verify run.
- **AC-18**: The BuildKit pod automounts no service account token and holds the same single use registry credential, owner referenced to its Job and collected with it.

## Decision

**Chosen option**: Option 1: a second Job shape, selected on the control plane, with the build namespace at `baseline`.

Detection happens on the control plane before the Job exists, the Dockerfile path is a rootless BuildKit container composed beside the existing Paketo one, and `deployer-builds` drops from `restricted` to `baseline` with four documented deviations on the BuildKit pod rather than forking into a second namespace.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `docker-patterns` (`~/.claude/skills/docker-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`)

## Rationale

Reasoning and options: see [rationale.md](rationale.md).

## Feature design

**Data model**: no schema change. `deployments.build_path` already exists with `CHECK (build_path IS NULL OR build_path IN ('buildpacks', 'dockerfile'))`, and `store.BuildResult` already carries a `BuildPath` field. What changes is when it is written. Spec 0002 sketched it as an output of `RecordBuildResult` after a successful build; it becomes an input to the existing `RecordBuild` call inside `startBuild`, which already runs before the build finishes. A build that fails then still carries its path.

**Path selection** (pure, no cluster, no filesystem writes):

```
open the stored archive read only
  → gzip reader → tar reader → walk headers only, never bodies
  → a regular file entry whose filepath.Clean(name) is exactly "Dockerfile" → dockerfile
  → the walk completes with no such entry                                   → buildpacks
  → the walk errors, or passes DEPLOYER_MAX_UPLOAD_FILES entries            → source_rejected
```

The rule is the tree root and nothing else, matching what the extractor already does: `internal/source` performs no prefix stripping, so an entry named `Dockerfile` is what lands at the root of `/workspace/source`, which is exactly what either engine is pointed at. A `Dockerfile` nested one directory down is ignored, and the build goes to Buildpacks, which is the same answer that tree gets today.

Two things this walk is not allowed to be loose about. It counts entries against the same limit `Extract` enforces, because the control plane is now doing work on caller supplied bytes and an archive of nothing but headers is small on disk and long to walk. And it matches on regular files only: a directory entry named `Dockerfile/` cleans to `Dockerfile`, and letting that select the Dockerfile path would send an archive with no Dockerfile in it to BuildKit, to fail confusingly there instead of building fine through Buildpacks.

Detection lives in `internal/source` as a header only mode of the walk `Extract` already performs, not as a second parser. Two independent readers of the same stream would eventually disagree about what counts as unreadable, and the disagreement would surface as a deployment that detected one thing and was then refused for an unrelated reason.

**State transitions**: unchanged. The path is chosen inside `startBuild`, between the move to `building` and the Job create.

**Build Job surface** (composed field by field in Go, as every manifest is):

| | Buildpacks path | Dockerfile path |
|---|---|---|
| Image | `DEPLOYER_BUILDER_IMAGE` | `DEPLOYER_BUILDKIT_IMAGE` |
| Command | `/cnb/lifecycle/creator` | `buildctl-daemonless.sh` |
| User | `DEPLOYER_BUILD_UID` / `GID` | `DEPLOYER_BUILDKIT_UID` / `GID` |
| Context | `/workspace/source` | `--local context=` and `--local dockerfile=`, both `/workspace/source` |
| Frontend | not applicable | `--frontend dockerfile.v0` |
| Push | direct to registry, `-daemon=false` | `--output type=image,name=<target>,push=true,registry.insecure=true` |
| Plain HTTP registry | `CNB_INSECURE_REGISTRIES` | `registry.insecure=true` on the output, since buildctl has no environment escape hatch for it |
| Snapshotter | not applicable | `native`, via `BUILDKITD_FLAGS` |
| Writable state | none beyond the shared volumes | a `buildkit-state` emptyDir mounted at the image's home directory, with `HOME` and `XDG_RUNTIME_DIR` pointing inside it |
| Temporary directory | image default | `TMPDIR` set to a path that survives the state mount, because RootlessKit puts its state directory under it |
| Root filesystem | writable on the builder, read only on the fetcher | writable, since `buildctl-daemonless.sh` calls `mktemp` in `/tmp` directly |
| Seccomp | `RuntimeDefault` | `Unconfined`, deviation one |
| AppArmor | container default | `unconfined`, deviation two |
| Privilege escalation | false | true, deviation three, so the setuid `newuidmap` can raise into the bounding set |
| Capabilities | drop `ALL` | drop `ALL`, add `SETUID` and `SETGID`, deviation four, and the ceiling everything else rests on |
| Cache, build args, secrets, entitlements | none | none |

Everything else is shared and unchanged: the same `fetch-source` init container, the same workspace volume, the same single use credential, `backoffLimit` zero, `automountServiceAccountToken` false, `allowPrivilegeEscalation` false, all capabilities dropped, the same deadline, and the same CPU and memory bounds.

These are named because leaving them implicit is how this slice fails quietly, and every one of them was learned by running the pinned image on the cluster rather than by reading documentation. `buildctl-daemonless.sh` starts a buildkitd for the one build, and that daemon wants a writable root of its own, which is not the build context and not covered by the volume list the Buildpacks path uses. Mounting that volume then hides part of the image's own tree, which is why `TMPDIR` is set explicitly: RootlessKit stats its state directory under `TMPDIR`, the image points that at a path inside the home directory, and the mount makes it vanish. The pod security deviation is four fields rather than the two the first draft of this spec claimed, and the correction is recorded in [rationale.md](rationale.md) with the evidence.

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| choose the path | `buildpacks` or `dockerfile` | derived by walking the stored archive's tar headers, read from `DEPLOYER_UPLOAD_DIR` on the control plane volume |
| choose the path | the entry limit the walk stops at | `DEPLOYER_MAX_UPLOAD_FILES`, the same value the extractor already enforces, not a second number |
| compose the Job | buildkitd's writable root path | the home directory the pinned BuildKit image declares, mounted as an emptyDir and pointed at by `HOME` and `XDG_RUNTIME_DIR` |
| compose the Job | `TMPDIR` | a platform constant in `internal/build`, chosen so it still exists once the state volume is mounted, never the image's own value, which the mount hides |
| compose the Job | the BuildKit pod's added capabilities | a platform constant in `internal/build`: exactly `SETUID` and `SETGID`, the pair RootlessKit's `newuidmap` needs, never configuration, because it is the privilege ceiling of the one place the platform runs code it did not write |
| compose the Job | the plain HTTP registry allowance | `registry.insecure=true` on the output, derived from the same registry host the target reference already carries |
| compose the Job | builder image | `DEPLOYER_BUILDER_IMAGE` or `DEPLOYER_BUILDKIT_IMAGE`, selected by the chosen path |
| compose the Job | pod uid and gid | `DEPLOYER_BUILD_UID`/`GID` or `DEPLOYER_BUILDKIT_UID`/`GID`, selected by the chosen path |
| CI drift check | the Paketo pair's true value | the pinned builder image's `CNB_USER_ID` and `CNB_GROUP_ID`, as today |
| CI drift check | the BuildKit pair's true value | the pinned BuildKit image's OCI config `Config.User`, the same field `registry.ImageUser` parses, because a BuildKit image follows no `CNB_` convention |
| compose the Job | target image reference | `build.TargetImage(registry host, app slug, deployment id)`, unchanged |
| compose the Job | ephemeral storage request and limit | new constants in `internal/build`, not configuration, for the same reason the CPU and memory pair are constants: they are a product decision about what one build may take |
| record the build | `deployments.build_path` | the chosen path, written through the existing `RecordBuild` call in `startBuild` |
| `deployment_status` | `build_path` | the `deployments.build_path` column, omitted while null |
| resolve the image | the image's declared user | the registry config blob, through `registry.ImageUser`, unchanged |
| fail a deployment | the reason code | the closed set in `internal/domain/reason.go`, no new member |

**Key invariants**:

- The path is a platform derivation from the archive's contents. No caller supplied value selects it, and `deploy_app` gains no argument.
- The path is a database write before it is an action: the row carries it before the Job that runs it exists.
- Both engines are invoked as a pinned image plus arguments, never as a library, so either is repinned without touching Go.
- An image and the uid it runs as are one unit, repinned together and checked together in CI.
- Build output never crosses the reason code boundary on either path.

**Security model**: unchanged for callers. The deviations are confined to one namespace and four named fields on one of the two build pods. The platform composes every field of both build pods itself and no caller value reaches a pod spec, so pod security here was always defence in depth behind the composer rather than the primary control; the composer is unchanged and gains a test that pins it. The BuildKit pod still runs non root as a declared uid, automounts no service account token, is granted no BuildKit entitlements, and sits behind the same `deployer-builds` network policy, which already permits public egress for dependency downloads and therefore needs no change for base image pulls.

What replaces the field this spec first held is a tighter statement about the same risk. `allowPrivilegeEscalation` true sounds like the pod can become root; it cannot. That setting only clears the no new privileges bit, which lets a setuid binary raise into the capability **bounding set**, and this pod's bounding set is exactly `SETUID` and `SETGID`. The proof is in the failed attempt rather than the successful one: with escalation allowed and all capabilities dropped, `newuidmap` still could not run, because there was nothing in the bounding set for it to reach. So the bounding set is the real control, and it is the field to hold narrowly and pin in a test. Escalation without capabilities buys an attacker nothing; capabilities without escalation do not reach an exec'd setuid helper, which is why both are needed together and why neither alone is the risk.

Those two capabilities let a process inside the pod change its own uid and gid within the mapping the pod already has. They do not cross the pod boundary, mount anything, or reach the node. The build still runs as an unprivileged uid on the host, in a namespace with no service account token, with one single use registry credential, on a network fenced by the existing policy.

Detection adds one new exposure, stated plainly: the control plane now parses caller supplied bytes rather than only storing and hashing them. It reads headers only, writes nothing to its own filesystem, and stops at the same entry limit the extractor uses.

**Configuration required**:

- `DEPLOYER_BUILDKIT_IMAGE`: the digest pinned rootless BuildKit image. Validated at startup exactly as `DEPLOYER_BUILDER_IMAGE` is, so a mutable tag is refused.
- `DEPLOYER_BUILDKIT_UID`: the uid that image declares.
- `DEPLOYER_BUILDKIT_GID`: the gid that image declares.

**Critical test scenarios**:

- Happy path: an archive with a root `Dockerfile` deploys through BuildKit and answers on its hostname, verifies **AC-1**, **AC-7**, **AC-17**.
- No regression: an archive without one deploys through Buildpacks unchanged, verifies **AC-2**, **AC-17**.
- Detection unit table: root `Dockerfile`, nested `Dockerfile`, `./Dockerfile`, `dockerfile` lowercase, `Dockerfile.dev`, a directory entry named `Dockerfile/`, none, and a corrupt stream, verifies **AC-1**, **AC-2**, **AC-3**, **AC-6**, **AC-7a**.
- Failure case: an archive of many header only entries stops at the entry limit and fails as `source_rejected` rather than walking to the end, verifies **AC-3**, **AC-6**.
- Failure case: a `Dockerfile` whose final stage sets no `USER` is refused with `image_runs_as_root` before any app object exists, verifies **AC-12**.
- Failure case: a `Dockerfile` with a failing `RUN` ends as `build_failed`, with no build output in the response, the row, or the log, verifies **AC-13**, **AC-14**.
- Failure case: a truncated archive fails as `source_rejected` with no Job created, verifies **AC-6**.
- Composition test: the Buildpacks pod satisfies every `restricted` field, including `allowPrivilegeEscalation` false and all capabilities dropped, and the BuildKit pod differs in exactly the four named fields with an added capability list equal to `SETUID` and `SETGID`, verifies **AC-10**, **AC-10a**, **AC-11**, **AC-18**.
- Reporting: a deployment that failed during a Dockerfile build still reports `build_path: dockerfile`, verifies **AC-4**, **AC-5**.

## Build plan

Ordered as a Tracer Bullet: get one Dockerfile app all the way to a hostname through the real cluster as early as the pieces allow, then thicken the refusals and the guards around it.

1. [x] Add `DEPLOYER_BUILDKIT_IMAGE`, `DEPLOYER_BUILDKIT_UID` and `DEPLOYER_BUILDKIT_GID` to `internal/config` with the same digest and id validation the Paketo pair gets, pin them in `deploy/configmap.yaml`, and generalise CI's `builder uid` step over both images: `CNB_USER_ID` and `CNB_GROUP_ID` for the Paketo one, the OCI config `Config.User` for the BuildKit one, satisfies **AC-8**, **AC-9**.
2. [x] Write the path selection as a header only mode of `internal/source`'s existing walk, test first, over a table of crafted archives, bounded by the extractor's own entry limit and matching regular files only, satisfies **AC-3**, **AC-6**, **AC-7a**, and the detection half of **AC-1**, **AC-2**.
3. [x] Prove the pod shape against the pinned image on the real cluster before anything depends on it. Done, and it corrected this spec: see [rationale.md](rationale.md), "The deviation correction". The proven shape is four deviations, `TMPDIR` set explicitly, and a writable root filesystem.
4. [x] Compose the BuildKit Job beside the existing one: the pinned image, `buildctl-daemonless.sh` with the dockerfile frontend, both `--local` paths, the insecure registry output, the native snapshotter, the `buildkit-state` volume with `HOME`, `XDG_RUNTIME_DIR` and `TMPDIR` set around it, the four named security fields, and the ephemeral storage bounds added to both paths, satisfies **AC-7**, **AC-7b**, **AC-10a**, **AC-15**, **AC-18**.
5. [x] Relax `deployer-builds` to `baseline` in `deploy/`, and land it through ArgoCD before anything creates a BuildKit Job, satisfies **AC-10**. `baseline` permits all four deviations, so none of them needs a further exception.
6. [x] Wire selection into `startBuild`: choose the path, record it through the existing `RecordBuild` call, then compose the matching Job, satisfies **AC-4**, and completes **AC-1**, **AC-2**.
7. Add `testdata/sample-dockerfile` (done) and prove the thin thread on the real cluster: it deploys through BuildKit, `testdata/sample-go` still deploys through Buildpacks, satisfies **AC-17**.
8. [x] Project `build_path` onto the `deployment_status` payload, satisfies **AC-5**.
9. Close the refusals: `build_failed`'s message no longer names one engine and `deploy_app`'s description carries the rule, both done. What is left is the live half: prove `image_runs_as_root` with a rootful `Dockerfile` and confirm build output still stays in the pod, satisfies **AC-12**, **AC-13**, **AC-14**.
10. [x] Pin the composed security contexts with a test now that the namespace no longer enforces them: the Buildpacks pod meets every `restricted` field, the BuildKit pod deviates in exactly the four named ones, and its added capability list is exactly `SETUID` and `SETGID`, satisfies **AC-11**, **AC-10a**.
11. [x] Update `deploy_app`'s tool description to state the detection rule, in the same commit as the behaviour it describes, satisfies **AC-16**.

## Migration plan

**Strategy**: feature flagged by configuration order, not by a flag. The namespace relaxation and the code that needs it land in separate deployments, in that order.

**Phases**:

1. The `deployer-builds` pod security label moves to `baseline` through ArgoCD. Nothing behaves differently: the Buildpacks pod composes the same fields it always did, and `baseline` permits every one of them.
2. The control plane image carrying detection and the BuildKit Job ships. The first Dockerfile build then finds a namespace that already admits it.

**Rollback**: revert the control plane image, and every build returns to Buildpacks because detection is gone. The namespace may be left at `baseline` or moved back to `restricted` independently, since the Buildpacks pod satisfies both.

**Risks**:

- Shipping in the other order means the first Dockerfile build is rejected by admission, and the deployment fails as `internal` with nothing useful in the reason. Phase order is the whole mitigation.
- Rootless BuildKit under `baseline` with the native snapshotter was the assumption this rested on, and build step 3 has now proved it on this cluster: the pod starts and a real Dockerfile build completes. What it also proved is that the deviation is four fields, not two, which is recorded in [rationale.md](rationale.md) and priced into AC-10.
- The risk that remains is drift in the other direction. Four named deviations are easy to grow into five, because the next thing that fails inside a rootless builder will also have a field that makes it work. AC-11 exists to make that a failing test rather than a quiet commit, and any fifth field is a spec update, exactly as the fourth one was.
- Repinning the BuildKit image can change what its startup needs. The `TMPDIR` and home directory handling here is tied to how this digest lays out its own tree, so a bump is a re run of build step 3, not just a digest edit.

## Consequences

**Positive**:

- Apps Buildpacks cannot detect now deploy, which is the whole point of the slice.
- `image_runs_as_root` and spec 0004's AC-10 stop being unreachable code, and spec 0005's deferred failure projection can finally be proved.
- `build_path` becomes real data on every deployment, including failed ones, so "which engine ran" is never a guess.
- Spec 0003's open follow up about `deployer-builds` pod security is answered with the build path actually in front of you, which is exactly how it asked to be decided.

**Negative / tradeoffs**:

- The build namespace loses enforced `restricted`. The composer is now the only thing keeping the Buildpacks pod at that shape, which is why a test has to pin it, and a test is a weaker guarantee than an admission controller.
- The Dockerfile build pod can change its own uid and gid, and can run a setuid binary that raises into that same pair. That is a real widening of the one place the platform runs code it did not write. It is bounded by the capability bounding set rather than by the escalation flag, it does not reach the node, and it is the price of building a Dockerfile without a privileged pod or a shared daemon. Weigh it against Option 4's shared builder, which is faster and gives up more.
- Two build engines means two Job shapes, two pinned images, two uid pairs, and two failure output formats to keep sanitized. Spec 0001 named this cost when it chose to have a Dockerfile path at all.
- The native snapshotter copies layers instead of overlaying them, so a Dockerfile build is slower and heavier on disk than it needs to be. That is the price of not mounting `/dev/fuse`.
- No layer cache means every Dockerfile build is cold, every time. For an agent iterating on one app this is the most noticeable regression against a local `docker build`.
- Detection reads the archive on the control plane, so the control plane now opens caller supplied bytes it previously only stored. It reads headers only, writes nothing, and stops at the extractor's own entry limit, but it is a new place untrusted input is parsed, and it is parsed in the process that holds the database.

**Neutral**:

- No migration and no schema change: the column and the store field were both put there by spec 0002 for this feature.
- The build namespace network policy needs no change, because it already permits public egress for dependency downloads, which covers base image pulls.
- Detection is deterministic and has no override, so an agent that wants Buildpacks removes its `Dockerfile`. That is worth one line in the tool description and nothing more.

## Follow-up

- [ ] If a fifth deviation is ever needed, split the namespaces instead of adding it: `deployer-builds` back to enforced `restricted` for Paketo, a second `baseline` namespace for BuildKit alone. The cost is priced in [rationale.md](rationale.md) and it is the agreed fallback, so it should not be re deliberated from scratch.
- [ ] Registry backed layer cache for the Dockerfile path, keyed per app. Deliberately out of this slice: it would put unbounded cache images on a registry that already has no garbage collection, which is an existing deferred scope item. Worth revisiting together with that one.
- [ ] Build arguments and build secrets, once slice 7 gives an app a way to declare configuration. The answer today is none, and the question should be reopened deliberately rather than by whoever builds slice 7 first.
- [ ] Consider whether `fuse-overlayfs` is worth the extra device mount once you can measure how slow the native snapshotter actually is on this cluster.
- [ ] Base image allowlisting for Dockerfile builds sits naturally beside the deferred image and dependency scanning item, not on its own.
- [ ] Spec 0003's follow up ("decide `deployer-builds` pod security in slice 1") is answered here and can be marked closed against this spec.
