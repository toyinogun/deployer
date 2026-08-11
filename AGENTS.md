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
go test ./...

# Format, vet, lint (all three gate a commit)
gofmt -l . && go vet ./... && golangci-lint run
```

## Specs

Stored in `docs/specs/`. Format: `docs/specs/NNNN-title/index.md`.

## Rules

- Layers: domain and use case logic (slug derivation, state transitions, manifest construction) never imports `client-go`, `net/http`, or the store. Those live at the edges and are reached through interfaces the inner code defines.
- A use case orchestrates, it does not hold business rules. Rules live with the type that owns the invariant.
- Every workload manifest is composed field by field in Go. No string templating, and no user supplied value is ever merged into a pod spec.
- Every deployment state transition is a database write before it is an action.
- Deploy by image digest, never by a mutable tag.
- Errors wrap with `fmt.Errorf("...: %w", err)`; sentinel errors for the cases callers branch on; never swallow one.
- Every `DEPLOYER_*` variable is validated in `internal/config` at startup, never at first use.
- Every exported type and function carries a doc comment starting with its own name.
- Tests: pure logic is written test first. Kubernetes and HTTP wiring is tested after, with the `client-go` fake clientset and a real SQLite file in a temp dir. No mocking of the store.
- Commits follow conventional format (`feat:`, `fix:`, `chore:`).

## Tooling

Chosen here, installed by `/develop tooling`:

- Lint and format: `gofmt`, `go vet`, `golangci-lint` (staticcheck and errcheck defaults, no strict custom set).
- Pre commit hook: format, vet, lint, and build on staged Go files. Tests are not in the hook.
- CI: one GitHub Actions workflow on push, running gofmt check, `go vet`, `golangci-lint`, `go test`, then `ko build`.

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

_Drafted by /audit from the repo, worth a quick human pass. Edit freely: once a line stops matching this draft, later runs treat it as curated and will flag rather than overwrite it._
