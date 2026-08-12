# 0003. Cluster foundation, verify steps

Run these against the real cluster, context `k3sprox-operator.tail62ceef.ts.net`. Each step names the acceptance criterion it proves. `$DOMAIN` is the value of `DEPLOYER_APP_DOMAIN`.

## One time setup this feature depends on

These are done by a human, not by code, and they are the part nothing in the repository can tell you about.

1. cert-manager's cluster resource namespace, which is where a ClusterIssuer resolves its `secretRef` and is an install time flag rather than always `cert-manager`. Read it, do not assume it:

   ```bash
   kubectl get deploy -A -l app.kubernetes.io/name=cert-manager \
     -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.spec.template.spec.containers[0].args}{"\n"}{end}'
   # look for --cluster-resource-namespace; absent means it defaults to cert-manager's own namespace
   ```

2. A Cloudflare API token scoped to `Zone.DNS: Edit` on the one zone, sealed with `kubeseal` into that namespace and committed to `k3sprox-gitops`.
3. The tailnet entry path, which is four separate gates and all of them live outside this repository:
   - pfSense advertises `172.16.70.40/32` onto the tailnet, alongside the `172.16.60.0/24` route it already carries.
   - The route is approved in the Tailscale admin console, under the pfSense machine.
   - The tailnet policy file carries `{"action":"accept","src":["group:prod"],"dst":["172.16.70.40/32:443"]}` in its `acls` list. It goes in `acls`, not `grants`: an object with an `action` key is the older ACL form, and the `grants` block takes `app` capabilities instead, so putting this there fails to parse. `group:prod` already exists in your policy file and contains your own account.
   - pfSense's own firewall has a pass rule on the `tailscale0` interface to `172.16.70.40:443`, next to the one already there for the camera VLAN. The tailnet ACL and the pfSense firewall are two separate gates and both must allow it. Skipping this one produces a timeout that looks identical to AC-12 passing.

   Then set a wildcard DNS record `*.$DOMAIN` and an apex record `$DOMAIN`, both A records to `172.16.70.40`, both **DNS only** in Cloudflare. A proxied record cannot reach a private address and would defeat the boundary. The apex needs the record because the certificate covers the bare domain too.

   Do not expose the controller Service through the Tailscale operator. It was tried on 2026-08-11 and does not work on this cluster; the reason is in [rationale.md](rationale.md).
4. The ingress-nginx `--default-ssl-certificate` flag, or its Helm equivalent, pointed at the wildcard secret. This restarts the controller and briefly interrupts TLS for the twelve apps already behind it.

## Routing, DNS, and TLS

```bash
# AC-10: the wildcard resolves to the ingress address, and the route is live
dig +short "hello.$DOMAIN"          # expect 172.16.70.40
# PrimaryRoutes lists approved routes only, so this proves advertised AND approved
# in one shot. 2>/dev/null drops the client/server version warning, which otherwise
# lands on stdout and breaks the JSON parse.
tailscale status --json 2>/dev/null \
  | jq -r '.Peer[] | select(.HostName|ascii_downcase=="pfsense") | .PrimaryRoutes[]'
# expect 172.16.70.40/32 in the list
# from a tailnet device that is NOT on the LAN, or the route is not what you are testing
curl -sSk -o /dev/null -w '%{http_code}\n' -H "Host: nothing.$DOMAIN" https://172.16.70.40/
# expect 404 from nginx: the hop works, the hostname just has no Ingress yet
#
# 443 and -k are both deliberate. Port 80 is not granted by the tailnet ACL or the
# pfSense rule, so an http:// probe here times out by design and reads as a broken
# path. -k because this step tests routing only: run before the wildcard is issued,
# nginx answers with its own self signed default and validation would fail for a
# reason that has nothing to do with AC-10.
#
# and confirm the closed port really is closed, which is part of the same design
curl -sS -m 5 -o /dev/null -w '%{http_code}\n' -H "Host: nothing.$DOMAIN" http://172.16.70.40/
# expect a timeout, exit 28: a typed http:// URL must not redirect, it must fail

# AC-8: the wildcard certificate is issued and Ready by the production issuer
kubectl -n ingress-nginx get certificate
kubectl -n ingress-nginx describe certificate wildcard-apps | grep -E 'Issuer|Status|Not After'

# AC-9: nginx serves the wildcard for a host that has no Ingress at all
echo | openssl s_client -connect 172.16.70.40:443 \
  -servername "nothing-here.$DOMAIN" 2>/dev/null | openssl x509 -noout -subject -issuer
# expect subject CN *.$DOMAIN, issuer Let's Encrypt

# AC-11: the hello world answers over HTTPS with a trusted certificate,
# run this from a tailnet device that is NOT on the LAN
curl -sS -o /dev/null -w '%{http_code} %{ssl_verify_result}\n' "https://hello-<suffix>.$DOMAIN"
# expect: 200 0
```

**AC-11 must be run from off the LAN.** Run from your desk it proves ordinary LAN routing and says nothing about the tailnet path, which is the part that is new and the part that can break. A phone on mobile data with Tailscale **on** is the right device.

**AC-12** cannot be checked from the cluster, the LAN, or the tailnet. From a phone on mobile data with Tailscale **off**:

```bash
dig +short "hello-<suffix>.$DOMAIN"                 # resolves, 172.16.70.40
curl -m 10 "https://hello-<suffix>.$DOMAIN"          # expect a connection timeout, not a 4xx or 5xx
```

**Prove the path is up before you trust this timeout.** A withdrawn route, a pfSense outage, or the missing firewall rule all produce the same timeout as a correctly enforced boundary, so a broken path reads as a pass. Immediately before or after, from a tailnet device off the LAN, confirm AC-11 still returns 200. A timeout in one place and a 200 in the other is the real pass; timeouts in both mean the path is broken, not that the boundary works.

A timeout is the pass. Any HTTP response at all means the app is reachable from the public internet, which is what this criterion exists to catch. Reaching it from your own LAN without Tailscale is expected and is not a failure.

## Namespaces and admission

```bash
# AC-5: the app namespace carries its labels, pod security, quota, and limit range
kubectl get ns "app-hello-<suffix>" -o jsonpath='{.metadata.labels}' | tr ',' '\n'
kubectl -n "app-hello-<suffix>" get resourcequota,limitrange -o wide

# AC-6: privileged, root, and bare pods are all refused by the API server
kubectl -n "app-hello-<suffix>" run bad --image=nginx --privileged --dry-run=server
kubectl -n "app-hello-<suffix>" run bad --image=nginx \
  --overrides='{"spec":{"containers":[{"name":"bad","image":"nginx","securityContext":{"runAsUser":0}}]}}' \
  --dry-run=server
kubectl -n "app-hello-<suffix>" run bare --image=nginx --dry-run=server
# all three expect: violates PodSecurity "restricted:latest"
# the third is the important one: a pod that simply says nothing is refused too

# AC-7: a container declaring no resources is admitted and gets the LimitRange defaults
kubectl -n "app-hello-<suffix>" get pod -l app=hello \
  -o jsonpath='{.items[0].spec.containers[0].resources}'
# expect the LimitRange defaults, not an empty object

# AC-7: an over quota workload is refused
kubectl -n "app-hello-<suffix>" create deployment fat --image=nginx
kubectl -n "app-hello-<suffix>" set resources deployment/fat --limits=cpu=2,memory=4Gi
kubectl -n "app-hello-<suffix>" get events --field-selector reason=FailedCreate
# expect: exceeded quota
```

## Service account rights

**`kubectl auth can-i --as=...` does not work here, and it fails in the direction that hides a problem.** The Tailscale operator proxy replaces every client identity with its own, which is in `system:masters`, so every probe returns `yes`, including one for a service account that does not exist. It reads as a clean pass on the first group and a blocking failure on the second, when in fact it measured nothing. Check the bogus account first if you ever doubt it:

```bash
kubectl auth can-i get nodes --as=system:serviceaccount:deployer-system:nonexistent
# prints yes, which is how you know the whole method is unusable on this path
```

Ask the API server from inside the cluster instead, as the real account, using its mounted token:

```bash
kubectl -n deployer-system run ac3 --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 \
  --overrides='{"spec":{"serviceAccountName":"deployer","securityContext":{"runAsNonRoot":true,"runAsUser":100,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"ac3","image":"curlimages/curl:8.11.1","stdin":true,"tty":false,"command":["sh"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}' <<'SH'
A=https://kubernetes.default.svc
T=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
C=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
q() { curl -s -o /tmp/o -w "%{http_code}\n" --cacert $C -H "Authorization: Bearer $T" -H 'Content-Type: application/json' "$@"; }

# must be refused
q $A/api/v1/namespaces/kube-system/secrets                      # 403
q $A/apis/apps/v1/namespaces/kube-system/deployments            # 403
q $A/api/v1/nodes                                               # 403
q $A/apis/apiextensions.k8s.io/v1/customresourcedefinitions     # 403
q -XPOST -d '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-evil"}}' \
     $A/api/v1/namespaces                                       # 422, admission policy
q -XDELETE $A/api/v1/namespaces/argocd                          # 422, admission policy

# must be allowed
q -XPOST -d '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"app-ac3-test"}}' \
     $A/api/v1/namespaces                                       # 201
q -XDELETE $A/api/v1/namespaces/app-ac3-test                    # 200
SH
```

A `403` or `422` in the first group and a `2xx` in the second is the pass. Anything the other way round is a blocking failure, not a note.

Two of these test the ValidatingAdmissionPolicy rather than RBAC, and that is the point. RBAC cannot fence `namespaces` or `rolebindings` by name, so the policy in `deploy/admission-policy.yaml` is the only thing standing between the control plane and deleting `kube-system`. Its denial message names the rule, so read the body of a `422`, not just the code.

**The control plane holds no rights in an app namespace until it binds them.** `ClusterRole/deployer-app` is bound nowhere by default; the platform creates a RoleBinding to it in each namespace it makes. So `create deployments -n app-<slug>` is correctly a **no** in a namespace that has no such binding, and that is not a failure. Create the binding first, then re-ask.

## Control plane

```bash
# AC-1, AC-2: one replica, Recreate, restricted namespace
kubectl get ns deployer-system -o jsonpath='{.metadata.labels}'
kubectl -n deployer-system get deploy deployer \
  -o jsonpath='{.spec.replicas}{" "}{.spec.strategy.type}{"\n"}'    # expect: 1 Recreate
kubectl -n deployer-system get deploy deployer \
  -o jsonpath='{.spec.template.spec.containers[0].securityContext}'
# expect runAsNonRoot true, readOnlyRootFilesystem true, capabilities drop ALL

# AC-2: the volume is mounted and writable by the non root user
kubectl -n deployer-system get pvc
kubectl -n deployer-system get deploy deployer \
  -o jsonpath='{.spec.template.spec.securityContext.fsGroup}{"\n"}'   # expect: 65532
kubectl -n deployer-system logs deploy/deployer | grep -i migrat
# a permission denied on /data here means fsGroup is missing, not that the PVC failed

# AC-4: readiness reflects the database, liveness does not
kubectl -n deployer-system get pod -l app=deployer \
  -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}'   # expect True

# AC-13: the platform answers on its own tailnet name, not on the app wildcard
curl -sS -o /dev/null -w '%{http_code}\n' "https://deployer.<tailnet>.ts.net/readyz"  # expect 200
curl -sS -o /dev/null -w '%{http_code}\n' "https://deployer.$DOMAIN/readyz"
# expect 404 from nginx: the platform is not on the app wildcard
#
# Use /readyz, not /healthz. ingress-nginx's default server answers /healthz with
# 200 for ANY host, so the second line returns 200 whatever is or is not deployed
# and the check can never fail. Confirm it yourself once:
#   curl -sSk -o /dev/null -w '%{http_code}\n' "https://nothing.$DOMAIN/healthz"   # 200
#   curl -sSk -o /dev/null -w '%{http_code}\n' "https://nothing.$DOMAIN/"          # 404

# AC-13: and nothing routes the platform onto the app facing controller at all
kubectl get ingress -A -o json \
  | jq -r '.items[] | select(.spec.ingressClassName=="nginx") | "\(.metadata.namespace)/\(.metadata.name)"'
# expect no deployer-system entry: the isolation rests on the platform having no
# nginx class Ingress, so check that rather than assuming it
```

**AC-16** is covered by the unit tests in `internal/config`, not by a cluster check. Run `go test ./internal/config/...` and confirm a case exists for each new setting being missing and being malformed.

## GitOps

```bash
# AC-14: no plain token anywhere.
# Assert on the value, not on nearby words. A sealed value sits three lines
# below the word SealedSecret, so filtering by that word leaves the ciphertext
# line matching and the check fails on a correctly sealed repository.

# no plain Secret manifest exists at all
grep -rln '^kind: Secret$' --include='*.yaml' k3sprox-gitops/
# expect no output

# and every token value in the repository is sealed-secrets ciphertext,
# which always begins Ag. A raw Cloudflare token is 40 characters of
# base62 and would not match, so a non zero count is a blocking failure.
grep -rhE '^[[:space:]]+[A-Za-z_-]*[Tt]oken:' --include='*.yaml' k3sprox-gitops/ \
  | awk '{print $2}' | grep -vc '^Ag'
# expect 0

# AC-15: ArgoCD cannot see or prune a runtime created namespace
argocd app get deployer -o json | jq '.spec.destination, .spec.syncPolicy'
argocd app resources deployer | grep -c "app-hello"    # expect 0
argocd app sync deployer
kubectl get ns "app-hello-<suffix>"                    # still exists after the sync
```

The last two lines are the important ones. A sync that removes a running app namespace is the failure this criterion exists to catch.
