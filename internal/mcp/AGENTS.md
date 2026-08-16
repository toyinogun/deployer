# internal/mcp

The agent facing tool surface, served over the streamable HTTP transport beside
the upload endpoint. Every tool an agent can call lives here: `deploy_app`,
`deployment_status`, `get_logs`, `list_apps`, `delete_app`, `list_releases`,
`rollback_app`, and the three configuration tools.

The browser surface is [internal/web](../web) and the plain HTTP surface is
[internal/httpapi](../httpapi). This package answers on the tailnet name and, since
spec 0022, on the deploy host; it is 404 on the console hostname.

## Files

- `mcp.go`: `Server`, `Options`, the interfaces this package declares over the
  store and the cluster, and the tool registration.
- `middleware.go`: the transport level gate, the rate limit, and `address`.
- `apps.go`, `status.go`, `logs.go`, `releases.go`, `config.go`: the tools,
  grouped by what they act on.

## Conventions

- **The handler observes and never acts.** `deploy_app` resolves the upload,
  resolves or creates the app, writes a queued deployment, and returns without
  reading anything back. `rollback_app` is the same shape. `deployment_status`
  and `get_logs` are pure reads. Everything in between belongs to the reconcile
  loop, and a tool that waits for a result here is a tool holding an agent's
  connection open for the length of a build.
- **Authentication happens before the transport sees the request**, in
  `authenticate`, so an unauthenticated caller never reaches a tool and never
  learns which tools exist.
- **The rate limit is spent in the handler and the lockout is not.** The bucket
  bounds the call rate, so it is spent here. The penalty for a run of bad tokens
  judges the credential, so it lives inside `auth.Authenticator` and both routes
  on the deploy path inherit it. This handler holds no copy of it, deliberately:
  a refusal added here would be a rule the upload route does not apply
  (spec 0022, AC-15, AC-16). Same shape as the sign in rule in
  [internal/identity](../identity).
- **A suspended account is carried through to the protocol layer rather than
  refused at the transport.** It presented a working credential, so it is not a
  transport failure. `refuseSuspended` is registered on the per request server
  and answers every tool call with `account_suspended`, which means a tool added
  later inherits the refusal instead of having to remember it (spec 0018, AC-9).
- **A refusal is a `CallToolResult` carrying `IsError` with a nil Go error**, not
  an error return. An error out of a method handler is a protocol error, and an
  agent reads that as a broken connection rather than as a decision it should
  stop retrying and report.
- **Every failure a caller sees is one of the closed `domain.Reason` codes**,
  never a wrapped error string and never build output. Build output stays in the
  Job's pod logs.
- **`address` is the one derivation of who is calling**, over the same trusted
  host set the pages and the upload route hold, so one visitor is one address and
  spends from one bucket rather than three (spec 0022, AC-13, AC-14).
- **A tool's description is part of the contract.** `deploy_app`'s carries the
  upload endpoint and the ceiling, both derived from configuration rather than
  written as literals, so the text cannot drift from the platform. See the root
  [AGENTS.md](../../AGENTS.md) rule.
- **`New` falls back to a private limiter, and that fallback shares nothing.**
  Production passes one instance to both `mcp.New` and `httpapi.New`, so one
  caller's burst is one budget. A harness that omits `Options.Limiter` is bounded
  but holds no shared budget, so it proves nothing about AC-15; build the limiter
  and pass it, the way `deployPathHarness` in `lockout_test.go` does. The lockout
  is unaffected either way, since it lives in `auth.Authenticator`.

## Tests

Two kinds, and the difference matters more here than anywhere else in the repo.

Most files test a handler method directly. That never crosses the tool's argument
schema, so a schema that refuses a call before the handler runs would hand the
caller a validation string instead of a reason code and still pass. Anything the
closed reason codes promise therefore needs a test through a real client and
server session: `wire_test.go` holds that path as the `callOverTheWire` helper,
and each feature's own test file calls it, so the cases live beside the feature
rather than piling up in that one file.

`lockout_test.go` and `description_test.go` are spec 0022's; `redaction_test.go`
and `egresscontract_test.go` are the ones to read before changing what a tool
hands back.

_Drafted by /sync at the engineer's request, worth a quick human pass._
