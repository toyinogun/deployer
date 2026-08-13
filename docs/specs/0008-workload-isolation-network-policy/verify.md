# Verify: workload isolation and network policy · spec 0008 · updated 2026-08-13
_Steps derived from spec 0008 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The fake clientset enforces no policy at all, so a wrong CIDR, a mis-shaped selector, or a DNS rule that accidentally opens all of kube-system passes the whole unit suite. Everything below has to run against the real cluster. Most of it runs through the probe app (`testdata/probe`), deployed the ordinary way with `deploy_app` so the policy under test is the one the platform actually writes.

## Shape of the composed objects

- [x] `kubectl get netpol -n app-<slug>` → exactly two, `default-deny` and `app-allow`, both labelled `app.kubernetes.io/managed-by=deployer` → AC-1
- [x] `kubectl get netpol default-deny -n app-<slug> -o yaml` → empty `podSelector`, both `Ingress` and `Egress` in `policyTypes`, no `ingress` or `egress` rules at all → AC-2
- [x] `kubectl get netpol app-allow -n app-<slug> -o yaml` → the ingress rule names `kubernetes.io/metadata.name: ingress-nginx` and TCP 8080 only; the DNS rule carries `namespaceSelector` and `podSelector` inside **one** peer entry (a single list item with both keys, not two items); the internet rule is `0.0.0.0/0` with every configured CIDR under `except` → AC-3, AC-4, AC-5

## The fence, from inside an app

Deploy the probe app twice, under two slugs, so each is the other's sibling. Read each one's `/probe` report over its own hostname. Every dial carries a 3 second timeout, so a silent drop reports as `timeout` rather than hanging.

- [x] Probe reaches a sibling app's pod IP → refused or timed out → AC-6
- [x] Probe reaches a sibling app's Service IP (`app.app-<other-slug>.svc:80`) → refused or timed out → AC-6
- [x] Probe reaches `10.43.0.1:443`, the Kubernetes API → refused or timed out → AC-7
- [x] Probe reaches `deployer-registry.deployer-system.svc:5000` and the control plane's internal URL → both refused or timed out → AC-7
- [x] Probe reaches a node IP (`172.16.70.20`) and the ingress load balancer (`172.16.70.40`) → both refused or timed out → AC-8
- [ ] Probe requests a sibling app's public hostname → fails, and the failure is the LAN block rather than DNS → AC-8
- [x] Probe resolves a public name and opens `https://` to it → succeeds → AC-9
- [x] The probe app is itself reachable on its own hostname over HTTPS from the tailnet, and `kubectl get pod -n app-<slug>` shows it Ready → the policy neither blocks ingress nor breaks the kubelet's readiness probe → AC-10

## Drift, retrofit, and failure

- [x] `kubectl delete netpol app-allow -n app-<slug>`, redeploy the app, `kubectl get netpol -n app-<slug>` → back, and byte identical to before → AC-11
- [ ] Weaken `app-allow` by hand (widen the ingress rule), redeploy → the hand edit is gone → AC-11
- [x] Pick an app namespace created before this slice, confirm it has no policies, restart the control plane pod, then `kubectl get netpol -n app-<old-slug>` → both present, with no redeploy of that app → AC-12
- [x] `kubectl get netpol -A -l app.kubernetes.io/managed-by=deployer` after the restart → one pair per app namespace, all seven pre-existing namespaces covered → AC-12
- [x] Force a policy write failure (temporarily drop `networkpolicies` from `ClusterRole/deployer-app`, or point at a namespace the binding does not cover) and deploy → the deployment ends `failed` with reason `internal`, and `kubectl get deploy -n app-<slug>` shows no workload was created → AC-13
- [x] Set `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS` to `not-a-cidr`, then to a valid IPv6 CIDR, then to `,  ,`, and restart → the pod fails to start all three times with a config error naming the variable, not a later deploy failure. Set it to the empty string → it boots on the default list, because an unset variable and an empty one are the same string to `os.Getenv` → AC-14
- [ ] `kubectl get netpol -A -l app.kubernetes.io/managed-by=deployer` before and after `PolicySweep` runs, with the deployment sweep's own log lines interleaved → the policy sweep completes first, and one namespace failing does not stop the others → AC-12

## The build namespace

- [x] `kubectl get netpol -n deployer-builds -o yaml` → deny both directions, no ingress rule anywhere, egress limited to CoreDNS, `deployer-system` on TCP 5000 and 8080, and `0.0.0.0/0` with the same `except` list → AC-15
- [x] Run a real `deploy_app` build end to end → source fetch, dependency download, and image push all succeed under the policy, and the deployment reaches `healthy` → AC-16
- [ ] While a build Job is running, `kubectl exec` into the build pod if the image allows it, or read the Job's logs, and confirm no connection is made outside those three destinations → AC-15

## Value sourcing

One step per row of the spec's Value sourcing table, exercising the edge that breaks if the source is wrong.

- [x] Change `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS` to add a public range, restart, redeploy an app, and have the probe try that range → now blocked, proving the list is read from config and not compiled in → blocked CIDR list
- [x] `kubectl get ns ingress-nginx -o jsonpath='{.metadata.labels}'` → `kubernetes.io/metadata.name` is present and set by the API server, so the ingress rule cannot be dodged by relabelling → ingress controller identity
- [x] `kubectl get pods -n kube-system -l k8s-app=kube-dns` → the CoreDNS pods and nothing else, so the DNS hole is exactly CoreDNS → DNS pod identity
- [ ] Create a namespace labelled `app.kubernetes.io/managed-by=deployer` with no matching apps row, restart the control plane → it gets policed, proving the sweep reads the cluster and not the database → sweep source

## Structural pins (unit, but listed so nothing is lost)

- [x] `go test ./internal/deploy/... ./internal/build/...` → the pod spec test proves no host or privileged field on either the app Deployment or the build Job, and the `deploy.Input` test proves no passthrough field exists → AC-17, AC-18
- [x] `go test ./deploy/...` (or wherever the YAML pin lands) → the build namespace YAML's `except` list matches the config default; change one entry in the YAML and confirm the test fails → AC-20

## The one fact that is not in the code

- [x] `kubectl -n ingress-nginx get pods -o jsonpath='{.items[*].spec.hostNetwork}'` → empty, so the controller pods are on the pod network and the AC-3 namespace selector matches real ingress traffic. Confirmed 2026-08-13. If this ever changes to `true`, AC-10 fails for every app at once and the ingress rule needs rethinking → AC-3, AC-10

## Acceptance-criteria coverage

- AC-1 · AC-2 · AC-3 · AC-4 · AC-5 · AC-6 · AC-7 · AC-8 · AC-9 · AC-10 · AC-11 · AC-12 · AC-13 · AC-14 · AC-15 · AC-16 · AC-17 · AC-18 · AC-20 all covered above. AC-19 is covered by the probe app being the instrument for the fence section rather than by a step of its own.

## Added by /develop, the concrete forms of the steps above

The build made four of the steps above exact rather than approximate. Updated 2026-08-13.

- [x] `go test -race ./...` → green, including the composed policy tests, the boot rejection of a bad CIDR list, and the write order test → AC-1, AC-2, AC-3, AC-4, AC-5, AC-11, AC-12, AC-13, AC-14
- [x] `go test ./internal/config/ -run Drift` → the `except` list in `deploy/builds-networkpolicy.yaml` matches the config default; edit one entry in the YAML and confirm it fails → AC-20
- [x] `go test ./internal/deploy/ -run 'Privileged|Passthrough'` → no host or privileged field on either pod spec, and `deploy.Input` carries no map, pointer, or passthrough named field → AC-17, AC-18
- [x] Deploy `testdata/probe` twice under two slugs, then `curl "https://<slug>.$DEPLOYER_APP_DOMAIN/probe?sibling_pod=<ip>:8080&sibling_service=<ip>:80&node=<ip>:6443&load_balancer=<ip>:443"` → only `public_host` reads `reached`; `sibling_pod`, `sibling_service`, `kubernetes_api`, `registry`, `control_plane`, `node` and `load_balancer` all read `timeout`, because a policy drop is silent rather than a refusal. `testdata/probe/README.md` has the `kubectl` lines that produce the four addresses → AC-6, AC-7, AC-8, AC-9, AC-19
