# Verify: open internet hardening · spec 0019 · updated 2026-08-15
_Steps derived from spec 0019 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The two halves are independent. The policy half is the one that can take the
platform down, so run it first and keep the revert to hand: remove the file from
`deploy/kustomization.yaml` and sync, or `kubectl delete networkpolicy -n
deployer-system default-deny control-plane-allow`.

## Commands: the control plane policy

- [ ] `kubectl get pods -n tailscale -o wide | grep ts-deployer` → the deployer ingress proxy pod is listed, so the `tailscale` namespace selector matches where it really runs. A ProxyClass can move it. → AC-18
- [ ] `kubectl get nodes -o wide` → the four internal addresses are exactly `172.16.70.20` to `.23`, matching the four `/32` entries in the policy. A fifth node means a fifth entry. → AC-12, AC-16
- [ ] `go test ./internal/config/ -run ControlPlane` → passes. Then widen one thing by hand (add a fourth ingress rule, or delete a `ports:` clause, or add `Egress` to `policyTypes`) and confirm it fails, then put it back. A test that only ever passes proves nothing. → AC-15
- [ ] `kubectl kustomize deploy/ | grep -A2 "name: control-plane-allow"` → the policy is in the built output, so ArgoCD will apply it. Nothing in CI checks the kustomization entry. → AC-11
- [ ] `kubectl apply -k deploy/ --dry-run=server` → the API server accepts both objects before anything is enforced. → AC-11

## Commands: the policy proved live

Run these after the policy is actually applied and synced.

- [ ] From a pod in any `app-<slug>` namespace: `wget -T5 -O- http://deployer.deployer-system:80` → times out or is refused, not answered. Use the Service name a stray pod would try; the policy bites on the pod's 8080 behind it. → AC-13
- [ ] From the same pod: `wget -T5 -O- http://deployer-registry.deployer-system:5000/v2/` → times out or is refused. → AC-13
- [ ] Open the console over the tailnet and sign in → the page loads and the session works, so the `tailscale` peer on 8080 is right. → AC-14
- [ ] `kubectl get pods -n deployer-system` → every pod stays `Running` and `1/1` for several minutes, so the kubelet's probes on 8080 and 5000 still land from the node addresses. This is the failure the node peer exists to prevent, and it shows up as a restart loop rather than an error. → AC-12, AC-14
- [ ] Deploy an app through the Paketo path end to end → the build completes, which proves `deployer-builds` can still fetch the tarball on 8080 and push on 5000. → AC-14
- [ ] Deploy an app through the Dockerfile path end to end → same, for `deployer-builds-dockerfile`. → AC-14
- [ ] `kubectl describe pod -n app-<slug> <pod>` after a fresh deploy → the image pull succeeded, so containerd reached the registry on 5000 as the node. Force it onto a node that has not pulled the image before, or the layer cache hides the failure. → AC-12, AC-14
- [ ] If any of the three above fail on the node peer specifically, stop: the fallback is a CiliumNetworkPolicy using the `remote-node` entity, which a portable `networking.k8s.io/v1` policy cannot express. That comes back through `/architect`, it is not improvised here. → AC-19

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
- AC-11 by the kustomize build and the server dry run · AC-12 by the node address read, the probe check and the image pull · AC-13 by the two refusals from an app namespace · AC-14 by the console, both build paths and the pull · AC-15 by the parse test plus its deliberate widening · AC-16 by the node address read against the file's comment · AC-17 by the absence of any `DEPLOYER_*`, migration, `rbac.yaml` or `admission-policy.yaml` change in the diff · AC-18 by the proxy pod namespace check · AC-19 by the stop condition on the node peer
