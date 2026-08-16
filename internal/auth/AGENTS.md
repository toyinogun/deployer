# internal/auth

The one place a presented credential becomes an account, and the one place the
visitor behind a request gets a name. Two routes reach it, a bearer token from a
machine and a session cookie from a browser, and both are resolved here so that
neither surface can apply a different rule. Governing specs:
[0002](../../docs/specs/0002-platform-data-model/index.md) and
[0004](../../docs/specs/0004-first-deploy-end-to-end/index.md) for the token
route, [0007](../../docs/specs/0007-accounts-tokens-app-ownership/index.md) for
sessions and the audit actions,
[0018](../../docs/specs/0018-account-suspension/index.md) for suspension, and
[0021](../../docs/specs/0021-public-edge/index.md) for the cookie name and the
visitor's address.

It imports no store. It declares the narrow interfaces it needs (`Store`,
`SessionStore`, `TokenToucher`, `Auditor`) and `internal/store` satisfies them
through an adapter in that package. It does import `net/http`, because reading a
cookie and a header is the job, but only to read a request, never to serve one.

## Files

- `auth.go`: the `Account` and `Token` values, the `Store` interface, `HashToken`
  and `TokenPrefix`, and `Bootstrap`.
- `authenticate.go`: the bearer route (`Authenticate`, `BearerToken`), the closed
  set of audit action constants, the `Audit` value, and `Record`.
- `session.go`: the browser route (`AuthenticateSession`, `SessionID`), the two
  cookie names and `SessionCookieName`, and `usable`, the verified gate both
  routes share.
- `clientaddr.go`: `ClientAddress`, the single derivation of who a rate limit
  bucket and an audit row belong to.

## What holds here

- **Two routes, one gate.** `usable` is where the verified check and the disabled
  check live, and both `Authenticate` and `AuthenticateSession` call it. A new
  surface cannot forget either rule because there is nowhere else to resolve a
  caller. Adding a gate to a handler instead of here is the mistake this shape
  exists to prevent, and root [AGENTS.md](../../AGENTS.md) records what it cost
  the last time a refusal lived in a handler.
- **Sameness is the feature.** `ErrTokenInvalid` covers unknown, revoked,
  expired, and held by an account that never confirmed its address, and they are
  deliberately indistinguishable: a caller learns only that the token does not
  work. `ErrSessionInvalid` is the same shape for sessions. Do not split one of
  these out to be helpful.
- **Two deliberate exceptions to that, both narrow.** `ErrAccountSuspended` is
  told apart because the caller already holds that account's credential, so it
  learns only about itself, and `Authenticate` returns the account alongside the
  error, which nothing else here does, because a surface refusing a suspended
  caller still has to audit which account it refused. `ErrEmailUnverified` exists
  on the session route only: a person can act on it by verifying, and a machine
  holding a token gets no such hint.
- **The session cookie's write and read pick the same name.** `SessionCookieName`
  takes `secure` and both sides go through it. `SessionID` deliberately takes
  `secure` rather than trying both names, and reintroducing a fallback reader is
  the thing to never do: `SessionCookiePlain` carries no `__Host-` prefix, so
  unlike the name the platform writes it can be set with a `Domain`, and an app
  deployed by a stranger on a sibling hostname could scope one to the parent
  domain and have the console read it. That was live until 2026-08-16, and every
  test that only checked the write still passed throughout. `IsSessionCookie`
  exists for callers inspecting a `Set-Cookie` without holding the scheme, which
  is tests, and is not a licence to read under both names.
- **`ClientAddress` is one function on purpose.** Both surfaces that rate limit
  call it, because a limit one of them spends from a different bucket is not a
  limit. A second copy is how that property is lost.
- **`CF-Connecting-IP` is read on the platform's public hostnames and nowhere
  else**, and what makes that safe is the path rather than the header: each
  public origin is the control plane Service directly, so network policy can name
  the only pods that may reach it. Move one behind the shared ingress controller
  and the header becomes writable from most of the cluster, with nothing in this
  package or the suite noticing. Since spec 0022 that set is two names, the
  console and the deploy host, so `ClientAddress` takes the trusted hosts as a
  variadic set rather than a single host (AC-13). Every surface passes the same
  set: the upload route, the MCP endpoint and the pages derive one address per
  visitor and spend from one bucket rather than three (AC-14).
- **Both shapes of "more than one value" are refused**, and the two checks are
  not redundant. Repeated headers arrive as several entries from `Values`, and
  one header carrying `a, b` arrives as a single entry with a comma in it,
  because `Values` does not split on commas. `requestHost` cuts an IPv6 literal
  at its closing bracket rather than at the first colon, for the same kind of
  reason.
- **Auditing never changes what a caller is told.** `Record` returns nothing and
  logs a failure. The action names are constants because the log is only useful
  if the same event is named the same way every time, and the set is closed:
  `ActionAdmin` carries which admin endpoint in its `Reason` rather than growing
  a constant per endpoint. `Audit.ClientAddress` is empty on a row the platform
  wrote itself, a suspension sweep, a reconcile drive, a scheduled backup, and
  that is stored as null.
- **A read is not an access decision.** Several actions record only refusals
  (`ActionStatus`, `ActionLogs`, `ActionAppList`, `ActionReleases`,
  `ActionConfigGet`, `ActionAppView`). The ones that record both outcomes did so
  because something changed or a value came back out: `ActionDeploy`,
  `ActionRollback`, `ActionAppDelete`, `ActionConfigReveal`, `ActionLogin`.
  `ActionConnectorGrant` (spec 0024) is the newest member and records **only the
  success**, because a refused OAuth exchange identifies nobody. It is also the
  one action whose subject travels in `Reason`: the struct holds one target pair
  and the target is the token that was issued, so the client id goes in the
  reason field (AC-23).
- **`HashToken` is a plain SHA-256 and that is deliberate.** These are 256 bit
  random values the platform mints, not passwords a person chose, so there is no
  guessable space a work factor would defend. Passwords are argon2id and live in
  `internal/identity`. `TokenPrefix` returns a short prefix for a short token
  rather than panicking.
- **A recorded use never refuses a good credential.** `TouchToken` and
  `TouchSession` failures are logged and the caller is let through, because the
  credential was valid and the bookkeeping is not the answer.
- **`Bootstrap` is safe on every boot.** The same token twice changes nothing,
  and a changed token revokes the old row before minting the new one, so rotating
  the sealed secret leaves exactly one working credential rather than two. The raw
  value is never logged and never returned in an error, at any level.

## Tests

Pure Go with a hand written fake store in the package, no SQLite and no HTTP
server. `clientaddr_test.go` and `session_test.go` build requests directly, which
is the right level for both: a header derivation and a cookie name are decisions
this package makes on its own. What that cannot reach is whether the network
policy behind the `CF-Connecting-IP` gate is really in place, which belongs to
`/check verify` against the real cluster.

_Drafted by /audit from the area, worth a quick human pass._
