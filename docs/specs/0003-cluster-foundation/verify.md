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
3. The tailnet entry path, which is three separate things and all of them live outside this repository:
   - pfSense advertises `172.16.70.40/32` onto the tailnet, alongside the `172.16.60.0/24` route it already carries.
   - The route is approved in the Tailscale admin console, under the pfSense machine.
   - The tailnet policy file carries the grant `{"action":"accept","src":["group:prod"],"dst":["172.16.70.40/32:443"]}`. `group:prod` already exists in your policy file and contains your own account.
   - pfSense's own firewall has a pass rule on the `tailscale0` interface to `172.16.70.40:443`, next to the one already there for the camera VLAN. The tailnet ACL and the pfSense firewall are two separate gates and both must allow it. Skipping this one produces a timeout that looks identical to AC-12 passing.

   Then set a wildcard DNS record `*.$DOMAIN` and an apex record `$DOMAIN`, both A records to `172.16.70.40`, both **DNS only** in Cloudflare. A proxied record cannot reach a private address and would defeat the boundary. The apex needs the record because the certificate covers the bare domain too.

   Do not expose the controller Service through the Tailscale operator. It was tried on 2026-08-11 and does not work on this cluster; the reason is in [rationale.md](rationale.md).
4. The ingress-nginx `--default-ssl-certificate` flag, or its Helm equivalent, pointed at the wildcard secret. This restarts the controller and briefly interrupts TLS for the twelve apps already behind it.

## Routing, DNS, and TLS

```bash
# AC-10: the wildcard resolves to the ingress address, and the route is live
dig +short "hello.$DOMAIN"          # expect 172.16.70.40
tailscale status --json | grep -A3 pfSense    # expect 172.16.70.40/32 among its routes
# from a tailnet device that is NOT on the LAN, or the route is not what you are testing
curl -sS -o /dev/null -w '%{http_code}\n' -H "Host: nothing.$DOMAIN" http://172.16.70.40/
# expect 404 from nginx: the hop works, the hostname just has no Ingress yet

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

```bash
SA=system:serviceaccount:deployer-system:deployer

# AC-3: the rights it must have
kubectl auth can-i create namespaces           --as="$SA"    # yes
kubectl auth can-i create deployments          --as="$SA" -n app-hello-x   # yes
kubectl auth can-i get pods/log                --as="$SA" -n app-hello-x   # yes

# AC-3: the rights it must NOT have
kubectl auth can-i get nodes                   --as="$SA"    # no
kubectl auth can-i create clusterrolebindings  --as="$SA"    # no
kubectl auth can-i list secrets                --as="$SA" -n kube-system   # no
kubectl auth can-i create customresourcedefinitions --as="$SA"  # no
```

Every line in the first group must print `yes` and every line in the second must print `no`. A `yes` in the second group is a blocking failure, not a note.

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
curl -sS "https://deployer.<tailnet>.ts.net/healthz"    # expect 200
curl -sS -o /dev/null -w '%{http_code}\n' "https://deployer.$DOMAIN/healthz"
# expect 404 from nginx: the platform is not on the app wildcard

# AC-13: and nothing routes the platform onto the app facing controller at all
kubectl get ingress -A -o json \
  | jq -r '.items[] | select(.spec.ingressClassName=="nginx") | "\(.metadata.namespace)/\(.metadata.name)"'
# expect no deployer-system entry: the isolation rests on the platform having no
# nginx class Ingress, so check that rather than assuming it
```

**AC-16** is covered by the unit tests in `internal/config`, not by a cluster check. Run `go test ./internal/config/...` and confirm a case exists for each new setting being missing and being malformed.

## GitOps

```bash
# AC-14: no plain token anywhere
grep -rn "CLOUDFLARE_API_TOKEN\|cloudflare.*token" k3sprox-gitops/ | grep -v SealedSecret
# expect no matches

# AC-15: ArgoCD cannot see or prune a runtime created namespace
argocd app get deployer -o json | jq '.spec.destination, .spec.syncPolicy'
argocd app resources deployer | grep -c "app-hello"    # expect 0
argocd app sync deployer
kubectl get ns "app-hello-<suffix>"                    # still exists after the sync
```

The last two lines are the important ones. A sync that removes a running app namespace is the failure this criterion exists to catch.
