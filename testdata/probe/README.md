# probe

The app that proves the network fence from spec 0008 is real. It reports, over
HTTP, what it could and could not reach from inside an app namespace.

Deployed through the real `deploy_app` path like any other app, and deployed
**twice** under two slugs, so each instance is the other's sibling and the app to
app isolation has something to be isolated from.

```bash
# once per instance, giving each a different name
curl -sS -X POST "$DEPLOYER_PUBLIC_URL/v1/uploads" \
  -H "Authorization: Bearer $DEPLOYER_TOKEN" \
  --data-binary @- < <(cd testdata/probe && tar czf - .)
```

Then call `deploy_app` twice with the two upload ids.

Four destinations have no name the probe can work out, only an address, so they
are passed in. Read them off the cluster first:

```bash
kubectl -n app-<other-slug> get pod -o wide          # sibling pod IP, port 8080
kubectl -n app-<other-slug> get svc app              # sibling Service IP, port 80
kubectl get node -o wide                             # a node IP, port 6443
kubectl -n ingress-nginx get svc                     # the load balancer address, port 443
```

```bash
curl -sS "https://<slug>.$DEPLOYER_APP_DOMAIN/probe?\
sibling_pod=10.42.1.7:8080&sibling_service=10.43.2.9:80&\
node=172.16.70.11:6443&load_balancer=172.16.70.20:443" | jq
```

Every row is `{target, address, outcome, ms}`, where `outcome` is one of
`reached`, `refused`, `timeout`, `dns_failed`. Under the fence the only row that
should read `reached` is `public_host`; everything else should be `timeout`,
because a destination a NetworkPolicy drops says nothing at all rather than
refusing. Each dial carries its own three second timeout for that reason: an
untimed dial to a fenced address never returns.
