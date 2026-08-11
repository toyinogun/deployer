# 0002. Platform data model, rationale

Reasoning, options weighed, and the forces behind the decision. `/develop` does not read this file; humans and a later `/architect` run do.

## Context

> ⚠️ Premise note: this schema becomes the only place that knows an app exists, who owns it, and which image is running. The cluster keeps running every app if the database is lost, but Deployer can no longer name, redeploy, roll back, or tear down any of them, and the slugs it retired are forgotten so a new app could take a live hostname. Spec 0001 already carries backup as a follow up and the scope defers it. That was defensible when the database held nothing; once this migration runs it holds everything. Treat the backup decision as a prerequisite of relying on Deployer, not of building it, and do not let it slide past slice 1.

Deployer has a settled stack (spec 0001) and an empty scaffold. It has no schema. Every slice from the tracer bullet onward writes to the same small set of entities: an account calls a tool, an app is created, a deployment walks a lifecycle, a release records what became healthy, and later slices add tokens, config, and an audit trail. The scope calls this out as the most expensive thing to redo, which is why it is decided before any slice writes to it.

Four forces shape the design.

**One writer, one file.** SQLite with a single control plane replica removes the usual reasons to denormalize for write throughput and removes the usual reasons to fear a join. It also removes any safety net: there is no second process to notice a bad write, and no server side migration tooling. Correctness has to come from constraints and from a small number of code paths that own each invariant.

**The caller is an AI agent, not a person.** An agent retries, fires twice, quotes an id back at you out of context, and reads whatever the tool returns. That pushes toward opaque self describing ids, toward a deployment lifecycle precise enough that a failure reports something actionable, and toward a hard rule that secret values never leave the database through a tool response.

**A rollback must restore an app, not an image.** Slice 8 wants a rollback to re promote an exact prior image without a build. An image alone is not the app: the same image with different environment configuration is a different running system. Whatever a release records has to be enough to reconstruct what was healthy.

**Later slices must not reopen the model.** Accounts, tokens, config, and the audit trail belong to slices 4, 7, and elsewhere, but their columns constrain the shape of everything around them. Deciding them now costs a bigger first migration. Deciding them later costs a migration against live data on a database with no backup.

Not deciding leaves the tracer bullet writing to whatever schema the first build invents, which is the failure the scope wrote this feature to prevent.

## Options considered

### Option 1: Full normalised model, nine tables, transitions as an event log

Every entity the MVP scope implies gets its own table in one migration: accounts, api_tokens, apps, app_config, uploads, deployments, deployment_events, releases, audit_log. Current state lives on the row, history lives in append only event and audit tables. CHECK constraints police enumerated values, Go owns which transitions are legal.

**Pros**:
- Every later slice adds behaviour and queries, not schema, so the model is decided once as the scope asks.
- History and current state are separate, so a status read is one row and a diagnosis is one indexed scan.
- Constraints carry the invariants that a single writer cannot otherwise guarantee, so a manual `sqlite3` fix or a stray query fails loudly.

**Cons**:
- The first migration is large relative to what slice 1 actually uses, and several tables sit empty for weeks.
- Nine tables plus their sqlc queries is real code to write before the tracer bullet proves anything.
- Deciding token, config, and audit columns now means guessing slightly at slices that have not been designed.

### Option 2: Core deploy path only, grow per slice

Ship apps, deployments, deployment_events, releases now. Accounts, tokens, config, and audit arrive with their own slices.

**Pros**:
- Smallest thing that lets the tracer bullet run, and nothing is designed before its slice is understood.
- Each migration is reviewed next to the code that uses it, which is when the requirements are sharpest.

**Cons**:
- App ownership has nothing to point at in slice 1, so `apps.account_id` either does not exist or points nowhere, and slice 4 becomes a migration against live rows on an unbacked database.
- The config snapshot on a release cannot exist before config does, so slice 8 rollback semantics change shape after slice 7.
- Directly contradicts the scope line that this is decided once, up front, before any slice writes to it.

### Option 3: Event sourced deployments

No `state` column. The deployment's state is folded from its event log on read; the events table is the only truth.

**Pros**:
- History cannot disagree with current state, because there is only one of them.
- A complete audit trail comes free rather than being a second write to remember.

**Cons**:
- Every status read becomes a scan and a fold, and the "one active deployment per app" rule cannot be a database constraint at all, only a code convention, on the exact operation an agent is most likely to fire twice.
- The reconcile loop's claim is a conditional UPDATE on a state column. Without one, claiming needs a lock or a separate projection table, which is the complexity this stack was chosen to avoid.
- Substantially more machinery than a nine table platform with one writer needs.

### Option 4: One JSON document per app

Store an app and its whole deployment history as a JSON blob, using SQLite as a key value store.

**Pros**:
- Almost no schema to design and no migrations to order.
- The shape can change freely while the platform is young.

**Cons**:
- The data is plainly relational: accounts own apps, apps have deployments, deployments produce releases. Cross entity queries such as "every non terminal deployment across all apps", which the reconcile loop runs on every tick, become full scans and application side filtering.
- No foreign keys, no uniqueness, no CHECK constraints, so every invariant including slug uniqueness moves into code with nothing behind it.
- Concurrent writes to one document need read modify write, reintroducing exactly the race the single writer was supposed to remove.

## Rationale

Option 1 wins on the two forces that dominate: the scope's explicit instruction to decide this once, and the absence of a backup. Any option that defers schema defers it into a migration that will one day run against the only copy of data describing every running app. A large first migration on an empty file is free; a small one later is not.

The relational shape is not a preference, it is what the data is. The reconcile loop's hot query is "every non terminal deployment", the rollback query is "this app's releases by number", and the authorization query is "does this account own this app". All three are one indexed lookup in Option 1 and a scan in Option 4. Option 3 loses the conditional UPDATE that the whole single writer claim design in spec 0001 rests on, which is too high a price for an audit trail that Option 1 gets by writing one extra row inside the same transaction.

Two choices here run against normal defaults and were made deliberately.

**Soft deleting apps.** The usual advice is right that soft deletes pollute queries and break unique constraints. Here the constraint is the point: a slug is a live DNS hostname, an image repository path, and a Kubernetes namespace. Reissuing one lets a new app inherit another account's cached image or stale DNS record, which is a cross tenant bug, not a tidiness bug. Keeping the row is the cheapest way to keep the slug permanently taken. The cost is real and is paid narrowly: `deleted_at IS NULL` is a filter on the app listing and lookup queries only, and the uniqueness that matters, `slug`, is deliberately global and unfiltered.

**Storing a config snapshot on the release.** Storing derived values is usually wrong because they go stale. This one is immutable by definition: it is what the configuration was at the moment that image became healthy, and nothing may ever update it. Without it, a rollback re promotes an old image against today's environment, which is not the system that was known good. A JSON column beats a versioned config table because the snapshot is only ever written once and read whole, so normalising it buys a join and nothing else.

The engineer chose plaintext storage for values marked secret rather than encrypting the column. That is the honest call for this deployment: the same database file already holds token hashes and single use upload tokens, and it sits on a Longhorn volume that only the control plane pod mounts. Encrypting one column while the rest of the file is readable is theatre, and it introduces a key whose loss destroys every app's configuration. The control that actually matters is that secret values never leave the database, which is enforced in the store's read surface rather than left to each caller to remember. The tradeoff to accept consciously: anyone who can read the volume or a backup of it reads every app's secrets in clear, so any future backup decision must treat that file as sensitive.

`oklog/ulid` over a hand rolled generator is one small dependency against thirty lines of encoding and monotonicity that would otherwise need its own tests. The prefix convention (`app_`, `dep_`, `rel_`) is worth the four extra bytes because an agent quotes ids back in isolation and a prefix makes a wrong one obvious immediately.
