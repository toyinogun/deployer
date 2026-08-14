# 0016. Per account app cap

**Date**: 2026-08-14
**Status**: In Progress

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

Nothing counts how many apps an account holds, so one account can create as many as the cluster will take. Every quota built so far bounds what a single app consumes (CPU, memory, pod count in its own namespace), not how many an account may start. This spec adds one configured number, one live count, and one new refusal code: a deploy of a name the account does not already have is refused once it is at its cap, while everything the account already runs keeps working and keeps deploying. There is no migration and no new table.

## Requirements

**User stories**:
- As the platform owner, I want one account to be unable to fill the cluster with apps, so that a runaway agent loop costs me one refusal rather than a night of cleanup.
- As the platform owner, I want to see how close each account is to its ceiling, so that I notice a person running out of room before they ask.
- As an agent holding a token, I want a refusal that tells me I am at the limit and what the numbers are, so that I delete an app and retry rather than guessing what went wrong.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):
- **AC-1**: A `deploy_app` naming an app the account does not already hold, made by an account already holding `MaxAppsPerAccount` live apps, is refused `app_limit_reached`. It writes no app row, no deployment row, and no configuration.
- **AC-2**: That refusal leaves the upload unredeemed, so a caller who deletes an app can retry with the same `upload_id` inside its expiry.
- **AC-3**: The refusal line carries both numbers, the account's current live app count and the configured cap, alongside the code. It reads exactly `app_limit_reached: this account is at its limit for apps, so delete one you no longer need (10 of 10 used)`, with the two numbers substituted.
- **AC-4**: A `deploy_app` naming an app the account already holds is never refused by the cap, whatever the count. An account at or over its cap can still redeploy, configure, and roll back everything it already runs.
- **AC-5**: A deleted app frees a slot immediately. After `delete_app` succeeds, a deploy of a new name that was refused a moment earlier is accepted.
- **AC-6**: Two `deploy_app` calls racing on two new names for one account one slot below its cap end with exactly one app created and the other refused `app_limit_reached`. The count and the insert happen inside one store transaction, so no interleaving takes an account over.
- **AC-7**: `DEPLOYER_MAX_APPS_PER_ACCOUNT` is optional and defaults to 10. It must parse as an integer greater than zero, and `internal/config` fails startup with a named error otherwise, never at first use. There is no value meaning no cap.
- **AC-8**: `app_limit_reached` is a member of the closed reason set in `internal/domain/reason.go`, carries its own static sanitized line that names no number, and `Valid()` accepts it.
- **AC-9**: The refusal is audited the way every other `deploy_app` refusal is: one audit row, action `deploy`, reason `app_limit_reached`, not allowed, with no app target because no app exists.
- **AC-10**: The apps page shows the signed in account's usage in the form `3 of 10 apps`, read from the same live count the refusal uses.
- **AC-11**: When the account is at or over its cap, the apps page shows a plain notice reading `You are at your limit of 10 apps. Deploying a new app will be refused until you delete one.`, with the number substituted. Below the cap it shows no notice.
- **AC-12**: The admin accounts page shows each account's live app count, read by one grouped statement joined into the existing accounts listing. No per row count query is issued.
- **AC-13**: `deploy_app`'s tool description states that an account may hold a limited number of apps, that deploying a new name past it is refused, and that deleting an app frees a slot. It states no number.
- **AC-14**: An account over the cap because the number was lowered keeps every app it has. Nothing is torn down, nothing stops serving, and no scheduled sweep acts on the overage.
- **AC-15**: The cap is per account, never per token. Two tokens of one account share one allowance, and one account's count is never moved by another account's apps.
- **AC-16**: The refusal is proved through a real MCP client and server session, not only by calling the handler, so the code reaches a caller as a reason code rather than as a schema validation string.
- **AC-17**: No migration. The schema is unchanged, and a previous binary runs against the same database unharmed.

## Decision

**Chosen option**: Option 1: A configured ceiling counted live at the one place an app is created.

An account may hold at most `DEPLOYER_MAX_APPS_PER_ACCOUNT` live apps, counted at deploy time from the `apps` table, checked in `deploy_app` immediately before `resolveApp`, and enforced exactly inside the store transaction that inserts the row.

**Implementation skills**: `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `security-patterns` (`~/.claude/skills/security-patterns/`)

## Feature design

**Data model sketch**:

No schema change. The cap is a count over rows that already exist:

| Entity | What this feature reads | Why nothing is added |
|---|---|---|
| `apps` | `account_id`, `deleted_at` | The live count is `COUNT(*) WHERE account_id = ? AND deleted_at IS NULL`, the same predicate every other app read already uses, so a soft deleted app frees a slot with no extra column and no reaper involvement |
| `accounts` | nothing new | The cap is one global number in configuration, not a per account column. See the Rationale for why an override column was left out |

The stored count is deliberately not materialised on `accounts`. A stored counter would need every create and delete to keep it true, and it goes stale the first time one of them does not.

**State transitions**: none. The cap is a gate on one create, not a lifecycle.

**API surface**:

| Surface | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `deploy_app` (MCP tool) | tool call | `name`, `upload_id`, optional `config` | unchanged: `name`, `slug`, `url`, `deployment_id`, `state` | bearer token | `app_limit_reached` (new, only when `name` is a name the account does not already hold), plus every existing code unchanged |
| `GET /apps` (page) | GET | session | the app list, plus the usage line and the at cap notice | session cookie | unchanged |
| `GET /admin/accounts` (page) | GET | admin session | the accounts listing, plus a live app count per row | admin session | unchanged |

Nothing new is exposed. There is no endpoint that reports the cap on its own, because the two places a caller needs it, the refusal and the apps page, both carry it.

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `deploy_app`, is this name new | the existing app, or its absence | `apps.ByName`, called by `deploy` itself. `resolveApp` is inlined and removed, so the one lookup both gates the cap and feeds the create. There is never a second `ByName` call |
| `deploy_app` cap check | the account's live app count | a new store read, `CountLiveAppsByAccount`, over `apps WHERE account_id = ? AND deleted_at IS NULL`, reached through a new `Count(ctx, accountID) (int, error)` method on the `Apps` interface `internal/mcp` already defines |
| `deploy_app` cap check | the cap | `config.MaxAppsPerAccount`, from `DEPLOYER_MAX_APPS_PER_ACCOUNT`, default 10, carried on a new `MaxAppsPerAccount int` field on `mcp.Options` beside `AppDomain` |
| `deploy_app` refusal | the reason code | `domain.ReasonAppLimitReached` |
| `deploy_app` refusal | the one line a caller reads | composed in `internal/mcp`. `deny` and `toolError` take an optional detail string appended after the static message in parentheses, so the domain message itself stays static and numberless and every existing refusal composes unchanged |
| `deploy_app` refusal | the audit row | the existing `s.deny` path, unchanged, with an empty app id because no app was created |
| create inside the transaction | the refusal in a race | `store.ErrAppLimit`, declared in `internal/store/errors.go`, returned by `CreateApp` when the count read inside its transaction is already at the limit, and mapped to the same refusal in `MCPApps.Create`. Its detail reports the cap as both numbers, which is exactly true at that point |
| apps page | `3 of 10 apps` | the same `CountLiveAppsByAccount` read, reached through a `Count` method on the data interface `internal/web/apps.go` already uses, and a new `MaxAppsPerAccount int` field on `web.Options` |
| apps page | whether to show the at cap notice | derived at render time: count is greater than or equal to the cap |
| admin accounts page | each account's live app count | one grouped statement left joined onto the accounts listing: `LEFT JOIN (SELECT account_id, COUNT(*) AS n FROM apps WHERE deleted_at IS NULL GROUP BY account_id) c ON c.account_id = a.id`, projected as `COALESCE(c.n, 0)` so an account with no apps reads `0` rather than dropping out or arriving null, the way `ListAppSummariesByAccount` already coalesces its own left join |

**Key invariants**:
- An account never gains an app past its cap. Enforced in the store transaction that inserts the row, not only by the read before it. The transaction is the store's existing `inTx`, which opens with `BEGIN IMMEDIATE` and so takes the write lock before the count statement runs. That ordering is what makes the check exact rather than a read that two writers can both pass.
- The count is read once at the top of that transaction. `CreateApp`'s existing `slugAttempts` loop retries a colliding slug suffix only, which cannot change how many apps the account holds, so it does not recount.
- A deploy of a name the account already holds is never gated. The cap is a rule about creating an app, never about deploying one.
- The count and the cap are read fresh at every decision. Neither is cached, stored, or derived from another count.
- The closed reason set stays closed. The numbers travel in the composed line, never as a new error shape and never as a wrapped internal error.
- A cap refusal is decided before anything is written, so a refused call changes nothing, which is the same promise the six `config_` codes already make.

**Security model**:
- The cap is an availability control, not an access control. It stops one account exhausting shared capacity; it grants and removes nothing.
- The count is always scoped to the calling account's id, taken from the authenticated token, never from an argument. There is no way to ask about another account's count through a tool.
- The admin count is on the admin accounts page, which already refuses an ordinary session with `admin_required` and is unreachable with an API bearer token. That guard is reused, not rebuilt.
- The refusal reveals only the caller's own numbers, so it leaks nothing about the platform or about anyone else.
- No regulated data, so no compliance scope is triggered.

**Configuration required**:
- `DEPLOYER_MAX_APPS_PER_ACCOUNT`: the greatest number of live apps one account may hold. Optional, defaults to 10, must be an integer greater than zero. Parsed and validated inline in `Load` in `internal/config`, in the same block and the same shape as `DEPLOYER_APP_QUOTA_PODS`, so it fails startup rather than at first use. Set it in `deploy/` alongside `DEPLOYER_APP_QUOTA_PODS` so the two ceilings, per app and per account, are read together.

**Critical test scenarios**:
- Happy path: an account below its cap deploys a new name and it is created exactly as before, verifies **AC-4**, **AC-15**.
- Cap refusal over the wire: an account at its cap deploys a new name through a real client and server session and gets `app_limit_reached` with both numbers, no app row, no deployment row, and an unredeemed upload, verifies **AC-1**, **AC-2**, **AC-3**, **AC-16**.
- Failure case, concurrency: two deploys of two new names issued at once by an account one slot below its cap, against a real SQLite file, end with one create and one refusal, verifies **AC-6**.
- Redeploy at the cap: an account at its cap deploys a name it already holds and is not refused, verifies **AC-4**.
- Freeing a slot: delete an app, then the previously refused new name deploys, verifies **AC-5**.
- Over the cap after lowering it: start with more apps than the cap allows, confirm every one still deploys and none is removed, verifies **AC-14**.
- Configuration: unset gives 10; `0`, `-1`, and `banana` each fail startup with the variable named, verifies **AC-7**.
- Auth/permission: an ordinary session gets `admin_required` on the admin accounts page, so the per account counts stay admin only, verifies **AC-12**.

## Build plan

Ordered as a Tracer Bullet: the thinnest end to end thread that refuses a real call comes first, then the exactness, then the surfaces.

1. Add `DEPLOYER_MAX_APPS_PER_ACCOUNT` to `internal/config`, parsed inline in `Load` beside `DEPLOYER_APP_QUOTA_PODS`, with its default of 10 and the greater than zero validation, satisfies **AC-7**.
2. Add `ReasonAppLimitReached` to `internal/domain/reason.go` with its static numberless message, and update the count in the package comment and the `Valid()` doc, satisfies **AC-8**.
3. Add `CountLiveAppsByAccount` to `internal/store/queries/apps.sql`, regenerate with `sqlc generate`, and expose it as `Count` on the `Apps` interface in `internal/mcp` and on the `MCPApps` adapter, satisfies **AC-1**, **AC-15**.
4. Give `deny` and `toolError` an optional detail string appended after the static message, leaving every existing refusal composing byte for byte as it does now, satisfies **AC-3**.
5. Inline `resolveApp` into `deploy` in `internal/mcp/mcp.go`: call `apps.ByName` after `checkUpload`, and only on `ErrNoApp` read the count, compare it against `Options.MaxAppsPerAccount`, refuse through `s.deny` with the detail, or create, satisfies **AC-1**, **AC-2**, **AC-4**, **AC-9**.
6. Prove the thread through a real client and server session with the `callOverTheWire` helper in `internal/mcp`, satisfies **AC-16**.
7. Make it exact: `CreateApp` in `internal/store` takes a `limit int`, wraps its existing `slugAttempts` loop in one `inTx`, reads the count once at the top of that transaction, and returns the new `ErrAppLimit` from `errors.go`, which `MCPApps.Create` maps to the same refusal. Race test it against a real SQLite file, satisfies **AC-6**.
8. Update `deploy_app`'s tool description with the rule, in the same commit as the check, satisfies **AC-13**.
9. Add the usage line and the at cap notice to the apps page, with a `Count` method on the page's data interface and a `MaxAppsPerAccount` field on `web.Options`, satisfies **AC-10**, **AC-11**.
10. Add the coalesced grouped live app count to the admin accounts listing statement and render it on the page, satisfies **AC-12**.
11. Cover the leftovers: freeing a slot after a delete, and an account left over the cap by a lowered number keeping everything, satisfies **AC-5**, **AC-14**.
12. Confirm no migration was added and the schema is untouched, satisfies **AC-17**.

## Consequences

**Positive**:
- One runaway agent loop now costs one refusal instead of as many namespaces as the cluster will hold.
- The control is one number in the deployed manifest, so raising it is an edit and a restart, not a code change.
- Nothing is stored, so the count cannot drift from the truth and there is no counter to repair.
- Existing accounts and apps are untouched. The feature can be deployed to a running platform with no data work at all.

**Negative / tradeoffs**:
- Everyone shares one number. Giving one person more room means raising it for everyone or editing the database by hand, and the escape hatch does not exist yet.
- The count is one more read on the deploy path. It is a single indexed count on a small table, but it is on the path.
- The number in the refusal is right at the moment it is composed and could be stale by the time an agent reads it, which is harmless for a message and would not be for a decision.
- The cap counts apps, not capacity. Ten tiny apps and ten heavy ones cost the same against it, so the per namespace quotas are still what actually bound consumption.
- A person can only discover the number by hitting it or by opening the apps page. The tool description deliberately does not carry it.

**Neutral**:
- `internal/web/reason.go` gains nothing. This code is refused before a deployment row exists, so it can never land in `deployments.failure_reason` and never reaches the page that renders a failure sentence. The six `config_` codes are absent from that table for the same reason.
- The closed reason set goes from twenty codes to twenty one, and the count is stated in the package comment, so that comment changes too.
- The store gains its second transactional create, after the deployment create with supersession, which is an established pattern here rather than a new one.
- `resolveApp` disappears. Its two jobs, look up and create, split across the cap check, which is a small readability loss in `deploy` traded for one lookup instead of two.
- `deny` and `toolError` grow an optional detail parameter. Every existing call passes nothing and composes exactly as before, so this widens a shape without changing any current output.

## Follow-up

- [ ] A per account override is deliberately out of scope. If a second person needs more room than the global number, that is a nullable `app_limit` column on `accounts` plus an admin control, and it is worth its own small spec rather than being smuggled in here.
- [ ] The apps page is the only place a person sees their ceiling. If a web create path is ever added, the cap check has to move behind a shared guard both paths call, because the current design relies on `deploy_app` being the only way an app row is born.
