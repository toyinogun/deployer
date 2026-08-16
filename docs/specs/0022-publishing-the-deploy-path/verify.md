# Verify: publishing the deploy path · spec 0022 · updated 2026-08-16

_Steps derived from spec 0022 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The unit suite already holds AC-2, AC-3, AC-4, AC-6, AC-7, AC-8, AC-9, AC-10,
AC-11, AC-12, AC-13, AC-14, AC-15, AC-16, AC-17, AC-18, AC-19, AC-22 and AC-23.
What is left below is the part the fake clientset and an in memory mux cannot
reach: real DNS, a real tunnel, a real agent with no Tailscale, and real timings.

## Commands

- [x] `go test -race ./...` → green, including `TestTheDeployHostAnswersOnlyTheDeployRoutes`, `TestTheConsoleHostCarriesNoDeployRoute` and `TestTheDeployHostGoesStraightToTheControlPlaneAboveTheWildcard` → AC-2, AC-3, AC-4, AC-6
- [x] `go test -run TestTheRemovedPublicURLFailsTheBoot ./internal/config/` → a configuration still setting `DEPLOYER_PUBLIC_URL` fails the boot naming it → AC-9
- [x] `kubectl -n deployer-system logs deploy/deployer | head` after the sync → the pod started, so `DEPLOYER_MCP_HOST` validated and no removed variable is still set → AC-8, AC-9
- [x] `kubectl -n deployer-edge get cm cloudflared -o yaml` → the deploy host rule is listed above `*.deploy.toyintest.org` and points at `http://deployer.deployer-system.svc.cluster.local:80` → AC-6
- [x] `kubectl -n deployer-system get networkpolicy -o yaml` → the peers on 8080 are the ones that were there before this change, with nothing added for the second hostname → AC-22

## Manual, from a machine with no Tailscale

- [x] `dig +short mcp.deploy.toyintest.org` → the tunnel's address, so the record exists → AC-1
      (2026-08-16: `104.21.72.176` / `172.67.153.85`, the same pair `console.deploy.toyintest.org`
      and an invented name both answer, so the name is covered by the wildcard record already
      pointing at the tunnel and needs no record of its own.)
- [x] `curl -sS -o /dev/null -w '%{http_code}\n' https://mcp.deploy.toyintest.org/` → `404` from the deploy host catch all, not an `ingress-nginx` page, which is what says the rule is not shadowed → AC-2, AC-6
- [x] `curl -sS -o /dev/null -w '%{http_code}\n' https://mcp.deploy.toyintest.org/login` → `404`: the pages are not on this hostname → AC-2
- [x] `curl -sS -o /dev/null -w '%{http_code}\n' https://mcp.deploy.toyintest.org/v1/uploads/upl_anything` → `404`: the single use fetch stayed off the internet → AC-4
- [x] `curl -sS -o /dev/null -w '%{http_code}\n' https://console.deploy.toyintest.org/v1/uploads -X POST` → `404`: the console still carries no route that changes cluster state → AC-3
- [x] Upload a real tarball to `https://mcp.deploy.toyintest.org/v1/uploads` with a valid bearer token, then call `deploy_app` on `https://mcp.deploy.toyintest.org/mcp` with the id → the app reaches a healthy hostname → **AC-1**
- [ ] Read the audit row that upload wrote → `client_address` is your own public address, not a `10.42.x` pod address → AC-13
- [x] Point an MCP client at `https://mcp.deploy.toyintest.org/mcp` with no Tailscale running and read `deploy_app`'s description → it names `https://mcp.deploy.toyintest.org/v1/uploads` and a 90 MB ceiling → AC-10

## Bounds, against the live platform

- [x] `curl` a body over 90 MB with a declared `Content-Length` → `413` with `{"error":"upload_too_large"}` from the platform, never a Cloudflare error page → AC-11, AC-12
- [x] Upload four tarballs without deploying any of them → the fourth answers `429` with `{"error":"upload_limit_reached"}`, and the volume is unchanged by it → AC-17
- [x] Present a wrong bearer token six times to `/v1/uploads`, then once to `/mcp` → both answer `429` with `{"error":"too_many_attempts"}`, which is the rule living in the authenticator rather than in one handler → AC-16, AC-19
- [ ] Leave an upload unclaimed for over an hour, then check the volume and the `uploads` table → the file and the row are both gone, and an upload a deployment names is still there → AC-18
- [x] Restart the pod, then present a wrong token once → it is refused as an ordinary bad token, not as a locked out one, which is the in memory bound the spec accepted → AC-23

## The timing bound and the cutover

- [x] Call every MCP tool through `https://mcp.deploy.toyintest.org/mcp` and record each one's wall clock → every one returns in under 30 seconds, well inside Cloudflare's 125 second origin timeout → **AC-20**
- [ ] Only after AC-1 and AC-20 have both been observed: remove the tailnet registrations for `POST /v1/uploads` and `/mcp` in their own commit, then `curl` both on the tailnet name → plain `404` → **AC-5**, **AC-21**
      (The commit landed on 2026-08-16, after AC-1 and AC-20 were both observed live.
      `TestTheCutoverTookTheDeployRoutesOffTheTailnet` pins both routes at 404 on the
      default pattern and pins `GET /v1/uploads/{id}` still answering there. The `curl`
      against the real tailnet name is still owed and needs this branch on the cluster.)

## Acceptance-criteria coverage

- AC-1 real deploy through the public hostname · AC-2 opt in registration, live and in the suite · AC-3 console carries no deploy route · AC-4 fetch route stayed internal · AC-5 tailnet 404s after the cutover · AC-6 tunnel rule order · AC-7 nothing else became public · AC-8 host validation · AC-9 removed variable fails the boot · AC-10 description carries the endpoint and the ceiling · AC-11 the 90 MB default · AC-12 both size gates · AC-13 header trusted on two hosts only · AC-14 one address per visitor · AC-15 the deploy path's own limiter · AC-16 the lockout on both routes · AC-17 the unclaimed cap · AC-18 the sweep · AC-19 closed codes on every refusal · AC-20 every tool under the edge timeout · AC-21 the cutover is last and alone · AC-22 no new network policy peer · AC-23 the restart bound, recorded rather than hidden
