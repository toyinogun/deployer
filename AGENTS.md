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
- Deploy by image digest, never by a mutable tag.
- Anything running inside the cluster reaches the platform on `DEPLOYER_INTERNAL_URL`. `DEPLOYER_PUBLIC_URL` is only for text handed to a caller, because a build pod resolves names on cluster DNS, which cannot see the public hostname.
- A build hands work between three different users: the build pod runs as the builder image's declared `CNB_USER_ID`, and the tree it unpacks is carried into the app image, where the run image's user is a different uid again. Read those uids from the pinned images, never assume them, and leave the unpacked tree readable by a user that does not own it.
- Errors wrap with `fmt.Errorf("...: %w", err)`; sentinel errors for the cases callers branch on; never swallow one.
- Every `DEPLOYER_*` variable is validated in `internal/config` at startup, never at first use.
- Every exported type and function carries a doc comment starting with its own name.
- Tests: pure logic is written test first. Kubernetes and HTTP wiring is tested after, with the `client-go` fake clientset and a real SQLite file in a temp dir. No mocking of the store.
- The fake clientset resolves no names, execs nothing, and the test process owns every file it writes, so a DNS address, a user switch, or a file mode taken from the wrong source passes the whole suite. Those belong to `/check verify` against the real cluster, and each one that bites is worth a unit test pinning the value afterwards.
- Commits follow conventional format (`feat:`, `fix:`, `chore:`).

## Tooling

- Lint and format: `gofmt`, `go vet`, `golangci-lint` ([.golangci.yml](.golangci.yml): the standard linter set plus `errorlint`, which enforces the `%w` wrapping rule above).
- Pre commit hook: [.githooks/pre-commit](.githooks/pre-commit), running format, vet, lint, and build on staged Go files. Tests are not in the hook.
- CI: [.github/workflows/ci.yml](.github/workflows/ci.yml), on push to `main` and on every pull request: gofmt check, `go vet`, `golangci-lint`, `go test -race`, then `ko build`.
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

Declined: argocd-gitops, sqlite-database-expert, kubernetes-specialist (registry search found nothing covering sqlc, goose, ko, Buildpacks, or client-go that the skills above do not already cover)

## Context files

<!-- Nested AGENTS.md files are listed here as they are created -->

- [internal/store/AGENTS.md](internal/store/AGENTS.md): the SQLite data layer, the sqlc generate loop, the migration, and the sentinel errors callers branch on.
- [deploy/AGENTS.md](deploy/AGENTS.md): the cluster manifests, the CI written image digest, the RBAC and admission policy pairing, and the ArgoCD scope.

_Drafted by /audit from the repo, worth a quick human pass. Edit freely: once a line stops matching this draft, later runs treat it as curated and will flag rather than overwrite it._
