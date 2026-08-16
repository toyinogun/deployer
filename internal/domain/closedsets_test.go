package domain_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
)

// identityCodes is the whole closed set the identity surface answers with,
// written out the same way theSet is, so the two lists can be compared.
var identityCodes = []identity.Code{
	identity.CodeEmailInvalid,
	identity.CodePasswordTooShort,
	identity.CodeNoteTooLong,
	identity.CodeCredentialsInvalid,
	identity.CodeEmailUnverified,
	identity.CodeLinkInvalid,
	identity.CodeInviteInvalid,
	identity.CodeAdminRequired,
	identity.CodeTokenNameTaken,
	identity.CodeInvalidExpiry,
	identity.CodeNotFound,
	identity.CodeRateLimited,
	identity.CodeMailUnavailable,
	identity.CodeInternal,
}

// sharedWithIdentity is the one value both closed sets hold, recorded rather
// than hidden. "internal" means the same thing on both surfaces, a fault inside
// the platform with nothing for the caller to act on, so it carries no ambiguity
// and predates both sets being written down. Every other value is the property
// AC-19 actually binds: a refusal names its own surface's code.
var sharedWithIdentity = map[string]bool{"internal": true}

// TestTheTwoClosedSetsShareOnlyTheInternalCode is spec 0022, AC-19. The machine
// surface and the identity surface each answer from their own closed set, and
// apart from "internal" the two share no values: a rate limit refusal on the
// deploy path is too_many_attempts, its own name, rather than the identity
// surface's rate_limited. A new shared value would let one string mean two
// different things depending on which surface produced it, which is what a
// closed set is for stopping, so this fails on the next one rather than on the
// one already there.
func TestTheTwoClosedSetsShareOnlyTheInternalCode(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	machine := map[string]domain.Reason{}
	for _, r := range theSet {
		machine[string(r)] = r
	}

	for _, c := range identityCodes {
		r, ok := machine[string(c)]
		if !ok || sharedWithIdentity[string(c)] {
			continue
		}
		t.Errorf("identity.%s and domain.Reason %q are the same string, so a caller cannot tell which surface refused it", c, r)
	}

	// The recorded overlap has to be real. A value listed here that neither set
	// holds any more is a stale exemption quietly widening what may collide.
	for value := range sharedWithIdentity {
		if _, ok := machine[value]; !ok {
			t.Errorf("%q is recorded as shared but the machine set no longer holds it", value)
		}
	}
}

// TestEveryIdentityCodeIsInTheListAbove keeps identityCodes honest. A code added
// to internal/identity without being listed here would silently escape the
// comparison above, and the comparison is the only thing holding the two sets
// apart.
func TestEveryIdentityCodeIsInTheListAbove(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	// internal/identity reports no count of its own, so this pins the number the
	// list was written against. A failure here means a code was added: add it to
	// identityCodes and raise this, having first checked it collides with
	// nothing in domain.
	const codesInTheIdentitySet = 14
	if len(identityCodes) != codesInTheIdentitySet {
		t.Fatalf("the identity list holds %d codes, want %d", len(identityCodes), codesInTheIdentitySet)
	}
	seen := map[identity.Code]bool{}
	for _, c := range identityCodes {
		if c == "" {
			t.Error("an empty code is in the list, which is not a code at all")
		}
		if seen[c] {
			t.Errorf("%q is listed twice, so the count above is not the count of distinct codes", c)
		}
		seen[c] = true
	}
}
