package store_test

import (
	"errors"
	"testing"

	"github.com/toyinogun/deployer/internal/store"
)

func TestSetConfigBatchWritesEveryKeyOrNoneOfThem(t *testing.T) {
	// covers: spec 0010 AC-1
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "ONE", Value: "1"},
		{Key: "TWO", Value: "2", IsSecret: true},
	}); err != nil {
		t.Fatalf("writing two keys: %v", err)
	}
	if got := configKeys(t, s, f.app.ID); len(got) != 2 {
		t.Fatalf("after the batch the app holds %v, want two keys", got)
	}

	// The store is the backstop rather than the reporter, but a bad key still has
	// to take the whole transaction down with it.
	err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "THREE", Value: "3"},
		{Key: "lower", Value: "4"},
	})
	if !errors.Is(err, store.ErrInvalidKey) {
		t.Fatalf("a batch holding a bad key returned %v, want ErrInvalidKey", err)
	}
	if got := configKeys(t, s, f.app.ID); len(got) != 2 {
		t.Fatalf("the refused batch left %v behind, want the original two keys", got)
	}
}

func TestUnsetConfigBatchRemovesEveryKeyOrNoneOfThem(t *testing.T) {
	// covers: spec 0010 AC-3
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "ONE", Value: "1"},
		{Key: "TWO", Value: "2"},
	}); err != nil {
		t.Fatalf("seeding the configuration: %v", err)
	}

	err := s.UnsetConfigBatch(ctx, f.app.ID, []string{"ONE", "NEVER_SET"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsetting a key that is not set returned %v, want ErrNotFound", err)
	}
	if got := configKeys(t, s, f.app.ID); len(got) != 2 {
		t.Fatalf("the refused unset left %v, want both keys still set", got)
	}

	if err := s.UnsetConfigBatch(ctx, f.app.ID, []string{"ONE", "TWO"}); err != nil {
		t.Fatalf("unsetting both keys: %v", err)
	}
	if got := configKeys(t, s, f.app.ID); len(got) != 0 {
		t.Fatalf("after unsetting both the app still holds %v", got)
	}
}

// configKeys is the app's keys as the deploy path reads them.
func configKeys(t *testing.T, s *store.Store, appID string) []string {
	t.Helper()
	entries, err := s.ListConfigForDeploy(t.Context(), appID)
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	return keys
}
