# Deployer

A small internal platform that lets an AI coding agent deploy an app it just wrote onto a homelab k3s cluster, over MCP.

The agent uploads a source tarball, calls one tool, and gets back a working HTTPS hostname. Nobody touches kubectl, Docker, or YAML. One Go binary runs as a single pod inside the cluster and does the rest: it builds the source with Cloud Native Buildpacks in a Kubernetes Job, pushes the image to an in cluster registry, composes the app's Deployment, Service and Ingress field by field in Go, waits for the app to answer, and returns the URL.

This is a personal homelab project, not a product. It is early: read [CHANGELOG.md](CHANGELOG.md) for what actually works today, and [docs/scope/scope.md](docs/scope/scope.md) for what is planned.

## How it looks from the agent's side

Upload the source, then call the tool:

```bash
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @- < <(tar czf - .)
# → {"upload_id": "upl_...", "expires_at": "..."}
```

```
deploy_app(name: "my-app", upload_id: "upl_...")
# → https://my-app.<your app domain>, plus the deployment id, release number and image digest
```

The call blocks for the whole deploy, which can take minutes on a cold build. A status tool replaces the waiting in the next slice. Your app has to listen on the port given in `PORT` and run as a non root user; an image whose user is root is refused before anything is deployed.

## Stack

Go 1.26, one module, `cmd/deployer` is the only binary. No framework: `net/http` with the standard mux and `log/slog` for JSON logs. SQLite through `modernc.org/sqlite` (pure Go, no cgo), with `sqlc` generated queries and `goose` migrations embedded via `go:embed`. Kubernetes through `client-go`. The image is built with `ko` and delivered by Kustomize and ArgoCD.

The full picture, including the load bearing invariants, is in [docs/specs/0001-stack-and-architecture/index.md](docs/specs/0001-stack-and-architecture/index.md).

## Running it locally

```bash
go mod download
go run ./cmd/deployer
```

Every `DEPLOYER_*` setting is validated at startup, so a missing or malformed one fails the boot naming the variable. [internal/config](internal/config) is the list. Without `DEPLOYER_BOOTSTRAP_TOKEN` the platform boots with no usable token, which is fine for local work.

```bash
go build ./...
go test -race ./...
gofmt -l . && go vet ./... && golangci-lint run   # all three gate a commit
git config core.hooksPath .githooks               # once per clone
```

## Deploying it

The cluster manifests are in [deploy/](deploy/), delivered by ArgoCD. CI writes the control plane image digest into `deploy/deployment.yaml` on every push to `main`, so the running pod is always something CI built and pinned. See [deploy/AGENTS.md](deploy/AGENTS.md).

## Where things are

| Path | What lives there |
|---|---|
| `cmd/deployer` | the binary, its wiring, and the `fetch-source` subcommand the build Job runs |
| `internal/domain` | slug derivation, reason codes, image rules. No infrastructure imports |
| `internal/store` | the SQLite layer, the migration, and the sqlc loop ([notes](internal/store/AGENTS.md)) |
| `internal/build`, `internal/registry`, `internal/source` | the build Job, digest resolution and image checks, the hardened extractor |
| `internal/deploy`, `internal/reconcile` | workload composition, and the loop that drives a deployment to healthy |
| `internal/httpapi`, `internal/mcp`, `internal/auth` | the upload endpoints, the MCP tool, and bearer token auth |
| `docs/specs` | the decisions, one folder per spec |

Conventions for anyone (or anything) working in here are in [AGENTS.md](AGENTS.md).

## Security, stated plainly

Two limits are worth knowing before you point anything real at this:

- **Auth is one seeded token with no revocation.** Whoever holds it is the platform's only account. Real token minting is a later slice.
- **The registry's write credential is mounted into the build container**, beside code the platform did not author, because Buildpacks needs it to push. A hostile buildpack could push any tag in the registry. Running apps are unaffected, since every deploy is by digest, but the proper fix (a token service issuing per build, push only credentials) is not built yet.

The rest of the model, including what the build namespace is allowed to do, is in [docs/specs/0004-first-deploy-end-to-end/index.md](docs/specs/0004-first-deploy-end-to-end/index.md).
