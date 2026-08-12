# 0004. First deploy end to end: rationale

Reasoning, options, and the premise note behind [index.md](index.md).

## Context

> ⚠️ Premise note: a blocking MCP tool call is not a safe place to hold a cold container build. A first Buildpacks build with no layer cache runs minutes, and every MCP client enforces its own tool timeout well under the 10 minute budget chosen here. The failure mode is not a broken deploy, it is a client that gives up while the platform is still working, leaving the agent with no result and no way to ask for one, because the status tool is slice 2. This is accepted rather than designed away, for two reasons: the deployment record is on disk and the deploy completes regardless of who is still listening, and building the status tool now is building slice 2. The mitigations are concrete: the tool description states plainly that the call runs for minutes, the response and the failure reasons are short, and a client side timeout leaves a row an operator can read directly. Slice 2 is the real fix and should not slip.

Everything under this feature already exists on paper and nothing is joined up. Spec 0001 chose the stack and drew the deploy path. Spec 0002 built the whole database, including the seven state deployment machine, supersession, and release creation. Spec 0003 put the control plane in the cluster with a scoped service account, gave app namespaces restricted pod security and quotas, and proved one hand applied hello world reachable over HTTPS on the wildcard domain. What has never happened is a single request travelling the whole path: an agent authenticating, source arriving, an image being built and pushed, a workload being composed, and a hostname coming back.

The forces are narrow and mostly already fixed. There is one operator, one caller, and no scale requirement worth naming. The cluster is a homelab, so nothing here can demand an operator function. Three of the four load bearing pieces are decided elsewhere and must not be reopened: the schema, the namespace and RBAC layout, and the request path into the cluster. What is genuinely undecided is how a build actually runs, how the built image gets from the registry into a composed workload without any user value touching a pod spec, and what the agent facing surface looks like when there is no way yet to ask how a deploy is going.

The cost of not deciding is that the four foundation features stay disconnected. Everything built so far is infrastructure with nothing running on it, and every later slice thickens a segment of a thread that does not exist.

## Options considered

### Option 1: Reconcile loop with Kubernetes Job builds, and a blocking handler that waits on state

The MCP handler writes a `queued` deployment row and then waits for it to reach a terminal state. A single in process reconcile loop claims the row, creates a build Job, reads the pushed digest back from the pod status, composes the app workload, waits for readiness, and marks the deployment healthy. The handler is a thin observer over machinery that would run identically with nobody watching.

**Pros**:
- Matches spec 0001's architecture and spec 0002's invariant that every transition is a database write before it is an action, so nothing has to be unwound later.
- A control plane restart mid build is survivable: the row is on disk, the sweep either resumes or fails it with a reason.
- Slice 2 becomes an addition (stop waiting, add a status tool), not a rewrite of the deploy path.
- A client timeout does not abandon the work; the deploy finishes and the record is readable.

**Cons**:
- More code than slice 1 strictly needs: a claim, a tick, a sweep, and a wait channel, all to serve one caller who is standing there watching.
- The waiting handler and the loop are two things reasoning about the same row, so the wait must be a plain poll of committed state rather than an in memory handoff, or the two can disagree after a restart.

### Option 2: Run the whole deploy inline in the request handler

The handler does everything itself in sequence: create the Job, watch it, compose the workload, wait for readiness, return. No loop, no claim, no sweep.

**Pros**:
- The least code by a wide margin, and the control flow reads top to bottom.
- Nothing to reason about concerning concurrency, because there is exactly one path.

**Cons**:
- A control plane restart abandons every in flight deploy, leaving rows non terminal forever with live Kubernetes objects behind them, which is exactly the state spec 0001 forbids.
- Slice 2 then rewrites the deploy path rather than extending it, and the schema built in spec 0002 sits unused in the meantime.

### Option 3: Return a deployment id immediately and add the status tool now

Pull slice 2's async shape forward. `deploy_app` returns a deployment id in under a second, and a second tool reports state.

**Pros**:
- The architecturally honest end state, reached sooner, with no blocking call to apologise for.
- Immune to the client timeout problem in the premise note.

**Cons**:
- It is slice 2. Building it here means the tracer bullet carries a status surface, its state to reason readback shape, and its failure reporting before the thread it reports on has ever run once.
- The agent facing loop (call, poll, interpret) is a design decision in its own right and deserves its own spec rather than being smuggled into this one.

### Option 4: Delegate builds to a build operator such as kpack

Install a Buildpacks operator, create an `Image` custom resource per app, and let the operator own build scheduling, rebasing, and caching.

**Pros**:
- Layer caching, automatic base image rebasing, and build scheduling come free and are genuinely well engineered.
- Less build orchestration code in the platform.

**Cons**:
- A new operator, its CRDs, and its controller become permanent cluster infrastructure and a permanent upgrade obligation, which spec 0001 explicitly rejected in favour of the cluster's own primitives.
- The Job spec stops being the platform's, so the isolation guarantees slice 5 depends on move into a third party's reconciler.
- It is a large dependency taken on before a single build has ever run.

## Rationale

Option 1 wins on the restart force, which is the one that actually differentiates the options here. Spec 0002's schema is built entirely around the idea that state is committed before it is acted on, and spec 0001 makes "a deployment is never left non terminal without a live Kubernetes object behind it" a load bearing invariant. Option 2 cannot hold either of those, and the moment it cannot, the sweep, the claim, and the event log are all dead code carried for a slice that does not use them. Building the loop now costs a tick, a conditional claim already written and tested in `internal/store`, and a poll; it is a small amount of new code sitting on machinery that exists.

Option 3 is where this ends up, and saying so plainly is better than pretending the blocking call is the destination. It is deferred because the tracer bullet's job is to prove the pipe, and a status surface designed before the pipe has ever carried anything is a surface designed against guesses. The premise note is the honest cost of that deferral.

Option 4 fails spec 0001's operating test rather than a technical one. The platform's whole shape is "introduce nothing the cluster does not already run", and a build operator is a controller, a CRD set, and an upgrade path for a homelab with one operator. The cache it would buy matters when builds are frequent; here the first build is the only build that has ever happened.

One thing changed under cross check, and it is worth recording rather than quietly editing. Spec 0001's build handoff contract has the build container write its digest to `terminationMessagePath`. The Buildpacks lifecycle does not do that: `creator` writes a `report.toml` inside the build, so honouring that contract would mean a wrapper entrypoint, a TOML parser, a custom image to hold both, and care about the 4096 byte truncation Kubernetes applies to termination messages. All of it to learn a digest the platform can simply ask the registry for, using the tag it chose itself, on a call it already has to make for the non root check. The channel was designed before anyone knew what the lifecycle actually emits. Resolving from the registry deletes a component rather than adding one.

The smaller calls follow the same instinct. The `lifecycle -creator` binary in one container rather than the phased lifecycle, because a cache is the only thing the phases buy and there is nothing to cache yet. The deployer image as the init container rather than a shell, because unpacking an untrusted tarball is exactly the code you want written deliberately in Go and unit tested, not delegated to `tar` in a namespace that also executes whatever an AI wrote. Inspecting the pushed image for a non root user before composing a Deployment, because spec 0003 guarantees admission will refuse it and an agent deserves a cause rather than a translated API server string. One registry credential rather than a token auth service, which is the weakest of these calls and is made with open eyes. The pull secret in an app namespace is genuinely safe, since the kubelet consumes it and the pod cannot read it. The build container is not: it holds a write credential while running buildpack phases over source nobody vetted, so a malicious buildpack can push any tag in the registry. What keeps that from mattering here is that every app is deployed by digest, so an overwritten tag changes no running app, and that the registry holds nothing else. The proper close is a token service issuing a per build, per repository, push only credential, and it is a follow-up rather than a pretence that the boundary is already there.

The one place a stated preference was overridden by nothing is the timeout split. Per phase budgets, with `activeDeadlineSeconds` on the Job itself, exist so that Kubernetes kills a wedged build rather than the platform merely losing interest in it. A single overall timeout would leave the Job running after the caller gave up.
