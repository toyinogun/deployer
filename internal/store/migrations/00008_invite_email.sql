-- Spec 0025. The address an invite is bound to.
--
-- Purely additive, one nullable column on one existing table with no default and
-- no constraint, so a previous binary reads the schema it leaves behind unharmed
-- and no existing row needs a value (AC-16). Every invite that already exists is
-- null here, which is exactly what those invites are: unbound, usable by
-- whichever address registers with them. The platform's own bootstrap invite is
-- unbound permanently.
--
-- No index and no uniqueness. Nothing looks an invite up by address: the lookup
-- is still by code_hash and the address only narrows that row's own predicate.
-- Two live invites may share an address, which is harmless, since whichever link
-- is used first spends its own row and the other expires unspent.

-- +goose Up

-- Null means unbound. A value is the normalized address this invite authorises,
-- written once at mint and never edited: the match in LiveInviteByCodeHash rests
-- on that immutability, which is why it lives in the lookup rather than also in
-- the spend guard, where expiry has to be rechecked because expiry moves.
ALTER TABLE invites ADD COLUMN email TEXT;

-- +goose Down
ALTER TABLE invites DROP COLUMN email;
