# 0001. Stack and architecture: rationale

The decision itself is in [index.md](index.md). This file holds the reasoning, the options weighed, and the evidence.

## Context

> ⚠️ Premise note: this platform's entire job is to take code an AI wrote, build it, and run it inside the cluster that also runs your real services. The scope defers workload isolation to slice 5, but slice 1 already runs untrusted code. Between those two points every deployed app is a normal pod with full default capabilities on a cluster where Cilium can see everything. The right framing is that the isolation defaults (non root, dropped capabilities, resource ceilings, a deny by default NetworkPolicy) are part of the first deploy path, not a later hardening pass. Slice 5 should be about proving and tightening them, not introducing them. The stack chosen here supports that: the platform composes every manifest in Go, so the defaults live in one function from the first deploy onward. A Follow-up records this so you can decide it deliberately.

Deployer has no code yet. Everything is open, which means the risk is not picking the wrong tool but picking too many. The forces that actually shaped this:

**The cluster is already opinionated.** Four k3s nodes running Cilium, Longhorn, ArgoCD, sealed-secrets, cert-manager, MetalLB, ingress-nginx, and a Tailscale operator. Every one of those is a decision already made and already operated. Anything this platform introduces is a new thing to run, patch, and be paged about, on top of twelve Argo applications that are currently healthy. The bar for a new component is therefore high, and the bar for reusing an existing one is nearly zero.

**There is one operator, and it is you.** No platform team, no on call rotation, no CI culture to inherit. A design that needs a message broker, a build operator, a database operator, and three services to make a single deploy work is not a design, it is a second job. The three time horizons all point the same way here: day 1 it has to be buildable alone, day 180 it has to be debuggable from one log stream, and day 730 the thing most likely to have happened is that you have not touched it for months and need to remember how it works.

**The workload is small and bursty.** One person deploying throwaway apps an AI wrote. Concurrent builds will be in single digits, probably usually one. Total apps will be tens, not thousands. Every piece of infrastructure that exists to handle throughput is infrastructure with no measured need behind it.

**The platform holds cluster credentials.** It is the one workload that can create Deployments in arbitrary namespaces. That makes its own attack surface, its image size, its dependency count, and its ability to keep user input out of a pod spec into security properties rather than preferences.

**Not deciding costs the whole project.** Every remaining feature in the scope sits on this. The data model, the cluster foundation, the tracer bullet, and all nine slices assume a language, a persistence story, and a process split. There is no way to start without settling it.

## Options considered

### Option 1: A single Go binary using the cluster as its scheduler

One Go program serving MCP and HTTP, holding state in SQLite on a Longhorn volume, running builds as Kubernetes Jobs it creates and watches, deployed as a single replica through the ArgoCD you already run.

**Pros**:
- Exactly one new long running component, plus a registry. Everything else is a Kubernetes object with a lifetime measured in minutes.
- Builds get scheduling, resource limits, isolation, and cleanup from the cluster rather than from code you write.
- `client-go` is the native Kubernetes client, so the most security sensitive code in the project (composing pod specs) is typed Go structs rather than templated YAML.
- The static binary on a distroless base is a small target for the one workload holding cluster rights.
- A restart is safe at any point, because deployment state is on disk and non terminal rows are re examined on start.

**Cons**:
- Genuinely cannot scale horizontally. SQLite and a single reconcile loop both assume one replica, and the `Recreate` strategy makes every platform deploy a short outage.
- The platform volume is a single point of data loss, and backup is deferred.
- Go's MCP ecosystem is younger than TypeScript's, so more of the protocol surface is hand written.
- Two build engines (the Buildpacks lifecycle and BuildKit) means two Job templates and two failure output formats to sanitize.

### Option 2: TypeScript control plane with the same cluster native shape

Same topology, but Node and TypeScript instead of Go, using the mature MCP TypeScript SDK and the JavaScript Kubernetes client.

**Pros**:
- The most mature MCP SDK by a wide margin, with the most examples, the most middleware, and the fewest protocol surprises.
- Fast to write, and the MCP surface is the part of this project a person actually iterates on.
- Very large ecosystem for everything adjacent (tarball handling, HTTP, validation).

**Cons**:
- The Kubernetes client is a generated wrapper rather than the canonical one, and watches and informers are noticeably rougher than `client-go`.
- Ships a Node runtime and a `node_modules` tree in the image of the workload that holds cluster credentials, which is a materially larger attack surface and dependency audit burden than a single static binary.
- Every tool this platform orchestrates (the lifecycle, BuildKit, ArgoCD, cert-manager, k3s itself) is Go, so reading their source or matching their behaviour means leaving your own language constantly.
- Long lived process plus watch reconnection logic is a place Node's error handling tends to leak.

### Option 3: Three services with a message broker and a build operator

Control plane, build worker, and deploy worker as separate deployments with NATS or Redis between them, kpack owning builds through CRDs, and Postgres via an operator for state.

**Pros**:
- Each part scales and fails independently, and a build storm cannot affect API latency at all.
- kpack gives automatic rebuilds when a base image is patched, which is real security value over time.
- Postgres gives concurrent writers, real backup tooling, and headroom for the deferred per app database feature.
- The shape most teams would recognise, and the one that survives growth without a rewrite.

**Cons**:
- Four new operational components (broker, kpack, Postgres operator, plus the extra services) before a single app deploys. For one operator this is the design most likely to be abandoned half built.
- kpack prefers Git or blob sources, which fights the tarball upload the scope already settled.
- Distributed state across a broker and a database makes "what happened to my deploy" a multi system question rather than one SQL query.
- Every one of these components is justified by throughput or team boundaries, and this project has neither.

### Option 4: Buy instead of build, run an existing PaaS in the cluster

Deploy an existing self hosted platform (a Heroku like PaaS, or a Kubernetes application platform) and write only a thin MCP shim in front of its API.

**Pros**:
- The hard parts (build, release, routing, rollback) are already solved and battle tested by other people.
- Dramatically less code to write and own.
- Most of them already do Buildpacks and Dockerfile builds well.

**Cons**:
- The isolation guarantees in the scope (own namespace, dropped capabilities, deny by default network policy, no privileged fields reachable by the caller) become properties of somebody else's product, configured rather than enforced by you. For running AI written code that is the exact property you least want to delegate.
- Most such platforms are far larger than this cluster's spare capacity and bring their own database, queue, and control loop anyway.
- The MCP shim ends up translating between two models, and every feature in the scope becomes a question of whether the underlying platform exposes it.
- Worth revisiting only if this project stalls: it is the honest fallback, not the starting point.

## Rationale

Option 1 wins on the forces from Context, not on taste. The cluster is already opinionated and already operated by one person, so the decisive question for every layer was "does this add something to run?" Kubernetes Jobs, SQLite, and the database as a queue all answer no. A broker, a build operator, and a database operator all answer yes, three times over, before the first deploy works (basis: your cluster inventory in `docs/scope/scope.md`, twelve Argo applications already operated).

Go over TypeScript came down to what the risky code is. The MCP surface is the part that is easier in TypeScript, and it is also the part that is small, well documented, and rarely changes once written. The part that is large, security sensitive, and constantly touched is manifest composition and cluster interaction, and there `client-go` is the canonical client rather than a wrapper (basis: `client-go` is the reference Kubernetes client). The image argument reinforces it: the workload that can create Deployments in any namespace should be a single static binary on a distroless base, not a Node runtime with a dependency tree (basis: minimise the attack surface of the most privileged component).

Buildpacks in a plain Job rather than kpack is the same reasoning applied once more. kpack's real value is automatic rebuilds on base image patches, which matters at a fleet of long lived apps and matters very little for throwaway apps an agent generated this afternoon. Against that it costs an operator, CRDs, and a source model that fights the tarball the scope already settled (basis: `docs/scope/scope.md`, source arrives as a tarball, no Git server). BuildKit rather than kaniko for the Dockerfile path is not a preference at all: kaniko was archived in June 2025 and the surviving fork takes security patches only, which is not a foundation for a new project.

The single replica ceiling is the honest cost, and it is worth naming plainly rather than burying. SQLite plus one in process loop means no horizontal scale and a brief outage on every platform deploy. For one person deploying tens of apps that ceiling is nowhere near, and the migration path if it ever is (Postgres plus row claiming with locks) is a schema port on a codebase whose SQL is already explicit thanks to `sqlc`, not a rewrite. Paying for concurrency you do not have, in operational complexity you would carry every day, is the trade this design refuses (basis: premature optimisation, add infrastructure to fix a measured bottleneck).

Option 4 deserves a real answer rather than dismissal. If the goal were only "deploy apps easily", an existing PaaS would beat writing this. The goal is narrower and stranger: run code an AI wrote, under isolation you can state and enforce, reachable by an agent over MCP. The isolation is the product, and the scope already made it non negotiable. Delegating it to a platform whose defaults you configure rather than compose is the one place this design will not save effort.

## Evidence: current landscape check (2026-08-11)

Verified during the design conversation, one pass, official repositories and registries first.

| Thing | Current state |
|---|---|
| Official Go MCP SDK | `modelcontextprotocol/go-sdk` v1.7.0 and up, active, stdio and streamable HTTP transports, 2026-07-28 protocol revision |
| kaniko | Archived June 2025 by GoogleContainerTools; `chainguard-dev/kaniko` fork takes security patches only. Not a foundation for new work |
| BuildKit | `moby/buildkit`, active, rootless and daemon modes both supported |
| Cloud Native Buildpacks | CNCF graduated, active, lifecycle runs as a container in a Kubernetes Job |
| kpack | `buildpacks-community/kpack`, active, runs unprivileged, CRD driven |
| distribution/registry | v3.x, active, the registry Docker Hub and GHCR are built on |
| zot | active, OCI native, CNCF sandbox, the credible lighter alternative |
| Harbor | v2.10 and up, active, full featured, needs Postgres and Redis |
| `modernc.org/sqlite` | active, pure Go, no cgo, the right choice for a static cross compiled binary |
| `mattn/go-sqlite3` | active but cgo required, which breaks cross compilation |
| goose | v3.x, active, supports the pure Go SQLite driver directly |
| `k8s.io/client-go` | v0.35.0, matching Kubernetes 1.35, which matches your k3s v1.35.5 |
| `sigs.k8s.io/controller-runtime` | v0.24.x, active, the right answer once there are CRDs and a real operator, which there are not |

## References

**Project sources** (verifiable, in this repo):
- `docs/scope/scope.md`, the settled MVP decisions (tarball source, Buildpacks with Dockerfile fallback, platform issued tokens, stateless apps, enforced isolation)
- `docs/scope/scope.md`, the live cluster inventory verified 2026-08-11 (k3s v1.35.5, Cilium 1.16.5, Longhorn 1.11.1, ArgoCD v3.3.6, sealed-secrets 0.36.6, ingress-nginx 1.11.3, cert-manager v1.16.2, MetalLB 0.14.9)
- Installed community skills: `senior-kubernetes-engineer`, `golang-patterns`, `golang-testing`, `mcp-server-patterns`, `mcp-builder`, `database-migrations`, `docker-patterns`, `security-patterns`
- The project's build approach, Tracer Bullet, recorded in the scope header

**Practices and standards**:
- Monolith first: extract a service only when a specific bottleneck or ownership boundary forces it
- A database backed queue before a broker: add a broker to fix a measured throughput problem
- Deploy by image digest, never by mutable tag, so a release is exactly reproducible
- Minimise the attack surface of the most privileged component
- Never build a security boundary by string templating user input into a manifest
- Premature optimisation: profile first, then add infrastructure

**Links** (web verified during the landscape check on 2026-08-11):
- Official Go MCP SDK: https://github.com/modelcontextprotocol/go-sdk
- Cloud Native Buildpacks: https://buildpacks.io
- kpack: https://github.com/buildpacks-community/kpack
- BuildKit: https://github.com/moby/buildkit
- kaniko (archived): https://github.com/GoogleContainerTools/kaniko
- distribution/registry: https://github.com/distribution/distribution
- zot: https://github.com/project-zot/zot
- Harbor: https://github.com/goharbor/harbor
- modernc.org/sqlite: https://pkg.go.dev/modernc.org/sqlite
- goose: https://github.com/pressly/goose
- atlas: https://atlasgo.io
- client-go: https://github.com/kubernetes/client-go
- controller-runtime: https://github.com/kubernetes-sigs/controller-runtime
