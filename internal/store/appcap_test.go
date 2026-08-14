package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/toyinogun/deployer/internal/store"
)

// TestTwoCreatesRacingForTheLastSlot is what makes the cap exact rather than
// advisory. The read before the create in internal/mcp is what composes a
// legible refusal; it is not what decides one, because two callers can both pass
// it. The count and the insert run inside one transaction that opens with BEGIN
// IMMEDIATE, so the second writer counts what the first already inserted.
func TestTwoCreatesRacingForTheLastSlot(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	// The fixture already holds one app, so a cap of two leaves exactly one slot
	// for the two callers below to race for.
	const limit = 2
	held, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if held != limit-1 {
		t.Fatalf("the fixture holds %d apps, this test needs exactly %d", held, limit-1)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.CreateApp(ctx, f.account.ID, fmt.Sprintf("racer %d", i), limit)
		}()
	}
	wg.Wait()

	var created, refused int
	for i, err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, store.ErrAppLimit):
			refused++
		default:
			t.Errorf("racer %d failed with something other than the limit: %v", i, err)
		}
	}
	if created != 1 || refused != 1 {
		t.Errorf("%d creates and %d refusals, want exactly one of each", created, refused)
	}
	after, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if after != limit {
		t.Errorf("the account holds %d apps, want %d: a race took it past its cap", after, limit)
	}
}

// TestACreateAtTheCapWritesNothing pins that a refused create leaves no row
// behind, which is what lets a caller retry with the same upload.
func TestACreateAtTheCapWritesNothing(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	before, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if _, err := s.CreateApp(ctx, f.account.ID, "one too many", before); !errors.Is(err, store.ErrAppLimit) {
		t.Fatalf("creating at the cap answered %v, want ErrAppLimit", err)
	}
	after, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if after != before {
		t.Errorf("the refused create left %d apps, want %d", after, before)
	}
}

// TestASoftDeletedAppFreesItsSlot is why the count needs no reaper and no stored
// counter: the predicate that hides a deleted app everywhere else hides it here.
func TestASoftDeletedAppFreesItsSlot(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	held, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if _, err := s.CreateApp(ctx, f.account.ID, "blocked", held); !errors.Is(err, store.ErrAppLimit) {
		t.Fatalf("creating at the cap answered %v, want ErrAppLimit", err)
	}
	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("soft deleting: %v", err)
	}
	if _, err := s.CreateApp(ctx, f.account.ID, "blocked", held); err != nil {
		t.Errorf("the create was still refused after a delete freed a slot: %v", err)
	}
}

// TestTheCountIsScopedToOneAccount pins the count's predicate: another account's
// apps never move this one's number (AC-15).
func TestTheCountIsScopedToOneAccount(t *testing.T) {
	// covers: AC-15
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	stranger, err := s.CreateIdentityAccount(ctx, store.NewIdentityAccount{
		Email: "stranger@example.com", PasswordHash: "argon2id$fake", DisplayName: "stranger",
	})
	if err != nil {
		t.Fatalf("registering the second account: %v", err)
	}
	mine, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	for i := range 3 {
		if _, err := s.CreateApp(ctx, stranger.ID, fmt.Sprintf("theirs %d", i), 100); err != nil {
			t.Fatalf("creating the stranger's app: %v", err)
		}
	}
	after, err := s.CountLiveAppsByAccount(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if after != mine {
		t.Errorf("the count moved from %d to %d because another account created apps", mine, after)
	}

	// The grouped read the admin listing uses answers the same numbers, and an
	// account with no apps is simply absent from it (AC-12).
	grouped, err := s.CountLiveAppsPerAccount(ctx)
	if err != nil {
		t.Fatalf("counting apps per account: %v", err)
	}
	if grouped[f.account.ID] != mine {
		t.Errorf("the grouped count says %d for this account, want %d", grouped[f.account.ID], mine)
	}
	if grouped[stranger.ID] != 3 {
		t.Errorf("the grouped count says %d for the stranger, want 3", grouped[stranger.ID])
	}
}
