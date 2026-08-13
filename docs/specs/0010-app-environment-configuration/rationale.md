# 0010 rationale: app environment configuration

The reasoning behind [index.md](index.md). A build does not need this file.

## Context

An app the platform deploys receives exactly one environment variable, `PORT`, hardcoded in `internal/deploy/deploy.go` with a comment saying application configuration is slice 7. Every app an agent writes that talks to anything (an API, a database, a model provider) therefore cannot run here, which caps the platform at apps that need nothing from the outside world. That is a narrow band of what an agent actually writes.

Three earlier decisions already leaned on this one. Spec 0002 designed the `app_config` table and the release `config_snapshot` column in the single up front migration, precisely so this slice would add behaviour rather than reopen the schema. Spec 0006 shipped log redaction that matches secret shapes, and said exact redaction waits until the platform knows which strings are secret because it injected them. Spec 0009 deferred the question of whether a Dockerfile build receives build arguments or build secrets to this spec.

The forces that shape the answer are mostly about blast radius. The caller is an AI agent, so anything it can get wrong it eventually will, and anything the platform hands back lands in a transcript that is stored, replayed, and sometimes pasted. The platform has no backup and no way to rotate anything, so a value it stores it stores permanently. And the project's own rules bind the design: no user supplied value is ever merged into a pod spec, a failure a caller sees is a closed reason code, an app's own output is never stored, and every state transition is a database write before it is an action.

The consequence of not deciding is that the platform stays a demo. An agent can deploy a static page and nothing else.

## Options considered

### Option 1: Configuration as an argument on `deploy_app` only

Values arrive with the deploy call and nowhere else. No new tools, no readback, no stored surface to keep coherent.

**Pros**:

- Smallest possible addition. One argument, one validation path.
- No question about when a change takes effect, because the change is the deploy.

**Cons**:

- No way to read what is set, so an agent debugging a missing variable has nothing to look at.
- Changing one value means a fresh upload and a full rebuild, which is minutes for a one character change.
- The `app_config` table spec 0002 designed goes unused, and the release snapshot has nothing to snapshot.

### Option 2: A stored configuration surface, applied at the next deploy (chosen)

Three tools own the stored values. A deploy reads them when it composes the workload and injects them through one Kubernetes Secret per app. A change alone does nothing to the running app.

**Pros**:

- The stored surface is readable, which is what makes a missing variable diagnosable.
- Uses the storage, the queries, and the snapshot column that already exist, so no migration.
- Injection through a Secret keeps every caller supplied string out of the pod spec, which the project's rules require anyway.
- The release snapshot becomes real, which is the input slice 8's rollback needs.

**Cons**:

- A change does nothing visible until the next deploy, which contradicts the intuition that setting a value changes the app.
- The scope row's own wording says a change triggers a new release, so this ships a smaller thing than the row promised.
- The platform becomes a durable store of live credentials before it has any backup or rotation story.

### Option 3: A stored surface where a change starts a deployment automatically

The same tools, but setting a value re promotes the app's current image digest with the new configuration, walks the normal deployment states, and cuts a release when healthy.

**Pros**:

- Matches what the scope row promised and what a caller expects.
- The running app and the stored configuration can never disagree.

**Cons**:

- Needs a deployment that skips the build entirely, which is exactly the machinery slice 8 exists to build. Building it here means building it twice or building slice 8 early.
- Every `set_config` becomes a state machine walk with a health check that can fail, so a configuration call gains a way to take the app down.
- Setting four keys one call at a time would start four deployments unless a debounce is invented, which is more machinery again.

## Rationale

Option 2 wins on the shape of the work rather than on the shape of the result. Option 3 is the better end state, and it is where this should land, but the only honest way to build it is on the no build redeploy path, and that path is slice 8. Building a second one here would mean two ways to promote an image before there is one good one, in a codebase whose whole discipline is that the reconcile loop owns every transition. The waiting is deliberate and the follow up records it.

The choices inside Option 2 all follow from the caller being an agent and the platform having no backup. An explicit secret flag beats a name heuristic because a heuristic that misses `AIRTABLE_PAT` fails silently while looking careful, and silent failure is the mode with no recovery when the value is already in a transcript. A single Secret per app beats a Secret per release because per release Secrets accumulate in every namespace with nothing pruning them, and this platform already has one unbounded growth problem in the registry it has not solved. Reading the configuration at compose time rather than at call time avoids a schema change and gives an obviously correct answer to what a deploy ran with, since the release snapshot records exactly that.

The eight character floor on literal redaction is worth naming as a deliberate hole rather than a limit. Redacting every secret value regardless of length means one app with a secret set to `true` gets logs that are almost entirely blanked and no explanation of why. A three character secret was not a secret, and the shape based rules from spec 0006 still catch anything that looks like a credential. The constant already exists in `internal/logs` as `minLiteral`, and the `Redact` function already takes a literals argument nothing currently fills, so this decision fits the plumbing that was left for it.

A cross check on a second model turned up two things worth recording, because both were the design quietly relying on something. The first: the upsert query overwrites `is_secret` with whatever the call passes, so re setting an existing key without the flag would silently turn a secret into a plain value, which is the same silent leak the explicit flag was chosen to avoid. Making the flag required on every write closes it, at the cost of a slightly noisier call. The second: `envFrom` values are read once when a container starts, and Kubernetes does not roll a pod when a referenced Secret's contents change. Configuration would therefore have reached the container only because every deploy happens to produce a new image digest, which is true today and stops being true for a reproducible rebuild and for slice 8's rollback. A checksum of the configuration on the pod template makes the rollout depend on the configuration itself rather than on a coincidence.

The same check found that redaction read from current configuration alone, so rotating a secret would leave the old value, the one the running pod actually printed, in clear in the logs. Redacting against the union with the running release's snapshot fixes the case that matters. It still does not cover a key deliberately flipped from secret to plain, which would need secrecy recorded per release, and that is a snapshot format change not worth making here.

Refusing `PORT` rather than accepting it silently follows the project's rule that a refusal a caller sees is a real access decision. Accepting a value the platform then overrides is the failure mode where the caller sees success and gets different behaviour, which is the hardest kind to debug through an agent. `APP_URL` joins it as reserved because a framework reading its own external address should read the same string the Ingress routes on, and the only way to guarantee that is to compose both in the same place.

Confirming spec 0009's answer on build values took no new reasoning. A value that reaches a build layer is baked into a published image, that image sits in a registry with no garbage collection, and nothing scans it. Runtime injection covers the real need. BuildKit build secrets are the correct mechanism for a private package registry token and will be worth having eventually, but they exist only on the Dockerfile path, so adopting them makes the two builders behave differently for no need anyone has yet stated.

## References

**Project sources**:

- `AGENTS.md`: no user supplied value is ever merged into a pod spec; a failure a caller sees is a closed reason code; the bounds in `internal/logs` are constants because they are product decisions; an MCP tool's description is part of the contract.
- Spec 0002: the `app_config` table, its key `CHECK`, and the release `config_snapshot` column, all designed for this slice.
- Spec 0006: shape based redaction, the `minLiteral` constant, and the `literals` argument on `logs.Redact` left unfilled for this feature.
- Spec 0009: the deferral of build arguments and build secrets to this decision.
- Spec 0003: the app namespace `ResourceQuota`, which allows ten secrets, and the control plane RBAC that already creates a Secret in an app namespace.
- The installed `senior-kubernetes-engineer` and `security-patterns` skills.

**Practices and standards**:

- Least privilege: the app holds no Kubernetes token, so it reads its configuration only as the environment it was started with.
- Fail closed on validation: refuse the whole call rather than write the valid subset.
- Do not guess at sensitivity: an explicit marking that can be wrong beats a heuristic that is wrong silently.
