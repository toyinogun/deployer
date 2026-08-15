# Verify: open internet hardening · spec 0019 · updated 2026-08-15
_Steps derived from spec 0019 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The two halves are independent. The policy half is the one that can take the
platform down, so run it first and keep the revert to hand: remove both files
from `deploy/kustomization.yaml` and sync, or `kubectl delete networkpolicy -n
deployer-system default-deny control-plane-allow` together with `kubectl delete
ciliumnetworkpolicy -n deployer-system node-ingress-allow`.

Apply the v1 pair and the Cilium object together, never one first. The v1 pair
alone is exactly the state that broke image pulls on 2026-08-15. After a revert,
delete any pod already in `ImagePullBackOff` rather than waiting: the backoff
window is long enough to read as the revert not having worked.

## Commands: the control plane policy

- [ ] `kubectl get pods -n tailscale -o wide | grep ts-deployer` → the deployer ingress proxy pod is listed, so the `tailscale` namespace selector matches where it really runs. A ProxyClass can move it. → AC-18
- [ ] `kubectl -n kube-system get cm cilium-config -o yaml | grep -E 'policy-cidr-match-mode|enable-host-firewall'` → `policy-cidr-match-mode` is unset or empty, and host firewall is off. If either has changed, the reasoning under AC-12a has moved and the rule needs rereading before anything is applied. → AC-12a
- [ ] `kubectl -n argocd get appproject deployer -o jsonpath='{.spec.clusterResourceWhitelist}'` → five kinds, no `cilium.io`. This is why the Cilium object is namespaced. If a cluster scoped one ever looks tempting, this is the check that says no. → AC-11
- [ ] `go test ./internal/config/` → passes. Run the whole package rather than `-run ControlPlane`: the Cilium assertions live in `nodepolicy_test.go` under their own name, so a filter written for the old test silently skips the half that is new. Then widen one thing by hand and confirm it fails, then put it back: delete a `ports:` clause, or add `Egress` to `policyTypes`, or change `remote-node` to `host` in the Cilium file, or put an `ipBlock` back in the v1 file. A test that only ever passes proves nothing. Note what these tests cannot do: a peer left out makes the list shorter, which is a valid policy, so they will never report one missing. The live checks below are the only thing that does. → AC-15
- [ ] `kubectl kustomize deploy/ | grep -E "name: (control-plane-allow|node-ingress-allow)"` → both are in the built output, so ArgoCD will apply them. Nothing in CI checks the kustomization entries. → AC-11
- [ ] `kubectl apply -k deploy/ --dry-run=server` → the API server accepts all three objects before anything is enforced. → AC-11
- [ ] `git diff --name-only main...HEAD` → no `configmap.yaml`, no `internal/config/config.go` env addition, no file under the migrations directory, no `deploy/rbac.yaml` and no `deploy/admission-policy.yaml`. The blast radius of this feature is the two policy files, `internal/config`, `internal/web`, and the two context files. → AC-17

## Commands: the policy proved live

Run these after the policy is actually applied and synced.

- [ ] From a pod in any `app-<slug>` namespace: `wget -T5 -O- http://deployer.deployer-system:80` → times out or is refused, not answered. Use the Service name a stray pod would try; the policy bites on the pod's 8080 behind it. → AC-13
- [ ] From the same pod: `wget -T5 -O- http://deployer-registry.deployer-system:5000/v2/` → times out or is refused. → AC-13
- [ ] Open the console over the tailnet and sign in → the page loads and the session works, so the `tailscale` peer on 8080 is right. → AC-14
- [ ] **The cross node pull, and run it before the rest.** This is the check that has been got wrong twice, and both times everything around it passed. First `kubectl -n deployer-system get pods -o wide | grep registry` to read which node the registry is on right now, because nothing pins it. Then run a throwaway Pod with `nodeName` set to any other node, its image an app digest that node has never pulled, and a `restricted` security context. Expected: `Pulled`. Then delete the Cilium object and repeat with a second fresh digest. Expected: `dial tcp <registry clusterIP>:5000: i/o timeout` into `ImagePullBackOff`. A check that only ever passes proves nothing here either. Two traps: a digest the node has already pulled is answered from the layer cache and never touches the registry, and a Pod the scheduler placed rather than you pinned may land on the registry's own node, which is `host` traffic and passes on a rule that has nothing to do with nodes. → AC-12a, AC-14
- [ ] `kubectl get pods -n deployer-system` → every pod stays `Running` and `1/1` for several minutes, so the kubelet's probes on 8080 and 5000 still land. Read this narrowly: each pod's probe comes from its own node, so this exercises the `host` rule only and says nothing about `remote-node`. It passed for ten minutes during the run that broke three nodes out of four. The check above is the one that covers the rest. → AC-12a, AC-14
- [ ] From a pod inside `deployer-system` itself: reach `http://deployer-registry.deployer-system:5000/v2/` → answers `401`, not a timeout. This is the control plane's own path to the registry and it is the peer the first attempt left out. The namespace enforces `restricted`, so a bare `kubectl run` is refused by admission: the probe pod needs `runAsNonRoot`, `runAsUser`, `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]`. → AC-12, AC-14
- [ ] Deploy an app through the Paketo path end to end, and read the state, not the Job → it reaches `healthy`, not merely `Complete`. A build Job goes green on a successful push and the next step is the control plane reading the digest back, so a green Job with a failed deploy is exactly what a missing in namespace peer looks like. A `build_no_digest` here means the registry was unreachable, not that the build produced nothing: check the build pod log for a `*** Images (sha256:...)` line before believing the reason code. → AC-14
- [ ] Deploy an app through the Dockerfile path end to end → same, for `deployer-builds-dockerfile`. → AC-14
- [ ] `kubectl describe pod -n app-<slug> <pod>` after a fresh deploy → the image pull succeeded, and **record which node it ran on**. If that is the registry's own node the pull proved nothing about `remote-node`, so redeploy until it lands elsewhere or rely on the pinned check above. Writing the node down is the part that was missing last time. → AC-12a, AC-14
- [ ] If the cross node pull fails with the Cilium object applied, stop rather than widening anything. The remaining lever is `policy-cidr-match-mode` on the cluster, which is a change outside this repository affecting every policy on it, and that comes back through `/architect`. → AC-19

## Commands: the CSRF guard

- [ ] `go test -race ./internal/web/` → passes, including the ten new tests in `internal/web/pretoken_test.go`. → AC-1 to AC-10
- [ ] `go test ./internal/httpapi/` → passes unchanged. No file in `internal/httpapi` was touched, so the JSON identity endpoints are unaffected and a curl caller against `/v1/auth/*` needs no cookie. → AC-8
- [ ] `curl -i -X POST https://<console>/v1/auth/login -H 'content-type: application/json' -d '{"email":"...","password":"..."}'` → answers normally with no cookie and no `csrf` field anywhere. → AC-8

## UI / manual: the CSRF guard

Run these in a real browser against the deployed console, because the cookie
attributes are the half a test on a laptop cannot prove.

- [ ] Open `/login` in a fresh private window, then look at the cookie in devtools → named `__Host-deployer_csrf`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`, and "Session" rather than a date under expiry. → AC-2
- [ ] View source on `/login` → a hidden `csrf` field holding 64 hex characters, and that value is not the cookie's value. The cookie holds the nonce, the field holds its HMAC, and they must differ. → AC-3, AC-9
- [ ] Repeat on `/register`, `/forgot`, `/reset` and `/unverified` → each sets the cookie and renders the field. `/resend` has no page of its own; its form is the one on `/unverified`. → AC-1
- [ ] Load a page that carries no pre sign in form, such as `/verify?token=nope` → no `__Host-deployer_csrf` cookie is set. → AC-1
- [ ] Sign in successfully → in the same response, the session cookie is set and `__Host-deployer_csrf` is deleted. Exactly one mechanism is live at a time. → AC-7
- [ ] Open `/login`, delete the `__Host-deployer_csrf` cookie in devtools, then submit → 403, the sign in form comes back with your address still in it and the password box empty, a sentence says the form expired, and a fresh cookie is set. Submitting again works. → AC-4, AC-5, AC-6
- [ ] Same on `/forgot` and on `/reset` reached from a real reset link → the refusal comes back as that form, not as a standalone message page, and the reset link's token survives in the hidden field. → AC-6
- [ ] Open `/login` in two tabs, submit in the second, go back and submit in the first → both are accepted. The nonce is reused per browser session, not rotated per render. → AC-10
- [ ] `kubectl exec` into the platform pod and read the audit rows for `page_csrf` after the two refusals above → one row reads `csrf_pretoken_missing` and, if you also tampered with the field rather than deleting the cookie, one reads `csrf_pretoken_mismatch`. Neither reads `csrf_invalid`, which belongs to the signed in path. → AC-5
- [ ] Grep the same rows and the platform log at info level for the cookie's value → it appears in neither. Only the HMAC ever leaves the cookie jar. → AC-9
- [ ] Run the platform locally over plain HTTP (`DEPLOYER_PUBLIC_URL=http://localhost:8080`), open `/login` → the cookie is named `deployer_csrf` with no prefix and no `Secure` flag, and a sign in completes. A browser refuses a `Secure` cookie over HTTP, so this carve out is what makes local development possible; it is also why the sibling subdomain guarantee cannot be exercised here. → AC-2a

## Acceptance-criteria coverage

- AC-1 covered by the five page checks, the no-cookie-elsewhere check, and `TestEveryPreAuthPageSetsTheNonceCookieAndRendersItsToken` · AC-2 by the devtools attribute walk and `TestTheNonceCookieCarriesTheHostPrefixAndItsFlags` · AC-2a by the local plain HTTP run and `TestOverPlainHTTPTheCookieDropsThePrefixAndStillSignsIn` · AC-3 by the view source check · AC-4 by the deleted cookie submit and `TestEveryGuardedPostRefusesWithoutItsPair` · AC-5 by the audit row reads and the two reason tests · AC-6 by the re rendered form checks on `/login`, `/forgot` and `/reset` · AC-7 by the sign in response check · AC-8 by the httpapi suite and the curl · AC-9 by the nonce grep and the extended leak crawl · AC-10 by the two tab check
- AC-11 by the kustomize build, the server dry run and the AppProject whitelist read · AC-12 by the in namespace registry probe and the deploys · AC-12a by the cross node pull proved both ways, the Cilium config read, and the probe check for the `host` half · AC-13 by the two refusals from an app namespace · AC-14 by the console, the probes, both build paths carried to `healthy`, the in namespace registry probe and the cross node pull · AC-15 by the parse tests plus their deliberate widening · AC-16 withdrawn, nothing to check · AC-17 by the absence of any `DEPLOYER_*`, migration, `rbac.yaml` or `admission-policy.yaml` change in the diff · AC-18 by the proxy pod namespace check · AC-19 resolved by the 2026-08-15 proof, with the stop condition now guarding the one lever left
