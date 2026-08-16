package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// boundInvitesFor counts the invite rows bound to one address.
func boundInvitesFor(t *testing.T, s *store.Store, email string) int {
	t.Helper()
	rows, err := s.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Email != nil && *r.Email == email {
			n++
		}
	}
	return n
}

// TestNoConcurrentMintOutrunsTheTakenAddressGuard is the race behind AC-3. The
// read that asks whether an address already has an account and the insert that
// binds an invite to it are one transaction on purpose, so a registration
// landing between them cannot leave a live invite addressed to somebody who is
// already here. Held apart, that window is a few microseconds wide and every
// sequential test in the suite passes straight through it.
//
// Two halves, because they fail differently. The first is deterministic: the
// address is taken before anything starts, so every concurrent mint must be
// refused and none of them may write. The second races the registration against
// the mint, where both orders are legal, and pins the part that is not: the
// answer and the row have to agree, so a refusal never leaves a row behind and a
// success is never rolled back under it.
// covers: AC-3
func TestNoConcurrentMintOutrunsTheTakenAddressGuard(t *testing.T) {
	const taken = "taken@example.com"

	t.Run("every concurrent mint to a taken address is refused", func(t *testing.T) {
		s, clock := newStore(t)
		register(t, s, taken)

		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.CreateInvite(t.Context(), store.NewInvite{
					CodeHash:  "hash-" + string(rune('a'+i)),
					Email:     taken,
					ExpiresAt: ids.Stamp(clock.Now().Add(time.Hour)),
				})
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if !errors.Is(err, store.ErrAddressRegistered) {
				t.Errorf("mint %d: got %v, want ErrAddressRegistered", i, err)
			}
		}
		if got := boundInvitesFor(t, s, taken); got != 0 {
			t.Errorf("%d invites were bound to an address that already has an account", got)
		}
	})

	t.Run("a mint racing the registration answers what it wrote", func(t *testing.T) {
		// Fresh store per round, because the two writers race for the same
		// address and the losing order has to be reachable again.
		for round := range 12 {
			s, clock := newStore(t)

			var wg sync.WaitGroup
			var mintErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, mintErr = s.CreateInvite(t.Context(), store.NewInvite{
					CodeHash:  "hash",
					Email:     taken,
					ExpiresAt: ids.Stamp(clock.Now().Add(time.Hour)),
				})
			}()
			go func() {
				defer wg.Done()
				<-start
				if _, err := s.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
					Email: taken, PasswordHash: "argon2id$fake", DisplayName: "someone",
				}); err != nil {
					t.Errorf("round %d: registering: %v", round, err)
				}
			}()
			close(start)
			wg.Wait()

			rows := boundInvitesFor(t, s, taken)
			switch {
			case mintErr == nil && rows != 1:
				t.Fatalf("round %d: the mint reported success but left %d rows", round, rows)
			case errors.Is(mintErr, store.ErrAddressRegistered) && rows != 0:
				t.Fatalf("round %d: a refused mint left %d rows behind", round, rows)
			case mintErr != nil && !errors.Is(mintErr, store.ErrAddressRegistered):
				t.Fatalf("round %d: the mint failed with something other than a refusal: %v", round, mintErr)
			}
		}
	})
}
