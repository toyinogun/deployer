# 0023. Joining: the ready to paste agent configuration, decision record

The build spec is in [index.md](index.md).

## Context

> ⚠️ Premise note: this feature does not make joining non technical, and the scope row already says so. Anyone driving Claude Code or Codex is using a developer's tool, so the coding agent is a higher wall than the configuration file. What it does remove is narrower and real: the one step where a person handles a password by hand and can get it wrong in a way that costs them a working setup or costs the platform a leaked credential. Judge the feature on that, not on making the platform approachable to someone who is not already running an agent.
>
> ⚠️ Premise note: choosing named client tabs means the platform now carries three configuration formats set by someone else's release schedule. That is the only part of this spec that rots on its own, and it rots silently, because a wrong command still renders. It is worth accepting deliberately rather than discovering in a year, and it is why the Follow up asks for a drift check rather than treating the formats as settled.

Registration, verification and token minting all work. What is missing is the last step between a verified person and a working agent. Today they sign in, find `/tokens`, mint, copy a raw token, then work out where their client keeps its MCP server configuration and what shape an entry takes, and paste a password into a file. Spec 0022 removed the step before this one by putting the deploy path on a public hostname, so an agent no longer needs Tailscale. The paste is what is left.

Three forces shape the answer.

The first is that the credential model is settled and worth not disturbing. Spec 0007 made a token an `api_tokens` row with a hashed value, and `/tokens` established the rule that the raw value exists in exactly one response body and nowhere else: no redirect, no URL, no second render. Spec 0015 made registration invite only. Anything that mints a token outside a signed in session, such as an invite link that hands back a ready configuration, is a new credential path with its own threat model, and the scope row itself separates that one click version out as the part that stays unbuilt.

The second is that verification creates no session. `verifyPage` marks the address confirmed and renders a message pointing at `/login`. So the scope line about a newly verified person landing on one page cannot be satisfied at verification time without turning an email link into a session grant, which makes a forwarded or leaked mail an account takeover rather than a wasted verification.

The third is that this is a page whose value is a block of text that is correct. The deploy endpoint appears in that text, and spec 0022 already removed `DEPLOYER_PUBLIC_URL` precisely because a hostname living in two places disagrees eventually. A block that writes the endpoint down is that bug again in a new spot.

## Options considered

### Option 1: A `/connect` page that mints on a button press and renders client tabbed blocks

One new console page, one nullable column marking whether the person has been there, and a redirect on the first sign in after verifying. The page renders a finished configuration block per named client, with a placeholder where the token goes; a button mints and re renders the same blocks with the real token substituted, once.

**Pros**:

- The token is an ordinary token at every layer below the page. One token table, one list, one revoke path, one authenticator, so no second security model.
- The page stays useful after onboarding. A second machine gets a real block rather than a remembered one.
- The credential discipline is inherited from `/tokens` rather than restated, so there is no second way for the one response body rule to be got wrong.
- Named tabs mean the person copies something that works in their client, not something they have to translate.

**Cons**:

- Three client formats the platform does not own, going stale silently.
- A write on a `GET`, so a prefetch or link preview can consume the one time redirect.
- Tabs and copy depend on JavaScript for the ordinary path.

### Option 2: Extend the existing `/tokens` one time panel with the blocks

No new page, no new column, no redirect. The panel `/tokens` already shows after a mint gains the client blocks with the token in them.

**Pros**:

- Smallest change by a distance, and no schema change at all.
- Exactly one page mints browser credentials, which is a genuinely cleaner story than two.
- Nothing new to route, so nothing new to remember to register on the console host.

**Cons**:

- Leaves the step that actually goes wrong in place. Someone new still has to find `/tokens`, a page about credential administration, and work out that minting is what they want. The scope's whole complaint is that they do not know to do that.
- The configuration reference only exists in the moment after a mint. There is no page to return to and read.

### Option 3: An invite link that mints the token and returns a ready configuration

The one click version. The invite mail carries a link that, when opened, mints a token and shows the finished block, with no sign in.

**Pros**:

- Genuinely one thing. The fewest steps any option here reaches.
- Solves the case this feature explicitly does not: someone who has not yet worked out the console at all.

**Cons**:

- A new credential path. A link in a mailbox that mints a deploy capable token is a different threat model from a signed in post, and it needs its own expiry, single use, and revocation reasoning.
- The scope row already carves this out as the part that stays unbuilt, so choosing it here quietly expands the feature into the thing that was deferred.

## Rationale

Option 1 is chosen because it is the only one that solves the stated problem without touching the credential model. The forces above pull in one direction: the token machinery from spec 0007 is settled and the one response body rule in `/tokens` is load bearing, so the right move is a new surface over the existing mint rather than a new way to mint. Option 3 fails on exactly that, and the scope row already separated it out for the same reason.

Option 2 was the closest call, and it wins on simplicity. It loses on the specific thing the feature exists for. The scope's claim is not that minting is hard, it is that a person who has just verified does not know minting is the next thing, and then does not know where the result goes. A panel that only exists in the moment after a deliberate mint on a page called `/tokens` cannot reach someone who never went there. A page they are sent to on their first sign in can, and the same page answers the second machine question later, which the panel cannot.

Two smaller calls follow from the same reasoning. The redirect hangs off a stamped column rather than off token history, because inferring it from an account having never held a token silently re fires for anyone who revokes their last one, which turns a one time onboarding nudge into a recurring one triggered by ordinary administration. And the stamp lands on the first page serve rather than on the first mint, because stamping on mint means someone who deliberately skipped the page is sent back to it every single sign in with no way to decline; the cost of the choice made is that a prefetch can spend the redirect, which is worth naming and is cheap next to nagging.

An independent cross check on a second model found three things this record should carry, because each is a decision rather than a detail. The first is that a fixed default token name cannot work: `identity.MintToken` refuses a name an account already holds live, so a second mint from the same tab would be refused by a rule written for a different purpose. Dating the name and appending an ordinal keeps the name useful for telling machines apart, which is the whole reason it is not just a random string. The second is that the sign in redirect must not swallow a `next`, because that value only exists when the session gate put it there, meaning the person was already trying to reach a specific page; taking them somewhere else drops an intent nothing can recover, and since they go unstamped, the next plain sign in still delivers them to `/connect`. The third is that the stamp has to be one conditional write rather than a read and then a write, or two tabs opened at once race on a marker whose entire job is being written once.

The engineer chose named client tabs over a single generic block, against the lower maintenance option. That is the right call for the feature's purpose, since a generic JSON block puts the person straight back into finding and editing a config file, which is the step being removed. The tradeoff being accepted consciously is three formats that go stale on someone else's schedule and fail silently when they do, which is why it is written into Consequences and carries a Follow up rather than being absorbed quietly.
