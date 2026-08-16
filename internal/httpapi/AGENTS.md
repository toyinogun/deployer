# internal/httpapi

The platform's plain HTTP surface. Two audiences share one package and they are
easy to confuse, so tell them apart before changing anything here.

- **Machines**, in `httpapi.go`: `POST /v1/uploads` and `GET /v1/uploads/{id}`.
  Authenticated by a bearer token, or by the upload's own single use fetch token.
  This is the path an agent and a build's init container use.
- **People**, in `identity.go` and the three `*routes.go` files: registration,
  sessions, an account's own API tokens, and the admin view, all under
  `/v1/auth`, `/v1/tokens` and `/v1/admin`. Authenticated by a session cookie. A
  bearer token never reaches these, because they read a cookie and a token is not
  one.

The MCP tool surface is `internal/mcp`, and the browser page surface is
`internal/web`. Governing specs:
[0007](../../docs/specs/0007-accounts-tokens-app-ownership/index.md),
[0015](../../docs/specs/0015-invite-only-registration/index.md),
[0018](../../docs/specs/0018-account-suspension/index.md) and
[0021](../../docs/specs/0021-public-edge/index.md).

## Files

- `httpapi.go`: `API`, the upload and fetch handlers, and the JSON write helpers.
- `identity.go`: `Identity`, the route table, the session and admin gates,
  `spend`, `decode`, `fail` and `statusFor`.
- `authroutes.go`: register, verify, resend, login, logout, forgot, reset, me.
- `tokenroutes.go`: the caller's own tokens, plus the admin account actions.
- `inviteroutes.go`: the admin invite list, mint and revoke.
- `oauth.go`: the two RFC 9728 protected resource documents, spec 0024.

## Conventions

- **Every route in this package answers 404 on the console hostname.** Since
  spec 0021 the platform serves two hostnames from one mux, and a route that
  changes cluster state is absent from the console host's mux rather than
  refused by a check inside the handler. Registration under the console host
  pattern is opt in, so a route added here is private by default and nobody has
  to remember to exclude it. That is the property to preserve: do not add a
  console host registration for a `/v1` route without reading AC-2 first.
- **The deploy host is a third pattern with the same opt in shape**, added by
  spec 0022. `API.Register` takes an `Options` carrying `MCPHost`, registers
  exactly `POST /v1/uploads` and `/mcp` a second time under it, and one catch all
  answers 404 for everything else on that hostname. The two properties to keep
  are that a route nobody registers there is absent by default (AC-2), and that
  **`GET /v1/uploads/{id}` is deliberately not among them** (AC-4): the single use
  fetch is what a build's init container reads over cluster DNS on
  `DEPLOYER_INTERNAL_URL`, and it has no reason to be on the open internet.
  `deployhost_test.go` pins both halves.
- **The two discovery documents are the only thing spec 0024 adds to the deploy
  hostname, and they carry no credential by design.** A caller that cannot
  authenticate is exactly the caller they are for (AC-1). There are two rather
  than one because each names its own exact resource and the client compares that
  against what the person typed: `/.well-known/oauth-protected-resource`
  describes the host, and the same path plus `/mcp` describes the endpoint, which
  is the one the `WWW-Authenticate` header on a 401 points at.
  `authorization_servers` holds exactly one entry, the console, because a client
  uses the first and does not fall back.
- **The upload path and the identity path now derive the visitor's address the
  same way.** `uploadAddress` used to pass an empty console host to
  `auth.ClientAddress` deliberately, because no `/v1` route was reachable on the
  only hostname the header was trusted on. Spec 0022 put `POST /v1/uploads` on a
  public hostname of its own, so that reasoning expired with it: the function is
  gone and every surface passes the same trusted host set, the console and the
  deploy host (AC-13, AC-14).
- The address derivation itself lives once, in `auth.ClientAddress`. The page
  surface and this one must derive it identically or they spend from two
  different buckets, and a limit a second surface resets is not a limit
  (spec 0021, AC-16; spec 0022, AC-14).
- `fail` is the one place an error becomes a status, and `statusFor` is the one
  place a code becomes one, so a code cannot mean two things on two endpoints.
  **There is a second copy of `statusFor` in `internal/web/identity_pages.go`.**
  The two agree today, value for value, and nothing tests that they keep
  agreeing. Change one and change the other in the same commit.
- Every rule that judges a credential belongs to `identity.Service`, not to a
  handler here. `login` used to check `LockedOut` and call `Failed` itself while
  the browser surface did neither; that moved into `Service.Login` on 2026-08-16
  and must not come back, because a second `Failed` call here would count one
  wrong password twice on this surface alone. See the sign in rule in the root
  [AGENTS.md](../../AGENTS.md) and in
  [internal/identity](../identity/AGENTS.md). `spend` stays here: it bounds the
  call rate rather than judging the credentials.
- A refusal is one of the closed `identity.Code` values and a short sentence.
  Nothing internal crosses this boundary: no wrapped error string, no hash, no
  session id, no raw token. The token and invite response shapes have no field
  for a raw value at all, so one cannot leak by being forgotten.
- Register, resend and forgot answer the identical body whether or not the
  address exists, and register costs a full password hash either way. Do not add
  an early return for an address that is taken.
- `decode` bounds an identity request body to 8KiB. Every one of them is a
  handful of short fields, so anything larger is not a real caller.
- The session gate in `session`, and `adminSession` on top of it, are the only
  places a request becomes an account. A handler that resolves an account any
  other way is how an ownership check gets skipped: an account id always comes
  from the resolved session, never from anything the caller sent.
- Both admin state changes go through `internal/suspend`, the same use case the
  admin page calls, so the two surfaces stop the same apps and write the same
  audit rows. A partial outcome answers 200 with the slugs rather than 204,
  because the account did change state and the caller has something to act on.

## Tests

Two harnesses, both on a real SQLite file in a temp dir, no store mocking:
`newHarness` in `httpapi_test.go` for the upload surface, and `newIDHarness` in
`identity_test.go` for the identity surface, which pins the clock so link and
token expiry are exercised without waiting.

`TestFailedSignInsThrottle` covers the lockout on this surface. Its pair for the
browser is `TestTheBrowserSignInLocksOutLikeTheJSONSurface` in `internal/web`.
Keep both: a shared rule asserted by only one surface's tests is exactly how the
lockout came to be missing from the other for months.

_Drafted by /audit at the engineer's request, worth a quick human pass._
