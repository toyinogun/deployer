# Deployer

A small internal platform that lets an AI coding agent deploy an app it just wrote onto a homelab k3s cluster, over MCP. One Go binary running as a single pod inside the cluster.

## Stack

- **Language / Runtime**: Go 1.26, one module, `cmd/deployer` is the only binary
- **Framework**: none. `net/http` with the standard mux, `log/slog` for JSON logs
- **Key dependencies**: `k8s.io/client-go` v0.35 · `modernc.org/sqlite` (pure Go, no cgo) · `sqlc` generated queries · `goose` migrations embedded with `go:embed`
- **Package manager**: `go` modules. Image built with `ko`, deployed by Kustomize plus ArgoCD

Full detail, including the build handoff contract and the load bearing invariants, is in [docs/specs/0001-stack-and-architecture/index.md](docs/specs/0001-stack-and-architecture/index.md). Read it before touching build, deploy, or reconcile code.

## Build approach

Tracer Bullet: prove the whole pipe works end to end before building any part of it fully.

## Commands

```bash
# Install deps
go mod download

# Run locally
go run ./cmd/deployer

# Build
go build ./...

# Test
go test -race ./...

# Regenerate the sqlc query code after editing internal/store/queries or the migration
sqlc generate

# Format, vet, lint (all three gate a commit)
gofmt -l . && go vet ./... && golangci-lint run

# Once per clone: turn on the pre commit hook
git config core.hooksPath .githooks

# Once per machine, if golangci-lint is missing
brew install golangci-lint   # or: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

## Specs

Stored in `docs/specs/`. Format: `docs/specs/NNNN-title/index.md`.

## Rules

- Layers: domain and use case logic (slug derivation, state transitions, manifest construction) never imports `client-go`, `net/http`, or the store. Those live at the edges and are reached through interfaces the inner code defines.
- A use case orchestrates, it does not hold business rules. Rules live with the type that owns the invariant.
- Every workload manifest is composed field by field in Go. No string templating, and no user supplied value is ever merged into a pod spec.
- Every deployment state transition is a database write before it is an action.
- A deployment something else already ended is not a fault. A drive that finds its row terminal stops quietly rather than writing over it, and a refusal a caller sees is a real access decision, never a masked internal error.
- Deploy by image digest, never by a mutable tag.
- A release records what actually ran. The configuration a deploy composed the app's Secret and pod checksum from is the same map handed to `MarkHealthy`, because the readiness wait is long enough for a `set_config` to land in the middle of it.
- A deploy is healthy only when the new pods are serving. A rolling update keeps the previous pods available while the new ones start, so `AvailableReplicas` on its own is answered by the old ReplicaSet and any wait built on it returns before anything new runs: readiness compares the way `kubectl rollout status` does, in `internal/kube`. This is what decides whether a failed deploy is recorded as one, so it cannot be loosened for speed.
- A quota tracked object is read before it is written. Kubernetes charges a ResourceQuota when a create reaches admission and does not refund it when that create fails with `AlreadyExists`, so creating a Secret or Service that already exists walks an app's namespace quota upward until the quota controller recalculates. Rollbacks are cheap enough to run several in a row, which is how three of them took an app to its ceiling of three services.
- The per account app cap holds because `deploy_app` is the only path that creates an app row, and nothing enforces that property. A second create path, such as a write action from the browser surface, bypasses the cap unless it goes through the same store call: the count and the insert live in one transaction in `internal/store`, so any new caller reaches the cap by using `CreateApp` rather than by repeating the check.
- An app's egress bound lives in two places at once. A NetworkPolicy can only permit, never refuse, so the blocked port list is composed as its complement over 1..65535 in `internal/deploy`, and the public egress rule stays exactly one peer because a `ports` list applies to every peer under the same `to`. Every bound the allow policy carries has to be passed by the policy sweep in `internal/reconcile` as well as by the deploy path, or an existing app namespace is swept back to a weaker policy than a deploy would compose.
- A suspension is asymmetric in both halves, and the asymmetry is what holds it. The lockout is a database write and it lands first, so a cluster outage can never leave a suspended person still signed in. The sweep in `internal/suspend` only ever scales down, which is what stops a bug in it putting a suspended account back on the network, so a restore is the single thing that ever scales an app up: a restore that ends early leaves the apps at zero with nothing behind it to retry, which is why a failed app list read carries `ErrAppsUnlisted` rather than reading to an admin as a plain failure. A vanished app namespace answers `Forbidden` rather than `NotFound`, because the platform's RBAC is granted per app namespace and the fake clientset answers `NotFound`, so `internal/kube` confirms the namespace is really gone before treating a refusal as a stopped app.
- Anything running inside the cluster reaches the platform on `DEPLOYER_INTERNAL_URL`. `DEPLOYER_PUBLIC_URL` is only for text handed to a caller, because a build pod resolves names on cluster DNS, which cannot see the public hostname.
- A build hands work between three different users: the build pod runs as the builder image's declared `CNB_USER_ID`, and the tree it unpacks is carried into the app image, where the run image's user is a different uid again. Read those uids from the pinned images, never assume them, and leave the unpacked tree readable by a user that does not own it. The build pod's pair is configuration (`DEPLOYER_BUILD_UID`, `DEPLOYER_BUILD_GID`) rather than a Go constant, and CI's `builder uid` step reads it off the pinned builder and fails on drift; reading it once by hand and committing the literal is how this broke before.
- Errors wrap with `fmt.Errorf("...: %w", err)`; sentinel errors for the cases callers branch on; never swallow one.
- A failure a caller sees is one of the closed reason codes in `internal/domain/reason.go`, never a wrapped error string. The same code goes into `deployments.failure_reason`. Build output stays in the Job's pod logs: it never reaches the response, the database, or the platform log at info level.
- An app's own output is never stored. `get_logs` reads it from Kubernetes at the moment of the call, redacts, bounds, and hands it back: no table, no log store, nothing written. The bounds in `internal/logs` are constants, not `DEPLOYER_*` configuration, because they are product decisions about what fits an agent's context window.
- An MCP tool's description is part of the contract, not decoration. `deploy_app`'s carries the upload endpoint and the rules an app must meet, so a change to either is a change to the description in the same commit. Nothing tests that drift.
- Every `DEPLOYER_*` variable is validated in `internal/config` at startup, never at first use.
- Every exported type and function carries a doc comment starting with its own name.
- Tests: pure logic is written test first. Kubernetes and HTTP wiring is tested after, with the `client-go` fake clientset and a real SQLite file in a temp dir. No mocking of the store.
- What that last rule bans is invented store behaviour, not a passthrough. A test type that embeds the real store, delegates every call to it, and returns a real error on one named call fakes no semantics at all, so a test cannot pass against store behaviour that does not exist. It is also the only way to reach a fault internal to the platform, such as a state write that does not land, and `internal/reconcile/stranded_test.go` is the case that established the distinction.
- The fake clientset resolves no names, execs nothing, and the test process owns every file it writes, so a DNS address, a user switch, or a file mode taken from the wrong source passes the whole suite. Those belong to `/check verify` against the real cluster, and each one that bites is worth a unit test pinning the value afterwards.
- A test calling an MCP handler method directly never crosses the tool's argument schema, so a schema that refuses a call before the handler runs hands the caller a validation string instead of a reason code and passes the suite. Anything the closed reason codes promise needs a test through a real client and server session: `internal/mcp/wire_test.go` holds that path, as the `callOverTheWire` helper each feature's own test file calls, so the cases live beside the feature rather than all in that one file.
- Commits follow conventional format (`feat:`, `fix:`, `chore:`).

## Tooling

- Lint and format: `gofmt`, `go vet`, `golangci-lint` ([.golangci.yml](.golangci.yml): the standard linter set plus `errorlint`, which enforces the `%w` wrapping rule above).
- Pre commit hook: [.githooks/pre-commit](.githooks/pre-commit), running format, vet, lint, and build on staged Go files. Tests are not in the hook.
- CI: [.github/workflows/ci.yml](.github/workflows/ci.yml), on push to `main` and on every pull request: gofmt check, `go vet`, `golangci-lint`, `go test -race`, the `builder uid` drift check, then `ko build`.
- The golangci-lint version is pinned in two places, the CI workflow (`v2.12`) and the hook's install hint. They can drift; keep them together when you bump one.

## Git

- integration: on
- branch prefix: `feat/`
- commit: per-milestone

## Agent skills

- [senior-kubernetes-engineer](~/.claude/skills/senior-kubernetes-engineer/): production Kubernetes, manifests, ArgoCD and GitOps, cluster troubleshooting.
- [golang-patterns](~/.claude/skills/golang-patterns/): idiomatic Go structure, error handling, concurrency.
- [golang-testing](~/.claude/skills/golang-testing/): table tests, fakes, coverage practice for Go.
- [mcp-server-patterns](~/.claude/skills/mcp-server-patterns/): building an MCP server, tool shape and transport.
- [mcp-builder](.claude/skills/mcp-builder/): `anthropics/skills`, scaffolding and reviewing MCP tool definitions.
- [database-migrations](~/.claude/skills/database-migrations/): migration ordering, reversibility, running them safely at startup.
- [docker-patterns](~/.claude/skills/docker-patterns/): image layering and build practice, relevant to the BuildKit path.
- [security-patterns](~/.claude/skills/security-patterns/): token handling, secret storage, least privilege.

Declined: argocd-gitops, sqlite-database-expert, kubernetes-specialist (registry search found nothing covering sqlc, goose, ko, Buildpacks, or client-go that the skills above do not already cover). Also declined for spec 0020's two libraries, `filippo.io/age` and `minio-go`: the age candidates cover file encryption by hand, and the S3 and R2 ones cover bucket administration and the AWS SDK, neither of which is the Go client this code uses.

## Context files

<!-- Nested AGENTS.md files are listed here as they are created -->

- [internal/store/AGENTS.md](internal/store/AGENTS.md): the SQLite data layer, the sqlc generate loop, the migration, and the sentinel errors callers branch on.
- [deploy/AGENTS.md](deploy/AGENTS.md): the cluster manifests, the CI written image digest, the RBAC and admission policy pairing, and the ArgoCD scope.
- [internal/config/AGENTS.md](internal/config/AGENTS.md): the startup validation of every `DEPLOYER_*` value, and the parse tests that pin the static policy YAML no Go code composes.
- [internal/web/AGENTS.md](internal/web/AGENTS.md): the browser surface, the two CSRF mechanisms, the template sets, and what a page refuses to render.
- [internal/backup/AGENTS.md](internal/backup/AGENTS.md): the platform's own backup and restore, the nil service that means backups are off, and why nothing in the cluster can decrypt one.

_Drafted by /audit from the repo, worth a quick human pass. Edit freely: once a line stops matching this draft, later runs treat it as curated and will flag rather than overwrite it._
