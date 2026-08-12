# 0006. Application logs: reasoning and options

Build spec: see [index.md](index.md).

## Context

> ⚠️ Premise note: the scope line for this feature promises logs "with secrets and tokens redacted", and that promise cannot be kept in full. Redacting arbitrary output written by code an AI generated is pattern matching against an open set, so some secrets will get through. The failure mode is not the leak itself, it is a caller trusting the word "redacted" and pasting the output somewhere it should not go. The right framing is defence in depth with an honest label: redact what can be recognised, redact exactly the values the platform itself put in the namespace, and say in the tool's own description that this is best effort. The real fix belongs to slice 7, where the platform learns which values are secret because it injected them.

An agent can now deploy an app and ask how the deploy went, but if the app deploys and then behaves wrong, the trail ends. The reason codes from spec 0005 say why a deploy failed, never why a running app is wrong. Today the only way to see an app's output is for you to open a terminal and run kubectl, which is exactly the thing the platform exists to avoid.

Three forces shape the answer. The first is that the cluster already keeps this data: the kubelet holds a rolling window of each container's output and serves it over the API, so any store the platform builds would be a second copy of something already there. The second is the context window. An agent reads this output into a conversation, so an unbounded response is worse than no response, and the bound has to be in a unit the agent can reason about. The third is that a crash looping app has almost no current output and all of its useful output in the container that just died, and that is the case an agent most needs help with.

There is also a boundary already drawn that this must not cross. Spec 0004 fixed that build output stays in the build Job's pod logs and never reaches a response, the database, or the platform log at info level, because Buildpack output can carry registry credentials. A log tool is the obvious place for that rule to quietly erode.

Not deciding leaves the platform able to run an app it cannot explain, which pushes the person back to kubectl on the first misbehaving deploy.

## Options considered

### Option 1: read the pod's log live from Kubernetes on each call

The tool resolves the app, finds its newest pod, asks the API server for that container's tail with timestamps, then bounds and redacts the result in pure Go. Nothing is stored, and the second read of the terminated container happens only when the container has actually restarted.

**Pros**:
- No new infrastructure, no retention policy, no growth in the SQLite file.
- Bounding and redaction are pure functions, so nearly all of the feature is unit testable without a cluster.
- The rights it needs are already granted and already fenced to app namespaces.

**Cons**:
- Only as good as the node's retention. A replaced or evicted pod takes its output with it.
- No search, no filter, no history beyond what the kubelet still holds.

### Option 2: ship logs into a store the platform queries

Run a collector on the cluster, or write lines into the platform's own database, and serve queries from that. Gives history, search, and output that survives the pod.

**Pros**:
- Answers questions option 1 cannot: what did this app print an hour ago, before the crash that replaced the pod.
- Retention becomes a policy you set rather than a node behaviour you inherit.

**Cons**:
- A whole new operational component to run, or a database that now stores app output. Spec 0002 already calls the SQLite file a secret store with no backup, and this would make it more so.
- Redaction becomes a storage problem rather than a response problem: an unredacted secret is now written to disk and lives there.
- Weeks of work in service of a debugging convenience, on a platform whose deployed apps are largely throwaway.

### Option 3: stream the log through the open MCP call

Hold the tool call open and push new lines as they arrive, so an agent can watch a boot.

**Pros**:
- Best experience for the narrow case of watching an app start.

**Cons**:
- Reintroduces the long open call that slice 2 just removed from `deploy_app`, and reintroduces its timeout problem with it.
- Unbounded by construction, which is the wrong shape for a context window.
- Needs streaming machinery on both sides for a case that polling already covers acceptably.

## Rationale

Option 1 wins on the forces above rather than on elegance. The cluster already holds the data with a rolling window, so option 2 spends real operational work to build a second copy of something that exists, and it does so in the one place the project has already flagged as risky: a database file with no backup that already holds secrets in clear. That cost is not repaid by a homelab platform whose apps are mostly disposable.

Option 3 fails on a decision this project already made deliberately. Slice 2 exists because a long open MCP call ties the agent's client timeout to work of unknown length, and streaming logs recreates exactly that coupling for a smaller benefit. Polling a bounded snapshot is the shape the rest of the surface already teaches an agent.

The two design details worth calling out both come from the crash looping case. Reading the newest pod whatever its phase, rather than the newest ready pod, is what makes a failing rollout report the pod that is failing instead of the old one still serving. Giving the previous container its own separate budget, rather than merging it into one list, is what stops a container that logs a lot on every restart from pushing the actual crash out of the response. Both are one line decisions that determine whether the feature works in the case it was built for.

A third detail earns its place for the same reason: deciding the empty case from pod status before the log API is ever called. Kubernetes returns an ordinary error both for a container that has not started and for a genuine API fault, so a version that infers the empty case from that error either reports a real outage as "nothing to show yet" or reports a crash looping app as a platform failure. Reading status first makes the two cases structurally distinct rather than distinguished by matching error text. The alternative of always attempting the previous container read and treating its error as absence was considered and rejected for the same reason: it trades one clear signal for another error string to interpret.

Keeping the bounds as Go constants rather than environment variables follows the project's own rule read the other way round: the build uid pair is configuration because CI checks it against a pinned image and it genuinely drifts. A default tail length does not drift, and three more validated variables would be startup surface for values nobody will set.
