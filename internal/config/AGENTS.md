# internal/config

Two jobs live here, and only the first one is what the package name suggests.
It loads and validates every `DEPLOYER_*` value at startup, and it holds the Go
tests that pin the static YAML in [deploy/](../../deploy) that no Go code ever
composes.

## Files

- `config.go`: the `Config` struct, one field per value with the spec that added
  it, and `Load`, which gathers every problem before it returns one error.
- `firstdeploy.go`, `identity.go`, `web.go`, `isolation.go`, `lifecycle.go`: the
  per spec loaders `Load` calls, each returning its own problems rather than
  failing on the first.
- `controlplanepolicy_test.go`, `nodepolicy_test.go`, `buildspolicy_test.go`,
  `buildsnamespace_test.go`, `blockeddrift_test.go`: the parse tests over
  `deploy/`, described below.
- The remaining `*_test.go` files cover the loaders beside them.

## Conventions

- Every `DEPLOYER_*` value is validated here at startup, never at first use. A
  quantity is parsed as a Kubernetes quantity, a namespace as a DNS1123 label, a
  CIDR as a real IPv4 range, so a bad deploy fails on its first boot rather than
  on its first request. Adding a key to `deploy/configmap.yaml` without adding it
  here means the pod reads nothing.
- `Load` collects problems into `missing` and `errs` and joins them into one
  error at the end. Do not return early on the first bad value: an operator
  fixing a misconfigured deploy one boot at a time is the failure this shape
  exists to prevent.
- A new spec's values get their own `loadX(getenv, *Config)` file rather than more
  body in `Load`. `Load` calls it and appends what it returns.
- Optional means optional on purpose, and each case says what unset costs.
  `DEPLOYER_RESEND_API_KEY` unset leaves mail sending answering `mail_unavailable`
  while the rest works, and `DEPLOYER_BOOTSTRAP_TOKEN` unset is a warning so a
  local run needs no secret. `DEPLOYER_CSRF_KEY` is the one the pod refuses to
  start without.

## The parse tests over deploy/

Several manifests in `deploy/` are applied by ArgoCD and never composed in Go, so
nothing at run time would notice a hand edit that opens one up. The tests here
read those files off disk by relative path, parse them into the real API types,
and pin their whole shape. They are the only thing standing behind those files.

- `controlplanepolicy_test.go` pins `deployer-system-networkpolicy.yaml`, the
  `networking.k8s.io/v1` pair: ingress only, the exact pod sourced peer groups,
  the exact ports, and no egress rule. The egress half is the load bearing one,
  since adding `Egress` to `policyTypes` denies the control plane its own path to
  the Kubernetes API server and takes the platform down.
- `nodepolicy_test.go` pins `deployer-system-cilium-networkpolicy.yaml`, the
  `cilium.io/v2` object carrying the node sourced peers as reserved identities. It
  reads into a minimal local struct rather than importing the Cilium module,
  which would add a dependency to `go.mod` for one test file. Keep that: a port
  there is a string where the v1 kind takes an int.
- `buildspolicy_test.go` and `buildsnamespace_test.go` do the same for the two
  build namespaces and their pod security levels.
- `blockeddrift_test.go` pins the blocked range list, which exists three times:
  as `defaultBlockedCIDRs` in `isolation.go` and as literal text in each build
  namespace's policy file. Editing one without the others is what this catches.

What none of them can tell you is that a peer is **missing**, because a shorter
policy is still a valid policy. Only a live walk on the cluster catches that,
which is why spec 0019's `verify.md` names its callers one at a time.

_Drafted by /sync at the engineer's request, worth a quick human pass._
