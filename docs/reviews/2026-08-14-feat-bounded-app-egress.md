# Review, feat/bounded-app-egress, 2026-08-14

**Reviewed by**: Claude Sonnet 5 (author on Claude Sonnet 5)
**Scope**: 27 files, branch vs `main` (merge base `559325a`)
**Verdict**: Approve

## Summary

Spec 0017 turns the app-allow NetworkPolicy's wide-open public egress rule into one bounded by the complement of a configured blocked-port list, and separately locks both build namespaces down to an allow list of ports 80/443. The Go side (`internal/config`, `internal/deploy`, `internal/reconcile`) is small, pure where it can be, and heavily table-tested, including a property test that walks the entire 65535-port space to prove the complement never double-covers or drops a port. The two places the spec calls out as load-bearing — the deploy path and `PolicySweep` composing the exact same `deploy.Input`, and the public egress rule staying a single peer — are each pinned by a dedicated test. `verify.md` records a genuine live baseline taken before the change shipped and a live proof after, including an honest correction to AC-13's wording once a live check showed the original claim was false under a narrowed override. `gofmt`, `go vet`, `golangci-lint`, and `go test -race` all pass clean on the full diff.

## Strengths

- `TestAllowedPortRangesCoverExactlyTheComplement` (`internal/deploy/portrange_test.go:102`) is a genuine property test over all 65535 ports, not just the literal sampled cases — exactly the kind of test that would catch an off-by-one the table cases happen to miss.
- `TestPolicySweepCarriesThePortBound` (`internal/reconcile/policyportsweep_test.go:18`) and the doc comment on `PolicySweep` (`internal/reconcile/policysweep.go:31`) directly guard the invariant AGENTS.md calls out: a bound added to the deploy path and forgotten in the sweep silently weakens existing app namespaces. Good that it's a test, not just prose.
- `publicEgressPorts` (`internal/deploy/networkpolicy.go:137`) takes `start` and `end` as fresh locals per loop iteration before taking their addresses, correctly avoiding the classic Go loop-variable-aliasing bug that would otherwise make every composed port entry point at the same (last) range.
- The `deploy_app` description test (`internal/mcp/egresscontract_test.go:13`) checks both that the required phrases are present and that no literal blocked-port number leaks in, which is exactly the AC-13 contract (names the shape, not the list).
- `verify.md` phase 0 records a real baseline (all four probe targets `reached`) before the control plane change shipped, which is what makes the later `timeout` readings attributable to the policy rather than to the ISP — and the AC-13 wording was actually revised after a live check falsified the stronger original claim, rather than the live result being quietly ignored.

## Test coverage

Coverage is thorough for a `TESTS = configured` project: the complement function is tested both by literal cases (adjacency, duplicates, both boundaries, one-port-wide ranges) and by the full-space property test; `internal/config` covers default, empty-means-default, override, sort/dedup, and every boot-refusal path (unparseable, out of range, empty complement, nothing usable); the composed `AllowPolicy` is pinned against the eight literal ranges from the spec (not against its own complement function, avoiding a test that would agree with a broken implementation); the single-peer invariant and the explicit-UDP-protocol invariant each have their own test; the retrofit path through `PolicySweep` is tested against the fake clientset; and the build-namespace YAML files are parsed and pinned to the two-port allow list. The one thing that cannot be unit tested — Cilium actually enforcing `endPort` on the live cluster's version — is explicitly called out as needing the live proof, and that live proof is recorded in `verify.md`. No gaps found.
