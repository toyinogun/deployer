# 0025. Emailed invites bound to an address: decision record

The build spec is in [index.md](index.md).

## Context

Spec 0015 made registration invite only and stopped there deliberately: an admin mints an invite, the page shows the link exactly once, and handing it over is a manual message the admin writes. The invite itself is anonymous. It authorises whoever holds the link, and the `note` column is the only record of who it was meant for, kept for the admin's memory rather than enforced by anything.

Two forces make that worth changing now. The platform is on the open internet since spec 0021, so a link pasted into a chat, forwarded, or read out of a device is a working account on a cluster that can run arbitrary workloads. And the invite is currently the widest credential the platform issues: it is the only one that is not tied to anything, where a session belongs to an account, an API token belongs to an account, and a verification link belongs to an address. The person the admin had in mind is nowhere in the enforcement path.

The delivery half is a smaller force but the one that prompted this. Copying a link out of an admin page and into a message is a step the platform could do itself, and it already has everything it needs: a Resend sender, a From address, and a console hostname the links are already derived from.

The constraint that shapes everything is spec 0015 AC-14, which states that a raw invite code exists in exactly two places, the admin response and the bootstrap log line, and never in a mailed message. That rule was written when nothing needed to mail one. Mailing an invite is the whole point of this feature, so the rule has to be consciously amended rather than quietly broken. The second constraint is spec 0015 AC-2, which makes all five ways an invite can be bad answer in identical words, so that a holder cannot learn whether the code they hold is live.

## Options considered

### Option 1: A nullable address column on the existing invite row, compared inside `Register`

Add one nullable `email` column to `invites`. A mint with an address stores it and sends the link to it inline, and the invite lookup carries the registering address in its own predicate, so a bound invite simply does not match the wrong one. A mismatch is therefore the existing `invite_invalid`, from the same query as every other bad invite. An empty address mints exactly as today.

**Pros**:
- The smallest change that delivers both halves. One column, one code, one message, one comparison.
- Unbound stays a first class state, so the boot time bootstrap invite and any hand delivered link need no special case and no backfill.
- The match lands in the lookup both register surfaces already share, so neither surface can end up with a rule the other lacks, and the refusal cannot drift apart from the other four by cost or by wording.
- The refusal reuses the existing uniform answer, so the property spec 0015 AC-2 protects is extended rather than punctured.

**Cons**:
- A live credential travels by mail, amending spec 0015 AC-14.
- The address of someone who never registered sits in the invite table indefinitely, and nothing deletes invite rows.
- The mint request blocks on an outside service for the first time.

### Option 2: Mail a second, separate token that redeems the invite

Keep the invite code unmailed. The message carries its own single use token, stored alongside the invite, which the register page exchanges for the invite server side.

**Pros**:
- Spec 0015 AC-14 stays literally true, with no amendment to write or explain.
- The mailed token could be given a shorter life than the invite it redeems.

**Cons**:
- No real gain in safety. Whoever reads the mailbox registers either way, so the property AC-14 was protecting is already gone the moment a link is mailed at all; only the wording survives.
- A second secret with its own lifetime, storage, revocation and expiry, plus a redemption step that can fail on its own, for a feature whose whole content is one comparison.
- The register page would have to resolve the token before rendering, which reintroduces exactly the GET time validation spec 0015 AC-18 removed.

### Option 3: A separate `invitations` concept alongside invites

Leave the invite mechanism alone and build a new bound, mailed invitation type beside it, retiring the old one later.

**Pros**:
- No change at all to a path that is built, tested and working.
- The strangler shape, which is usually right for replacing something in production.

**Cons**:
- Two mechanisms that mean the same thing, two admin surfaces, two refusal paths, and a register path that has to check both.
- The strangler pattern earns its cost when the old thing is being replaced. Here it is being extended by one optional field, and nothing about the old behaviour is wrong.
- The bootstrap invite would still need the old mechanism, so the old one never retires.

## Rationale

Option 1 wins on the shape of the change rather than on preference. The gap is not that the invite mechanism is wrong, it is that the invite carries no address, so the fix is one field on a row that already exists and one comparison on a path that already runs. Options 2 and 3 both build a second thing to avoid touching the first, and in both cases the second thing has to keep the first alive anyway, because the platform mints itself an unbound invite at boot on an empty database and always will.

The address being optional rather than required follows from that same boot time invite. Since the unbound path cannot be removed, making the field required would leave both mechanisms in the code while pretending only one exists. Optional keeps the two states visible, and a blank address column in the list is a useful signal of which invites are named.

Refusing a mismatch as `invite_invalid` rather than as its own code is the one place this spec chooses the worse experience on purpose. The invited person who mistypes their address gets a message that does not explain itself. The alternative tells anyone holding a link that the code behind it is live and only the address is wrong, which is precisely the distinction spec 0015 AC-2 spent its uniformity budget removing. The person who needs the explanation already has the address in front of them, in the message they just opened.

Sending inline, and reporting the outcome, departs from the platform's own mail rule, and the departure is narrow and reasoned. Verify and reset mail is fire and forget because the account or the link is already committed and the caller is a browser mid form, with nothing useful to do about a provider failure. Here the caller is an admin looking at a page that is holding the link, and knowing the send failed is what tells them to copy it. The cost, a page that can wait ten seconds on Resend, is bounded by the sender's existing timeout and lands on an admin page rather than on the deploy path.

Refusing the mint when the address already has an account is the one recommendation the engineer overrode, and it is the better call. Without the check the invite is minted, mailed, and dead on arrival: registering on a taken address answers exactly like a fresh registration and leaves the invite unspent, so the person receives a link that silently does nothing and the admin has no way to learn that. The check costs one read on an admin only surface where saying plainly that the address is taken leaks nothing.

The match lives in the lookup query rather than in Go, and that is the correction a cross check forced on the first draft of this spec. The draft compared the addresses in `Register`, after `CheckEmail`, which made the uniformity claim untrue: a dead code returns from the lookup immediately, while a bound code with the wrong address would first pass the lookup, then run validation, then fail a comparison. More work, a longer response, and a different code path for what is supposed to be the same refusal. Folding the address into the lookup's own `WHERE` makes the two cases one query returning one error at one cost, so the property holds by construction rather than by careful ordering that a later edit could undo. It also means no bound address is ever read upward on the register path, so no handler can branch on one.

Normalizing the submitted address before the lookup, rather than validating it, is what lets that work. `NormalizeEmail` is free, so spec 0015 AC-11 is untouched, and `CheckEmail` still runs afterwards for format. The one thing this does not fix is that a malformed address plus a live unbound invite still answers `email_invalid` where a dead code answers `invite_invalid`, which is a liveness oracle spec 0015 already had. Closing it means moving `CheckEmail` behind the lookup, which changes what 0015 AC-1 promises, so it is recorded as a follow up rather than smuggled in here.

The taken address check moved inside the `CreateInvite` transaction for the same reason `CreateApp` counts inside its own: the store's convention is that a guard deciding whether a write is allowed holds the write lock while it decides. The stakes here are small, a race produces a bound invite that is dead on arrival rather than anything corrupt, but a documented invariant with one quiet exception stops being an invariant.

Not building a resend follows from where the code lives. After the mint response the platform holds only the hash, so re-sending means either storing the raw code, which breaks spec 0015 AC-14 in the way that actually matters rather than the way this spec amends, or minting a fresh code onto the old row, which makes an invite row stop meaning one code and leaves its `created_at` describing a link that no longer works. Revoke and mint again is one more click and keeps both properties.
