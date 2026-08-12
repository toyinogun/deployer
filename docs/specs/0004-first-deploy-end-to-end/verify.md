# 0004. First deploy end to end: verify steps

The hand run steps. Everything else in this feature is covered by automated tests against the fake clientset and a real SQLite file; what is here needs the real cluster, the real registry, or a real agent session.

Run these in order. Each names the acceptance criterion it proves.

## Before you start

- `kubectl` pointed at `k3sprox-operator.tail62ceef.ts.net`.
- The platform already deployed and ready (spec 0003).
- A tailnet device that is not on your LAN, for the reachability checks.

## 1. The registry is up and private (AC-20)

```bash
kubectl -n deployer-system get pods -l app=registry
kubectl -n deployer-system get pvc registry-data
kubectl get ingress -A | grep -i registry     # expect no output
```

Push and pull once by hand from inside the cluster, using the htpasswd credential from the sealed secret:

```bash
kubectl -n deployer-system run regcheck --rm -it --restart=Never \
  --image=<a small image with crane or skopeo> -- \
  crane copy <a tiny public image> registry.deployer-system.svc:5000/apps/regcheck:probe
```

Expect the copy to succeed, and the same command without credentials to be refused.

## 2. The build namespace refuses what it should (AC-7)

```bash
kubectl get ns deployer-builds -o jsonpath='{.metadata.labels}' | grep restricted
```

Apply a pod into `deployer-builds` asking for `privileged: true`, and one with no `securityContext` at all. Both must be rejected by the API server, not by platform code.

## 3. Bootstrap seeding is idempotent and quiet (AC-1)

Restart the control plane twice, then:

```bash
kubectl -n deployer-system exec deploy/deployer -- sqlite3 /data/deployer.db \
  "select count(*) from accounts; select count(*) from api_tokens;"
```

Expect `1` and `1` after both restarts. Then read the pod log and confirm the raw token value appears nowhere in it.

## 4. Upload rejects what it should (AC-2)

From a tailnet device, with `$TOKEN` set to the bootstrap token and `$URL` to `DEPLOYER_PUBLIC_URL`:

```bash
# no token
curl -sS -o /dev/null -w '%{http_code}\n' -X POST $URL/v1/uploads --data-binary @-  </dev/null   # expect 401

# over the cap
head -c $((200*1024*1024)) /dev/urandom | \
  curl -sS -o /dev/null -w '%{http_code}\n' -X POST $URL/v1/uploads \
    -H "Authorization: Bearer $TOKEN" --data-binary @-                                            # expect 413

# not gzip
echo hello | curl -sS -o /dev/null -w '%{http_code}\n' -X POST $URL/v1/uploads \
  -H "Authorization: Bearer $TOKEN" --data-binary @-                                              # expect 400
```

Then confirm each denial wrote one `audit_log` row.

## 5. The real deploy, twice (AC-21, AC-4, AC-15)

From a fresh agent session with the MCP server configured:

```bash
tar czf - -C testdata/sample-go . | \
  curl -sS -X POST $URL/v1/uploads -H "Authorization: Bearer $TOKEN" --data-binary @-
```

Take the `upload_id`, then have the agent call `deploy_app` with `name: hello` and that id.

Expect back a URL of the form `https://hello-<suffix>.<DEPLOYER_APP_DOMAIN>`, a deployment id, release number 1, and a digest. Then:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://hello-<suffix>.<domain>    # expect 200, from a tailnet device
```

Repeat the whole thing. The second deploy must return the **same** hostname and release number 2. Confirm the running pod's image is the new digest.

## 6. The workload is shaped as specified (AC-12, AC-13, AC-14)

```bash
kubectl -n app-hello-<suffix> get deploy -o yaml
```

Check by eye: image is a `@sha256:` reference and not a tag; the only environment variable is `PORT=8080`; `runAsNonRoot: true`, `seccompProfile: RuntimeDefault`, all capabilities dropped, `allowPrivilegeEscalation: false`; a TCP readiness probe on 8080; requests and limits present. Then:

```bash
kubectl -n app-hello-<suffix> get ingress -o yaml | grep -A3 tls     # expect no tls block
kubectl -n app-hello-<suffix> get secret                            # expect the pull secret only
```

## 6b. Nothing is left behind (AC-22, AC-12)

After a completed deploy:

```bash
kubectl -n deployer-builds get secret            # expect none once the Job is reaped
kubectl -n deployer-builds get jobs              # expect none past the ttl
kubectl -n deployer-system exec deploy/deployer -- ls /data/uploads   # expect empty
```

Then confirm the app pod cannot read its own pull secret:

```bash
kubectl -n app-hello-<suffix> get pod -o yaml | grep -c 'regcred'   # expect 1, the imagePullSecrets line only, no volume and no env
```

## 7. A root image is refused with a cause, not a stack trace (AC-10)

Deploy a source tree whose build produces a root running image (easiest: a project with a `Dockerfile` is not it, since slice 1 ignores Dockerfiles, so force it by pushing a root image to the registry under the app's repo and pointing a deployment at it by hand, or wait for slice 6).

Expect the deployment to fail with `image_runs_as_root`, and:

```bash
kubectl get ns app-<that slug> -o name    # namespace may exist
kubectl -n app-<that slug> get deploy     # expect nothing created
```

## 8. Restart mid build resolves (AC-18)

Start a deploy, and while the build Job is running:

```bash
kubectl -n deployer-system rollout restart deploy/deployer
```

The deploy either completes or fails with a reason. Confirm no row is left non terminal:

```bash
kubectl -n deployer-system exec deploy/deployer -- sqlite3 /data/deployer.db \
  "select id,state from deployments where state not in ('healthy','failed','cancelled');"
```

Expect no rows once the platform has settled. Then repeat, deleting the build Job by hand during the restart, and confirm the deployment ends `failed` with a reason rather than hanging.

## 9. Timeouts fire where they should (AC-17)

Set `DEPLOYER_READY_TIMEOUT_SECONDS=5`, deploy an app that listens on the wrong port, and confirm the call fails with `app_never_ready` at roughly five seconds rather than at the overall budget.

Set `DEPLOYER_BUILD_TIMEOUT_SECONDS=soon` and confirm the pod fails to boot with an error naming that variable.

## Recorded outside this repo

Nothing new. This feature adds no state to pfSense, Cloudflare, or the Tailscale admin console; spec 0003's `verify.md` still owns all of that. The one thing added elsewhere is the registry htpasswd credential, sealed into this repo's `deploy/`, and the `DEPLOYER_BOOTSTRAP_TOKEN` value, which lives in the same platform `SealedSecret` and in your password manager.

---

# Verify: first deploy end to end, milestones 1 to 3 · spec 0004 · updated 2026-08-12

_Steps derived from spec 0004 acceptance criteria and its Value sourcing table, for the part of the build that has landed: the registry and build namespace, configuration and bootstrap seeding, the upload endpoint, the source extractor, the build Job composition, and the registry client. `/check verify` runs these; `/test` locks the durable ones. The sections above still own the whole pipe, which is not connected yet._

## Commands

- [x] `go test -race ./...` → every package passes → AC-1, AC-2, AC-8, AC-9, AC-10, AC-17
- [x] `go test -race ./internal/source/` → every hostile archive (absolute path, `..`, symlink, hardlink, device, fifo, too many entries, gzip bomb, truncated) is rejected and nothing lands outside the extraction root → AC-8
- [x] `go test -race ./internal/build/` → the composed Job is one attempt, non root, all capabilities dropped, `RuntimeDefault` seccomp, no service account token, and pushes to the platform's chosen tag → AC-7, AC-9
- [x] `go test -race ./internal/store/ -run Bootstrap` → seeding three times leaves one account and one live token, and rotating the token revokes the old one → AC-1
- [x] `gofmt -l . && go vet ./... && golangci-lint run` → clean

## Value sourcing, one step per row that these milestones cover

Each varies the input and checks the output changes with it, so a value taken from the wrong source fails here even when the shape is right.

- [ ] Bootstrap seed: run the platform twice with the same `DEPLOYER_BOOTSTRAP_TOKEN`, then once with a different one. Expect one `accounts` row named `bootstrap` throughout, and exactly one live `api_tokens` row, holding the SHA-256 of whichever token is current → AC-1. **Half done 2026-08-12**: same token across a restart leaves one account and one token, and `sha256(raw token)` equals the stored `token_hash`. Rotating to a different token is still owed
- [x] Bootstrap seed, secrecy: `kubectl -n deployer-system logs deploy/deployer | grep "$DEPLOYER_BOOTSTRAP_TOKEN"` → no match, at any log level → AC-1
- [x] Upload `path`: post two uploads and read `uploads.path`. Expect `DEPLOYER_UPLOAD_DIR` joined with each row's own `upl_` id, never anything from the request → AC-2
- [x] Upload `sha256` and `size_bytes`: post a body, then `sha256sum` the same bytes locally. Expect the stored hash and size to match what you sent, computed by the platform rather than declared by the client → AC-2
- [x] Upload `expires_at`: post an upload and expect `expires_at` to be exactly one hour after `created_at`, not a fixed clock time → AC-2
- [x] Upload `fetch_token_hash` at upload time: post an upload, then try `GET /v1/uploads/{id}` with any token you can construct. Expect 401: the seeded hash unlocks nothing, because its input was discarded unread → AC-2
- [ ] Fetch token minted at build time: mint, fetch (200), fetch again (409), mint again, fetch (200), then retry the first token (401). Proves a resumed build gets a working token and a leaked one stays dead → AC-8. **Half done 2026-08-12**: the build's own token fetched once (the build succeeded) and replaying that spent token returned 409. Minting a second token for the same upload is still owed
- [x] Expected `sha256` passed to the build: read the composed Job's init container env and expect `DEPLOYER_EXPECTED_SHA256` to equal `uploads.sha256` for that upload, not anything the archive carries → AC-8
- [x] Build Job name: expect the deployment id in the RFC 1123 form an object name has to take, `build-dep-<lowercased ULID>`, so a row read off disk after a restart finds its own Job → AC-18. The raw id carries an underscore and uppercase letters and the API server refuses both
- [x] Build target image: expect `DEPLOYER_REGISTRY_HOST + "/apps/" + slug + ":" + deployment id`, with the slug the platform derived and no part of the app name the caller sent → AC-9
- [x] Builder and init images: change `DEPLOYER_BUILDER_IMAGE` to a mutable tag and expect the boot to fail naming that variable. Same for `DEPLOYER_SELF_IMAGE` → AC-17
- [x] Build result digest: push a tag by hand, resolve it with the registry client, and expect the digest the registry reports rather than anything the build container said → AC-9
- [ ] Image user: push one image with `USER 1000` and one with no `USER`. Expect the first to read as non root and the second to read as root → AC-10

## Cluster steps still owed for these milestones

- [x] Registry (AC-20): `kubectl -n deployer-system get pod -l app=deployer-registry` is `Running`, and from a throwaway pod in the cluster a `docker` or `crane` push and pull of a small image both succeed with the sealed credential. Confirm there is no Ingress for it: `kubectl get ingress -A | grep registry` returns nothing. Push and pull were proved by the real thing rather than by hand: three builds pushed to it and the kubelet pulled the result by digest. A throwaway pod with no credential gets 401 on `/v2/`
- [x] Build namespace (AC-7): `kubectl get ns deployer-builds -o jsonpath='{.metadata.labels}'` shows `pod-security.kubernetes.io/enforce: restricted`
- [x] Whether the Buildpacks lifecycle actually runs under `restricted` is not proven until a real build runs there. If it does not fit, that is a finding and a spec update, never a right granted in advance
- [x] Insecure registry on every node: `/etc/rancher/k3s/registries.yaml` names the registry on all four nodes, and k3s has been restarted on each. Missing it on one worker makes deploys succeed or fail by where the pod is scheduled

## Run record, `/check verify` 2026-08-12

The whole pipe ran against the real cluster. `deploy_app` was called over the live MCP endpoint with a
freshly uploaded `testdata/sample-go`, and returned release 3 on the same hostname the first two
deploys got. The app answers 200 on `https://hello-4dfssb.deploy.toyintest.org`.

Met, with the evidence that proved it:

| AC | How it was proved |
|---|---|
| AC-1 | one `accounts` row and one live `api_tokens` row after a restart; `sha256(raw token)` equals the stored hash; the raw token appears zero times in the full pod log |
| AC-2 | 401 with no token and with a bad one, 413 on a 120 MiB body, 400 on a non gzip body, 201 returning only `upload_id` and `expires_at`; stored `size_bytes` and `sha256` match the bytes sent, `path` is the upload dir joined with the row's own id, `expires_at` is one hour after `created_at`; every denial wrote one `audit_log` row |
| AC-3 | `tools/list` returns exactly one tool, `deploy_app`, with `name` and `upload_id` both required and a description carrying the upload step, the endpoint, and the minutes long warning; an unknown `upload_id` returns `upload_invalid` and creates no deployment |
| AC-4 | three deploys of `hello` all resolved to slug `hello-4dfssb` |
| AC-5 | `deployment_events` for each healthy deploy holds exactly `queued`, `building`, `pushing`, `deploying`, `healthy`, in order, once each |
| AC-6 | only one build Job was ever in `Running` while the others sat `Complete` |
| AC-7 | the Job carries `backoffLimit: 0`, `activeDeadlineSeconds: 480`, `ttlSecondsAfterFinished: 3600`, `automountServiceAccountToken: false`, `runAsNonRoot` as uid 1001, all capabilities dropped, `RuntimeDefault` seccomp, in a namespace labelled `enforce: restricted` |
| AC-8 | the init container's `DEPLOYER_EXPECTED_SHA256` equals that upload's stored `sha256`; a forged fetch token gets 401; replaying the build's own spent token gets 409 |
| AC-9 | the build ran the pinned Paketo builder by digest and pushed to `<registry>/apps/hello-4dfssb:<deployment id>`; the platform recorded the digest the registry reported |
| AC-11 | `app-hello-4dfssb` carries the ownership and restricted pod security labels and holds the ResourceQuota and LimitRange from the template |
| AC-12 | one `kubernetes.io/dockerconfigjson` secret in the app namespace, referenced once from `imagePullSecrets` and nowhere else: no volume, no env |
| AC-13 | the container image is an `@sha256:` reference, `PORT=8080` is the only environment variable, the security context matches, the Service is `app` on 80 targeting 8080, the Ingress carries no `tls` block; the third deploy updated the Deployment in place rather than recreating it |
| AC-14 | TCP readiness probe on 8080; three `releases` rows numbered 1, 2, 3 with `apps.current_release_id` pointing at the newest |
| AC-15 | the tool returned all six fields: name, slug, url, deployment id, release number, image digest |
| AC-16 | `upload_invalid`, `build_failed` and `app_never_ready` were all produced live, each one short and sanitized; no build output reached the response, `failure_reason`, or the log at info |
| AC-17 | the validation half: `DEPLOYER_BUILD_TIMEOUT_SECONDS=soon` fails the boot naming that variable, and a mutable tag on `DEPLOYER_BUILDER_IMAGE` or `DEPLOYER_SELF_IMAGE` is refused naming the variable. `app_never_ready` fired live on an earlier deploy |
| AC-19 | one `audit_log` row per `deploy_app` call, with the app as target; denials carry a null account |
| AC-20 | the registry pod is `Running` on a Longhorn volume with an htpasswd `SealedSecret`, has no Ingress, and refuses an anonymous `/v2/` with 401; `registries.yaml` names it on all four nodes |
| AC-21 | three deploys, same hostname, release 3, HTTP 200 from a tailnet device |
| AC-22 | `/data/uploads` is empty after every deploy reached a terminal state |

Still owed, and why:

- **AC-10, a root image is refused**: never exercised against the real cluster. It needs a root running
  image pushed to the registry under an app's repo and a deploy pointed at it. Only the fake clientset
  test covers it today, so the real refusal path is unproven.
- **AC-17, the timeouts firing**: `app_never_ready` fired live, but the build phase timeout and the
  overall deploy timeout have never been watched fire. Both need config changes ArgoCD would revert.
- **AC-18, a restart during a live build**: the startup sweep does leave no non terminal row (the
  control plane restarted while deployments existed, and none was left hanging), but a restart while a
  build Job is actually running, and the delete the Job by hand variant, were not run.
