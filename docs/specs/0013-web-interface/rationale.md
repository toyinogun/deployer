# 0013. Web interface: rationale

The decision record behind [index.md](index.md). Not read during a build.

## Context

> ⚠️ Premise note: the scope row for this feature says "Feature 8 deliberately builds no pages, so every endpoint they need is already there and drivable with curl." That is true of identity and false of apps. `/v1/auth`, `/v1/tokens` and `/v1/admin` are session backed HTTP endpoints. Everything about an app, the list, the releases, the logs, the status, the configuration, exists only as an MCP tool behind a bearer token, and MCP tools are not reachable from a browser session. So this feature is not purely a rendering job on top of finished endpoints. It either adds a session backed read surface for apps or renders that data server side, and the choice between those two is the load bearing decision here.

> ⚠️ Premise note: `internal/store/config.go` states that `ListConfigForDeploy`, the only method returning secret configuration values, may be called by the deploy path and the release snapshot and never by a tool response. A browser control that reveals a secret value would be a third caller and would make the browser a weaker door than the agent surface, since a stolen session would read every credential the account holds while a stolen API token cannot. The reveal in this spec is therefore restricted to keys not flagged secret, which `ListConfigForResponse` already returns.

The platform is complete as a machine surface and has no human surface at all. A person registers by posting JSON, verifies by clicking a link that renders a JSON body, mints a token with curl, and learns what their agent deployed by asking the agent. The four admin endpoints built in spec 0007 have never been driven by anything.

Three forces shape the answer. The first is the artifact: `AGENTS.md` describes one Go binary in one pod, built by `ko`, deployed by Kustomize and ArgoCD, and CI is a Go pipeline with no node step. Anything that introduces a bundler changes the build, the image, and the CI contract for six pages. The second is the audience: this is a homelab platform for one person and whoever they share a tailnet with, holding a handful of apps, reached over Tailscale, sometimes from a phone. There is no scale problem to design against and no anonymous traffic to defend against. The third is the existing layering rule: the domain and use case layers never import `net/http`, the store, or `client-go`, and the edges reach them through interfaces. A page surface is another edge and must sit exactly where `internal/httpapi` sits.

Two things this feature must not do. It must not become a second, subtly different implementation of the ownership rules, because the moment the browser and the agent disagree about who may read what, one of them is a hole. And it must not weaken the leak boundary the platform has held so far: no raw credential in a response that is not a one time mint, no app output stored anywhere, no secret value readable through a response path.

## Options considered

### Option 1: server rendered Go templates in the existing binary, reading the store directly

Page handlers in a new `internal/web` package render `html/template` output, with templates, one stylesheet and one small script embedded via `go:embed`. Handlers call the same `internal/identity` service and `internal/store` methods the JSON and MCP surfaces call. Browser forms get their own page POST handlers beside the existing JSON ones, both thin wrappers over one service call.

**Pros**:
- No frontend toolchain, no node in CI, no bundle step before `ko`, no change to the image or the deployment.
- One authorisation path. A page cannot disagree with the MCP tool about ownership because it calls the same account scoped method.
- Server rendering is the strongest fit for pages that are read only, small, and behind a login: no client state to synchronise, first paint is the final paint.
- Works with JavaScript disabled for everything but live polling.

**Cons**:
- No component model. Shared page furniture is template partials and hand written CSS, which becomes real friction at several times this many pages.
- Anything genuinely interactive later, a live log tail, an inline editor, is awkward and will eventually argue for a real frontend.
- Page handlers are a second consumer of store methods that only the MCP path currently exercises through a wire level test.

### Option 2: session authed JSON endpoints under `/v1/apps`, consumed by pages or by any client

Build a real read API for apps: `GET /v1/apps`, `/v1/apps/{slug}/releases`, `/logs`, `/config`, session authenticated, and have the browser consume it.

**Pros**:
- A reusable surface. A script, a second client, or a future mobile view gets the same data for free.
- Clean separation between data and presentation, and each endpoint is independently testable.

**Cons**:
- Doubles the authorisation surface. Every endpoint needs its own ownership check, its own audit rows, and its own tests, and the MCP tool and the endpoint can drift.
- It is a public looking API built for exactly one consumer that lives in the same process and could have called the method directly.
- It does not remove the need to render pages; it adds a layer under them.

### Option 3: a React or Svelte single page app built by Vite, served as static assets

A real frontend project in the repo, built in CI, its output embedded or served from the pod, talking to the JSON surface from Option 2.

**Pros**:
- The best developer experience for building UI, a genuine component model, and the largest ecosystem for anything interactive later.
- Clean separation of concerns, and the UI could be developed and previewed without the cluster.

**Cons**:
- Adds node to a Go repository and a bundle step to a CI pipeline whose whole shape is Go, plus a second dependency tree to keep current and scan.
- Forces Option 2's API surface as a hard prerequisite, so it is strictly the largest of the three.
- Buys component ergonomics for six read only pages that will not change often, which is paying the cost at day 1 for a benefit that arrives at day 730 if ever.

## Rationale

Option 1 wins on the artifact constraint alone. `AGENTS.md` describes a project whose defining property is that it is one Go binary with no cgo, built by `ko`, and the entire operational story rests on that. Introducing node and a bundle step for six read only pages is the clearest possible case of a technically respectable choice that is wrong for its operational reality. Option 3 is what you would pick if the UI were the product; here the MCP surface is the product and the UI is a window onto it.

Between Options 1 and 2, the deciding force is the second thing this feature must not do. Ownership is the platform's load bearing security property, and spec 0007 spent twenty two acceptance criteria on it. Option 2 recreates every one of those checks in a second place, and the failure mode when they drift is silent: the browser shows an account something the MCP tool would have refused, and nothing fails a test because both surfaces pass their own. Option 1 makes that class of bug unrepresentable, because there is only one path to the data. The cost is the component model, and at this size a component model is a convenience, not a capability.

The read only choice for apps follows the same reasoning from the other side. Every mutating MCP tool already exists, works, and is exercised by the caller the platform was built for. Adding browser buttons for them means CSRF protected destructive paths, confirmation flows, and a second way to reach `delete_app`, in exchange for saving a person one sentence typed at their agent. Rollback is the one with a genuine case, since it is the thing you want at 2am, and it is recorded as a follow up rather than smuggled into this slice.

Two smaller calls worth recording. The CSRF token is derived as an HMAC of the session id rather than stored on the session row, because a derived token is valid exactly as long as its session and is revoked by the same act that revokes it, with no migration and no rotation logic; the cost is one new configuration variable, which the project already validates a dozen of. And motion is CSS only with cross document View Transitions, because a multi page server rendered app is precisely the case View Transitions was designed for: the premium feel comes from pages not flashing white between navigations, which is a handful of lines and no dependency, rather than from an animation library orchestrating things that do not move.

## The design source

The visual direction comes from two reference screenshots the engineer placed in the repository root, `design.webp` and `design2.webp`. Both are analytics dashboards for products Deployer is not, so the language was taken and the content was not: the shell, the sidebar in labelled groups, the card surfaces, the rounding and the single saturated accent survive; the stat tiles do not, because Deployer would have to invent numbers to fill them. The chosen blend is the `design2.webp` shell, its deep green accent and its inset rounded panel, with the tighter table row density of `design.webp`, because release and log tables are the densest thing on any page here and the heavier rounding fights them.

The engineer chose light theme only, responsive down to a phone, and accessibility at the semantic HTML and reduced motion baseline rather than a formal WCAG target, on the grounds that this is an internal tool for one or two people on a tailnet.
