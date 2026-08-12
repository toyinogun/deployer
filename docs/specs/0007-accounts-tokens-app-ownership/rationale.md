# 0007 rationale

The reasoning behind [index.md](index.md). Not read during a build.

## Context

> ⚠️ Premise note: the scope calls this slice "thickens the auth segment", and lists **Web UI** under Deferred with "no identity provider, no OAuth in the MVP" settled for the MVP. The request that opened this design was a product where people register, which is a larger thing than the slice as written. It is the right direction, and it does two things the scope has not accounted for: it makes the deferred web interface the obvious next feature rather than a someday item, and it ends spec 0002's deliberate one migration decision. This spec covers the identity model and its whole HTTP surface, and explicitly leaves the pages to a separate feature, so the thing being built here stays one decision that is fully verifiable with curl.

The platform authenticates exactly one caller. `DEPLOYER_BOOTSTRAP_TOKEN` is seeded into an account named `bootstrap` at every boot, and that is the only credential that has ever existed. There is no way to create a second account, no way to mint a second token, and no way to revoke one short of changing the sealed Secret and restarting.

The pieces underneath are in better shape than that suggests. Spec 0002 designed `accounts`, `api_tokens` and `audit_log` up front, including a partial unique index that frees a token name when it is revoked, an `expires_at` column nothing has ever written, and a `disabled_at` nothing has ever set. Tokens are already stored as SHA-256 hashes with a readable prefix. `ResolveToken` already treats unknown, revoked, expired, and belonging to a disabled account as one indistinguishable failure. Every app, upload and deployment already carries an `account_id`, and `deployment_status` and `get_logs` already refuse another account's row with a closed reason code rather than a 404 that leaks existence. The boundary was designed; it has just never had two accounts to hold apart.

Three forces shape what happens next. The perimeter is a tailnet, so everything here is reachable only from your own network, which makes a distributed rate limiter and a hosted identity provider both disproportionate. The control plane is one pod with a 512Mi limit and a SQLite file on a volume, so anything that allocates per request has to be bounded, and anything that writes has one writer. And the database has no backup, which is why spec 0002 shipped a single migration on purpose, and why adding a second one is a decision rather than a detail.

The cost of not deciding is that the platform stays a single user tool with an unprovable isolation story, and every later slice, workload isolation in particular, has nothing real to isolate from.

## Options considered

### Option 1: an operator CLI, no web surface

Add `deployer accounts create` and `deployer tokens mint|revoke` to the same binary, run with `kubectl exec` into the pod. Authority is cluster access, which is already total power over the platform, so there is no role model and no schema change at all.

**Pros**:
- Smallest possible change: no new HTTP surface, no sessions, no passwords, no outside dependency.
- Nothing new to secure. Anyone who can exec into the pod could already read the database file.
- Keeps spec 0002's one migration decision intact.

**Cons**:
- Not a product. Every account is a manual act by you, which is the opposite of what was asked for.
- Leaves `disabled_at`, `expires_at` and the whole idea of more than one user theoretical.
- Nothing to build a web interface on later without designing this all over again.

### Option 2: extend the account into a person, session backed web surface beside the token surface

`accounts` gains `email`, `password_hash`, `email_verified_at`, `is_admin` and `display_name`. People sign in with a session cookie on new endpoints; machines keep bearer tokens on the endpoints they already use. Verification and reset go out through a hosted email API.

**Pros**:
- Every table already points at `accounts(id)`, so ownership, apps, deployments, uploads and the audit log need no change at all. The boundary that exists starts holding real people apart.
- Two credential types, each on the surface it suits: an HttpOnly cookie for a browser, a bearer token for an agent. Neither leaks into the other's path, and CSRF never touches the deploy path.
- Fully verifiable with curl before any page exists, so the web interface can be designed as a UI rather than as an identity system.
- Uses the columns spec 0002 already designed rather than working around them.

**Cons**:
- The first hard dependency on an outside service, for mail.
- Password hashes in a database that has no backup and no encryption at rest.
- A second migration against live data, which is exactly what spec 0002 was written to avoid.
- A meaningful amount of security sensitive code, all of it conventional and all of it easy to get subtly wrong.

### Option 3: separate users and accounts, with membership

A `users` table for the login, `accounts` staying the ownership boundary, and a join table so several people can share an account. The shape a team product wants.

**Pros**:
- Teams, shared apps and per person credentials on a shared account all become possible without another migration.
- Cleanly separates who you are from what owns things, which is the more correct model in the abstract.

**Cons**:
- Every ownership check in the platform grows a join, for a homelab with one operator and no stated need for teams.
- More to build, more to test, and more to get wrong in exactly the code whose whole job is refusing the wrong caller.
- Solves a problem that does not exist yet, and it can be migrated to later if it ever does.

### Option 4: OAuth with GitHub

No passwords at all. Everyone who deploys code already has a GitHub account, and the platform stores no credential.

**Pros**:
- Nothing to hash, nothing to reset, no password policy, no breach exposure from the database file.
- Genuinely less security sensitive code than any option here.

**Cons**:
- The callback URL has to resolve for GitHub, and the platform is a tailnet only hostname, so the flow needs work that has nothing to do with the feature.
- An outside dependency on the sign in path itself, not just on mail: GitHub down means nobody signs in at all.
- A registered OAuth app and a client secret to hold, for a platform whose whole user base is people already on your tailnet.

## Rationale

Option 2 is chosen because the data model spec 0002 designed is already most of the way there and the alternative options either ignore that work or duplicate it. The columns exist. The hashed token pattern exists. The audit table with target columns exists and has never had a privileged action to record. Adding five columns and two tables that mirror `api_tokens` is a smaller change than it sounds, and it turns a set of unused affordances into the feature they were designed for.

Option 1 is genuinely underrated and would have been the recommendation for the slice as scoped. It is rejected on the stated goal alone: a product where people register is not a product where you exec into a pod. Option 3 is the right model for a team product and the wrong one for this platform today; the cost lands on every ownership check, which is the code path least worth complicating, and the migration to it stays available. Option 4 trades the password problem for a callback problem on a hostname that only resolves inside your tailnet, and it puts an outside service on the sign in path rather than only on the mail path.

The forces from Context show up directly in the shape. The tailnet perimeter is why rate limiting is an in memory bucket in one pod rather than a distributed limiter, and why the argument for a hosted identity provider does not carry. The 512Mi limit is why argon2id runs at the OWASP minimum of 19 MiB behind a semaphore rather than at RFC 9106's 64 MiB, which four concurrent sign ins would turn into half the pod's ceiling. The missing backup is why the migration plan opens with a manual copy rather than mentioning one in passing, and why the negative consequences say plainly that this ends spec 0002's one migration decision instead of quietly filing a `00002`.

Two smaller calls worth recording. The MCP failure reason set in `internal/domain` is deliberately not extended: its own doc comment says it describes why a deployment failed, and sign in outcomes are not that, so the web surface carries its own small closed code set. And admin gets no override on apps, logs or deployments; it can see who registered, disable an account, and kill a token, and nothing more. An isolation boundary with a privileged bypass is a boundary that has to be argued about every time someone asks for support access, and the slice after this one is about making that boundary trustworthy.
