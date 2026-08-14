# 0015 · Invite only registration: rationale

_The decision record behind [index.md](index.md): why this shape, and what lost._

## Context

Registration is the only place on the platform where a stranger creates state without holding a credential. That was a deliberate, recorded tradeoff: the tailnet was the outer fence, and being able to reach the page was itself the authorisation. Slice 12 exists to rebuild each control the tailnet was quietly providing, and this is the first of them, because it is the one that decides who is inside at all.

The forces that shape the answer are small and specific. The platform holds under ten people in its first months, so this is a door for people you know personally, not a growth funnel. The identity surface already has the exact machinery an invite needs: a single use secret stored as a hash, a purpose checked alongside the hash, an expiry, and a consumed stamp, all built for `email_tokens` in spec 0007. There are two doors onto registration, the page and `POST /v1/auth/register`, and the project's rule is that neither is ever weaker than the other. And there is one thing that has no precedent: a fresh database has no admin, so nothing can issue the first invite, and the platform would be permanently closed to everyone.

Not deciding leaves the front door open at the moment the address becomes publicly resolvable, which is the one ordering mistake in slice 13 that cannot be undone quietly.

## Options considered

### Option 1: An invite row checked at the top of the existing register path

A new `invites` table holding a hashed single use code, checked as the first step of `Service.Register` and spent inside the account creation transaction. The admin gets a page to mint, list, and revoke.

**Pros**:
- Reuses the exact single use secret shape spec 0007 already built, tested, and operates: hash the secret, store the hash, match on hash plus state.
- The invite is a first class row, so revoke, expiry, and who invited whom are all just columns rather than inferred facts.
- One insertion point in the service means both surfaces are gated by construction, not by remembering to gate two handlers.

**Cons**:
- A new table, a new migration, a new admin page, and a new refusal code: the largest of the three options.
- The bootstrap case has to be solved, because an empty database has no admin to mint the first one.

### Option 2: A shared signup password in configuration

A single `DEPLOYER_SIGNUP_SECRET` that every registration must present.

**Pros**:
- No schema change, no admin page, no new lifecycle. Perhaps thirty lines in total.
- Nothing to bootstrap: the value is in the deploy Secret before the first boot.

**Cons**:
- One secret for everyone means it cannot be revoked for one person, only rotated for all of them, and every rotation is a deploy.
- Nothing records who let a person in, which the scope row asks for outright.
- A shared secret spreads. The first time somebody forwards the sentence "the signup password is X" the fence is gone and there is no way to know.

### Option 3: An allow list of email addresses

Registration is refused unless the address is on a list an admin maintains.

**Pros**:
- No secret to leak: knowing the address is not enough, you must already be on the list.
- Reads naturally on an admin page, since the unit is a person rather than a link.

**Cons**:
- You must know the exact address a person will use, and people have several. The first mismatch is a support conversation, and the failure mode looks identical to being uninvited.
- It gives the person nothing to click, so joining is still explain the URL, explain the flow, rather than send one link.
- It does not remove the need for the row anyway, since expiry and revocation are still wanted.

## Rationale

The forces from Context point the same way. The identity package already holds the single use secret pattern this needs, so Option 1 is mostly assembly of a shape the codebase has, rather than a new mechanism: `HashSecret`, a live row predicate, a consumed stamp, and a link built off `PublicURL` are all in place. That matters more than the line count difference with Option 2, because the thing being built is the platform's outermost security boundary and the safest version of it is the one that looks like the code beside it.

Option 2 loses on revocation and on the record. With under ten people, the realistic incident is not a mass attack, it is one link ending up somewhere it should not, and the only useful response to that is revoking one thing while everyone else stays working. A shared secret cannot do that. It also cannot answer who invited whom, which the scope row names as a requirement and which is the only thing that makes an audit trail worth reading after the fact.

Option 3 was the closest call, and it is worth naming why it lost. Binding to an address is genuinely tighter if a link leaks. But the invite is a full strength random secret sent to one person over a channel of your choosing, which is the same threat model the verify and reset links already accept, and binding costs you knowing the exact address up front. For a platform where you already know the people personally, the leak risk is small and the friction is certain, so the tighter option buys little and costs on every single invite.

Two calls that were mine rather than yours. `invite_invalid` maps to **403 Forbidden**, not the 400 that `link_invalid` carries: this is an authorisation decision about whether the caller may register at all, and the closest existing pairing is `admin_required`, which is also a 403. The runner up was 400 for symmetry with the other single use link, which reads consistently but tells a caller their input was malformed when it was in fact simply not allowed. And the spend is a conditional `UPDATE` guarded on the live predicate inside the account transaction, rather than a read then write: SQLite gives you the atomicity for free there, and it is the same trick `CreateIdentityAccount` already uses to make the first admin rule race safe.
