# internal/identity

The rules a person's account turns on: what a password has to be, what an
address has to look like, how long a link or a session lives, and the closed set
of codes a caller is ever told. Governing specs:
[0007](../../docs/specs/0007-accounts-tokens-app-ownership/index.md) for accounts,
tokens and the admin view, [0015](../../docs/specs/0015-invite-only-registration/index.md)
for invites, and [0021](../../docs/specs/0021-public-edge/index.md) for the parts
the public edge changed.

It is pure. It resolves nobody, opens nothing, and imports neither the store,
`net/http`, nor `client-go`. The surfaces that do reach those things take the
narrow interfaces declared here.

## Files

- `identity.go`: the `Code` set, the `Error` type, `CodeOf`, and the lifetime
  constants.
- `service.go`: the `Store` and `Mailer` interfaces, `Options`, `Service`, and
  registration, verification and resend.
- `session.go`: `Login` and `Logout`, and therefore the whole sign in refusal
  path.
- `credential.go`: the password policy, `NormalizeEmail`, `CheckEmail`,
  `CheckPassword`, and the argon2id `Hasher`.
- `limiter.go`: both limits, the per caller token bucket and the per account
  lockout.
- `tokens.go`: API tokens, plus the admin reads and `SetDisabled`.
- `oauth.go`: the OAuth value types, the endpoint paths, the redirect URI rules
  (`CheckRedirectURI`, the exact match and the loopback port relaxation), PKCE
  (`S256Challenge`, `VerifyPKCE`) and `CleanClientName`.
- `oauthgrant.go`: the connector client lifecycle, `RegisterClient`,
  `ApproveClient`, `Grant` and `SweepClients`.
- `invites.go`: the invite lifecycle and the derived `InviteState`.
- `messages.go`: the plain text bodies of every message the platform sends.

## Conventions

- A failure a caller sees is one of the `Code` values in `identity.go`, never a
  wrapped error string. `Error` deliberately does not carry the underlying error,
  and `CodeOf` answers `CodeInternal` for anything that is not an `*Error`, so a
  fault is never dressed up as an access decision. This set is separate from
  `domain.Reason`, which describes how a deploy ended; the two share no values.
- `Service` orchestrates and holds no business rule. Rules live beside the value
  that owns them, as pure functions in this package. A new rule goes next to its
  type, not into a method body.
- **Every rule a sign in is refused by lives in `Service.Login`, never in a
  handler.** Two surfaces call it, `internal/web` and `internal/httpapi`, and a
  rule only one of them applies is not a rule (spec 0021, AC-5). The lockout was
  in the JSON handler alone until 2026-08-16: `Login` touched the limiter
  nowhere, so the browser counted no failures and honoured no penalty, and the
  public edge had just made the browser the only surface reachable from the
  internet. Two things follow. A refusal added to a handler is a bug even when
  that handler looks like the only one, and a rule moved in here has to be
  **removed** from the handler, or one wrong password counts twice on that
  surface. The token bucket is the deliberate exception: handlers spend it
  themselves, because it bounds the call rate rather than judging the
  credentials.
- **`limiter.go` says "address" for two different things, and they are not the
  same key.** `Allow(client)` buckets on the caller's network address, from
  `auth.ClientAddress`. `LockedOut(email)` and `Failed(email)` key on the email
  address of the account being guessed at, which is what stops one person's typo
  spree locking out everyone behind a shared address. Read every "per address"
  comment in that file against the parameter name before trusting it.
- The limiter lives in memory and is lost on every restart, and ArgoCD restarts
  the pod on each sync. Its own comment used to justify that with "the perimeter
  is a tailnet", which spec 0021 retired when the console went public. Spec 0022
  settled it rather than leaving it owed: the comment no longer claims a tailnet,
  and the cost is recorded in the open, a restart forgives every run of bad
  credentials in flight (AC-23). It is accepted, not overlooked, so do not
  rediscover it as a finding.
- **The OAuth flow is the one written exception to the closed `Code` set.** The
  three routes spec 0024 adds answer RFC 6749 error codes (`invalid_request`,
  `invalid_grant`, `invalid_target` and friends) rather than `Code` values,
  because a connector client parses the OAuth vocabulary and understands nothing
  else. The exception is scoped to those routes and recorded in AC-24, so it is
  not licence to invent a second code set anywhere else. The errors live in
  `oauthgrant.go` as sentinels (`ErrClientMetadataInvalid`,
  `ErrRedirectURIInvalid`, `ErrGrantInvalid`), and the first two must never
  overlap because they answer different OAuth codes.
- **Every way an exchange can fail is one error on purpose.** `ErrGrantInvalid`
  covers an unknown code, a spent one, an expired one, the wrong client, the
  wrong redirect URI, the wrong resource and a failed verifier alike, so the
  response never says which check refused it (AC-18). The same reasoning as the
  uniform register and resend answers: a more helpful message is a way to ask
  what the platform knows.
- **A replay's verifier decides whether it revokes, and the order of the checks
  in `Grant` is what makes that true.** A consumed code is always refused, but it
  revokes the token it issued only when the replay cannot produce the stored PKCE
  challenge (AC-16b). The consumed branch therefore answers *before* the client
  id, redirect URI and resource are compared, so a replay naming the wrong client
  but proving the verifier still keeps the token. Moving that branch after any of
  those checks quietly restores the bug it replaced, where an ordinary retry cost
  a connector the credential it had just been issued.
- **There are two limiters, and their numbers are a parameter rather than
  constants.** `NewLimiter` takes a `Settings`, `SignInSettings()` holds exactly
  the values that were package constants before spec 0022, and
  `DeployPathSettings()` is the upload and MCP endpoint's own, wider and refilling
  faster because an agent polls `deployment_status` through a build that runs for
  minutes. Keeping them separate is the point: a burst on the deploy path must
  never spend a person's sign in budget or lock them out of the console (AC-15).
  Spec 0024 made that set three: `ConnectorSettings()` in `oauth.go` is the OAuth
  endpoints' own bucket, and it is kept off the sign in one for the same reason
  sharpened. Adding a single connector spends it three times in a row from one
  address, so a shared bucket would let a person adding a connector lock
  themselves out of the console they are signing in to (spec 0024, AC-6, AC-22).
- Answers are uniform on purpose, and the uniformity is the feature. Register,
  resend and forgot all answer the same sentence whether or not the address
  exists. All five ways an invite can be bad are one sentence. A row that belongs
  to somebody else is `not_found`, exactly as a missing one is. Adding a more
  helpful message to any of these turns it into a way to ask who is registered.
- Uniform wording is not enough on its own, so the work is uniform too. An
  unknown address still spends a full password hash, and the bootstrap account's
  empty stored hash goes through `burn` rather than returning early, so signing
  in as it is not measurably faster than a wrong password on a real account.
  An early return added for efficiency reopens a timing oracle.
- `Register` looks the invite up in its first statement, ahead of `CheckEmail`
  and `CheckPassword`. A caller with no valid invite is refused `invite_invalid`
  whatever else is wrong with their submission, so the gate is never spoken past
  by a validation message and costs the platform no key derivation.
- The argon2id parameters hold about 19MiB per hash, which is why `Hasher` bounds
  concurrency. A burst of sign ins without that bound walks the pod past its
  memory limit. Every stored hash carries the parameters it was made with, so a
  future bump verifies old hashes at their own settings.
- Lifetimes (`SessionLifetime`, `LinkLifetime`, `InviteLifetime`, the token day
  bounds) and `NoteLimit` are constants here rather than `DEPLOYER_*`
  configuration, because they are product decisions rather than something an
  operator tunes per cluster. The same reasoning governs the bounds in
  `internal/logs`.
- Mail is best effort. `send` swallows a provider failure into the log, because
  the account or the link is already committed and a provider being down must not
  turn a successful registration into an error. A nil `Mailer` is a supported
  state and means the mail dependent endpoints answer `mail_unavailable` while
  everything else works.
- `InviteState` is derived from three timestamps against the clock, never stored,
  so nothing sweeps the table and a clock change leaves no stale value behind.
- `SpendInviteAndCreateAccount` returns two errors that must not collapse into
  one at this boundary: `ErrInviteInvalid` is a refusal the caller sees, and
  `ErrEmailTaken` is the case that has to read exactly like a success.

## Tests

Pure logic here is written test first, per the root [AGENTS.md](../../AGENTS.md).
`credential_test.go` and `limiter_test.go` are table tests over the pure
functions and need no store. Anything reaching `Service` needs a real SQLite file
in a temp dir, which is why most `Service` coverage lives in the harnesses in
`internal/httpapi` and `internal/web` rather than here. That split is worth
knowing before you go looking for a `Login` test in this package: the one that
holds the lockout is `TestTheBrowserSignInLocksOutLikeTheJSONSurface` in
`internal/web`, paired with `TestFailedSignInsThrottle` in `internal/httpapi`.
A rule that only one surface's tests assert is how the lockout gap survived.

_Drafted by /audit at the engineer's request, worth a quick human pass._
