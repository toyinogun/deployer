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
