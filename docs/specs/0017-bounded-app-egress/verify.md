# Verify: bounded app egress · spec 0017 · updated 2026-08-14
_Steps derived from spec 0017 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

**Ordering is load bearing.** The baseline run below happens on the platform as it stands, before the control plane carrying this change is deployed. Without it a `timeout` on port 25 proves nothing: many home internet connections already block outbound 25 at their own edge, so AC-5 would be satisfied by your provider rather than by the policy, and a mis composed UDP entry would pass unnoticed.

## Phase 0: baseline, before the change ships
- [ ] Deploy `testdata/probe` through the real `deploy_app` path on the currently running platform, then `GET /probe` on its hostname → `public_smtp_mx`, `public_stratum`, `public_smtp_relay` and `public_host` all read `reached` → AC-7, AC-8
- [ ] Record the four outcomes in this file before going further. Any one of them reading `timeout` here means the upstream connection is doing the blocking, and AC-5 is carried by whichever target still read `reached` → AC-8

## Phase 1: the app bound, live
- [ ] Ship the control plane change, then `GET /probe` again → `public_smtp_mx` reads `timeout`, not `refused` → AC-5
- [ ] Same response → `public_stratum` reads `timeout`, not `refused` → AC-5
- [ ] Same response → `public_smtp_relay` (587) and `public_host` (443) both still read `reached` → AC-6
- [ ] `curl` the probe's own hostname through ingress → it serves, and `kubectl get pods -n app-<slug>` shows it ready → AC-12
- [ ] `kubectl get networkpolicy app-allow -n app-<slug> -o yaml` → the public egress rule carries eight TCP entries with `endPort` plus one UDP entry with `protocol: UDP` and no `port`, on exactly one peer → AC-3, AC-4
- [ ] Confirm Cilium actually enforces `endPort` on this version: the `timeout` above is the proof. A `reached` on 25 with the ranges composed correctly means falling back to the `CiliumClusterwideNetworkPolicy` in the spec's Follow-up → AC-3

## Phase 2: the retrofit
- [ ] Pick an app namespace created before this change, restart the control plane pod, do not redeploy the app, then `kubectl get networkpolicy app-allow -n app-<older-slug> -o yaml` → it carries the same ports list → AC-11

## Phase 3: the build namespaces
- [ ] Ship `deploy/builds-networkpolicy.yaml` and `deploy/builds-dockerfile-networkpolicy.yaml` through ArgoCD → both show synced, each public egress rule carries only TCP 80 and TCP 443 → AC-9
- [ ] Run one Buildpacks build end to end (`testdata/sample-buildpacks`, or the fixture the build tests use) → it fetches dependencies and pushes its image, deployment reaches `healthy` → AC-10
- [ ] Run one Dockerfile build end to end (`testdata/sample-dockerfile`) → same → AC-10

## Commands
- [ ] `go test -race ./internal/config/ -run BlockedPorts` → the default, empty, override, sort and dedup, and every boot refusal pass → AC-1
- [ ] `go test -race ./internal/deploy/ -run PortRange` → adjacency, duplicates, both boundaries, the one port wide range → AC-2
- [ ] `go test -race ./internal/deploy/ -run 'AllowPolicy|PublicEgress|BlockedPorts|OrdinaryPorts'` → the eight literal ranges, the explicit UDP protocol, the single peer → AC-3, AC-4, AC-14
- [ ] `go test -race ./internal/config/ -run BuildNamespacePolic` → both YAML files pin TCP 80 and 443 → AC-9
- [ ] `go test -race ./internal/mcp/ -run EgressBound` → the description names port 25, mining pools, the silent timeout and 587, and no literal pool port → AC-13
- [ ] `DEPLOYER_APP_EGRESS_BLOCKED_PORTS=banana` on the real deployment → the pod refuses to start and the log names the variable → AC-1

## Value sourcing
- [ ] Set `DEPLOYER_APP_EGRESS_BLOCKED_PORTS=25` only, restart, read `app-allow` → two TCP ranges (`1-24`, `26-65535`), not eight: the ranges come from the complement function, never from a literal → spec Value sourcing, AC-2
- [ ] With that override live, `GET /probe` → `public_stratum` reads `reached` again, `public_smtp_mx` still `timeout`: the configured list is the single description of the bound → AC-1
- [ ] Restore the default and restart → the eight ranges come back with no redeploy of any app → AC-11
- [ ] `deploy_app`'s description names the shape and not the numbers, so neither override above falsifies it → AC-13

## Acceptance-criteria coverage
- AC-1 covered by the config command steps and the two override steps · AC-2 by the PortRange command step and the first override step · AC-3 by the live policy read, the Cilium enforcement step and the deploy command step · AC-4 by the live policy read and the deploy command step · AC-5 by the two phase 1 timeout steps, read against phase 0 · AC-6 by the 443 and 587 step · AC-7 by the phase 0 probe step · AC-8 by the phase 0 recording step · AC-9 by the ArgoCD step and the config command step · AC-10 by the two build steps · AC-11 by the phase 2 step and the restore step · AC-12 by the ingress and readiness step · AC-13 by the mcp command step and the last value sourcing step · AC-14 by the deploy command step
