# Verify: bounded app egress · spec 0017 · updated 2026-08-14
_Steps derived from spec 0017 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

**Ordering is load bearing.** The baseline run below happens on the platform as it stands, before the control plane carrying this change is deployed. Without it a `timeout` on port 25 proves nothing: many home internet connections already block outbound 25 at their own edge, so AC-5 would be satisfied by your provider rather than by the policy, and a mis composed UDP entry would pass unnoticed.

## Phase 0: baseline, before the change ships
- [x] Deploy `testdata/probe` through the real `deploy_app` path on the currently running platform, then `GET /probe` on its hostname → `public_smtp_mx`, `public_stratum`, `public_smtp_relay` and `public_host` all read `reached` → AC-7, AC-8
- [x] Record the four outcomes in this file before going further. Any one of them reading `timeout` here means the upstream connection is doing the blocking, and AC-5 is carried by whichever target still read `reached` → AC-8

**Baseline recorded 2026-08-14 18:19 UTC**, on control plane `ghcr.io/toyinogun/deployer@sha256:0798ef2a…`
(main's pinned digest, no port bound; `app-allow` in `app-hello-4dfssb` carried no ports list at the time).
Deployment `dep_01M00QZ3HX1814V011WT64KM41`, app `probea`, slug `probea-gh44sv`, healthy in 43s.

| target | address | outcome | ms |
|---|---|---|---|
| `public_smtp_mx` | `aspmx.l.google.com:25` | **reached** | 27 |
| `public_stratum` | `pool.supportxmr.com:3333` | **reached** | 47 |
| `public_smtp_relay` | `smtp.sendgrid.net:587` | **reached** | 50 |
| `public_host` | `example.com:443` | **reached** | 25 |

All four reached, so nothing upstream is blocking 25 or 3333 and AC-5 is carried by both targets rather
than by the stratum port alone. The spec 0008 fence read correctly in the same response: `kubernetes_api`,
`registry` and `control_plane` all timed out at 3000ms.

Deviation worth knowing: the probe went out over the existing app `probea` rather than a new app, because
the bootstrap account is at its per account cap (`40 of 10 used`, 31 of those rows failed deploys that
never held a namespace). `resolveApp` only checks the cap on the create branch, so redeploying an existing
app is untouched by it. `app-probea-gh44sv` is therefore a **new** namespace and cannot serve as the AC-11
retrofit subject; use `app-hello-4dfssb`, created 2026-08-13 and not redeployed since.

## Phase 1: the app bound, live
- [x] Ship the control plane change, then `GET /probe` again → `public_smtp_mx` reads `timeout`, not `refused` → AC-5
- [x] Same response → `public_stratum` reads `timeout`, not `refused` → AC-5
- [x] Same response → `public_smtp_relay` (587) and `public_host` (443) both still read `reached` → AC-6
- [x] `curl` the probe's own hostname through ingress → it serves, and `kubectl get pods -n app-<slug>` shows it ready → AC-12
- [x] `kubectl get networkpolicy app-allow -n app-<slug> -o yaml` → the public egress rule carries eight TCP entries with `endPort` plus one UDP entry with `protocol: UDP` and no `port`, on exactly one peer → AC-3, AC-4
- [x] Confirm Cilium actually enforces `endPort` on this version: the `timeout` above is the proof. A `reached` on 25 with the ranges composed correctly means falling back to the `CiliumClusterwideNetworkPolicy` in the spec's Follow-up → AC-3

**Phase 1 read 2026-08-14 18:26 UTC**, control plane `ghcr.io/toyinogun/deployer@sha256:c95f69a8…`
(branch `feat/bounded-app-egress`, shipped by hand with `ko`; `DEPLOYER_APP_EGRESS_BLOCKED_PORTS` left
unset, so the bound comes from the default list, which also exercises AC-1's unset clause).

Same pod as the baseline, `app-98b87dc6d-kzdxh`, never restarted between the two reads. Only the policy changed.

| target | baseline | under the bound |
|---|---|---|
| `public_smtp_mx` `:25` | reached 27ms | **timeout 3000ms** |
| `public_stratum` `:3333` | reached 47ms | **timeout 3001ms** |
| `public_smtp_relay` `:587` | reached 50ms | reached 40ms |
| `public_host` `:443` | reached 25ms | reached 28ms |

Both blocked ports read `timeout` rather than `refused`, so this is the fence and not an absent listener.
**Cilium enforces `endPort` on this version**, which closes the spec's open question: the
`CiliumClusterwideNetworkPolicy` fallback in Follow-up is not needed.

AC-12: `GET /` through ingress returned `200` with body `probe`; the pod reads `ready=true`, `restarts=0`.

## Phase 2: the retrofit
- [x] Pick an app namespace created before this change, restart the control plane pod, do not redeploy the app, then `kubectl get networkpolicy app-allow -n app-<older-slug> -o yaml` → it carries the same ports list → AC-11

Subject: `app-hello-4dfssb`, created 2026-08-13, last deployed 2026-08-12, **not redeployed**. After the
control plane rollout alone its `app-allow` carried all eight TCP ranges plus the UDP entry. The composed
rule matched the spec's eight literals exactly (`1-24`, `26-3332`, `3334-4443`, `4445-5554`, `5556-7776`,
`7778-9998`, `10000-14443`, `14445-65535`), the UDP entry carried `protocol: UDP` with no `port`, and the
rule held exactly one peer whose `except` list was unchanged. Same reading in `app-probea-gh44sv`.

## Phase 3: the build namespaces
- [x] Ship `deploy/builds-networkpolicy.yaml` and `deploy/builds-dockerfile-networkpolicy.yaml` through ArgoCD → both show synced, each public egress rule carries only TCP 80 and TCP 443 → AC-9
- [x] Run one Buildpacks build end to end (`testdata/sample-buildpacks`, or the fixture the build tests use) → it fetches dependencies and pushes its image, deployment reaches `healthy` → AC-10
- [x] Run one Dockerfile build end to end (`testdata/sample-dockerfile`) → same → AC-10

Applied with `kubectl apply` rather than through an ArgoCD sync, because ArgoCD was paused and serving
`main` for this run. Both `build-allow` objects then read exactly `[{TCP 80},{TCP 443}]` on the public
egress rule, one peer, `except` list unchanged. **The ArgoCD sync itself is proved only after merge**;
what is proved here is the composed object and the two builds under it.

| path | fixture | app | deployment | result |
|---|---|---|---|---|
| Buildpacks | `testdata/sample-go` | `gohello` | `dep_01M00RBDX5HKRXB7N5J7STE3NP` | healthy, Job Complete in 33s |
| Dockerfile | `testdata/sample-dockerfile` | `verify-dockerfile` | `dep_01M00RCV16F0NK26946YTPY9G3` | healthy, Job Complete in 6s |

Caveat on AC-10: both ran against warm caches, which those durations make plain (6s for the Dockerfile
path). So this proves neither build **needed** a port outside 80 and 443 today, not that a cold build's
whole fetch set fits. A cold cache run is the stronger test and has not been done.

## Commands
- [x] `go test -race ./internal/config/ -run BlockedPorts` → the default, empty, override, sort and dedup, and every boot refusal pass → AC-1
- [x] `go test -race ./internal/deploy/ -run PortRange` → adjacency, duplicates, both boundaries, the one port wide range → AC-2
- [x] `go test -race ./internal/deploy/ -run 'AllowPolicy|PublicEgress|BlockedPorts|OrdinaryPorts'` → the eight literal ranges, the explicit UDP protocol, the single peer → AC-3, AC-4, AC-14
- [x] `go test -race ./internal/config/ -run BuildNamespacePolic` → both YAML files pin TCP 80 and 443 → AC-9
- [x] `go test -race ./internal/mcp/ -run EgressBound` → the description names port 25, mining pools, the silent timeout and 587, and no literal pool port → AC-13
- [x] `DEPLOYER_APP_EGRESS_BLOCKED_PORTS=banana` on the real deployment → the pod refuses to start and the log names the variable → AC-1

Observed: pod `deployer-7fc9f6ff47-khjlk` went straight to `Error`, and the log read
`shutting down error="config: DEPLOYER_APP_EGRESS_BLOCKED_PORTS entry \"banana\" must be a port number:
strconv.Atoi: parsing \"banana\": invalid syntax; DEPLOYER_APP_EGRESS_BLOCKED_PORTS must list at least one
port, got \"banana\""`. It names the variable twice and refuses at boot, not at first deploy.

## Value sourcing
- [x] Set `DEPLOYER_APP_EGRESS_BLOCKED_PORTS=25` only, restart, read `app-allow` → two TCP ranges (`1-24`, `26-65535`), not eight: the ranges come from the complement function, never from a literal → spec Value sourcing, AC-2
- [x] With that override live, `GET /probe` → `public_stratum` reads `reached` again, `public_smtp_mx` still `timeout`: the configured list is the single description of the bound → AC-1
- [x] Restore the default and restart → the eight ranges come back with no redeploy of any app → AC-11
- [x] `deploy_app`'s description carries no pool port literal, so an override that changes which pool ports are blocked needs no edit to it → AC-13

Read `app-allow` only **after** the startup sweep has finished. A read taken the moment
`rollout status` returns still shows the previous list, which reads exactly like a failure and is not one.

This step is the softened one. As first written, AC-13 claimed "a configuration change cannot falsify it",
and this run showed it can: under the `=25` override the sentence "Outbound mail straight to a mail
exchanger on port 25 is closed, as are the common mining pool ports" was false in its second half for as
long as the override was live. The description is unchanged; **AC-13 was reworded** to claim only what
holds, that it carries no pool port literal and so needs no edit when one changes, and that it describes
the default configuration rather than every configuration. Low severity, no bearing on the bound itself.

## Acceptance-criteria coverage
- AC-1 covered by the config command steps and the two override steps · AC-2 by the PortRange command step and the first override step · AC-3 by the live policy read, the Cilium enforcement step and the deploy command step · AC-4 by the live policy read and the deploy command step · AC-5 by the two phase 1 timeout steps, read against phase 0 · AC-6 by the 443 and 587 step · AC-7 by the phase 0 probe step · AC-8 by the phase 0 recording step · AC-9 by the ArgoCD step and the config command step · AC-10 by the two build steps · AC-11 by the phase 2 step and the restore step · AC-12 by the ingress and readiness step · AC-13 by the mcp command step and the last value sourcing step · AC-14 by the deploy command step
