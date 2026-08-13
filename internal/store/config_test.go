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

func TestOnlyTheDeployReadPathEverReturnsASecretValue(t *testing.T) {
	// covers: spec 0010 AC-2
	// The split between the two read methods is the invariant, and it is settled
	// in SQL rather than in Go so that no caller can forget it. Nothing above this
	// layer proves it: every test up there runs against a stub that was told to
	// behave this way.
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "API_KEY", Value: "hunter2", IsSecret: true},
		{Key: "LOG_LEVEL", Value: "debug"},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}

	response, err := s.ListConfigForResponse(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading for a response: %v", err)
	}
	deployed, err := s.ListConfigForDeploy(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading for a deploy: %v", err)
	}
	if len(response) != 2 || len(deployed) != 2 {
		t.Fatalf("the two reads returned %d and %d entries, want both keys from each", len(response), len(deployed))
	}

	for _, e := range response {
		switch e.Key {
		case "API_KEY":
			// The key and its flag are the whole answer. The value is not withheld
			// in Go, it never arrives.
			if !e.IsSecret || e.Value != "" {
				t.Errorf("the response read returned %+v, want the flag and no value", e)
			}
		case "LOG_LEVEL":
			if e.IsSecret || e.Value != "debug" {
				t.Errorf("the response read withheld a value nobody called secret: %+v", e)
			}
		}
	}
	for _, e := range deployed {
		if e.Key == "API_KEY" && e.Value != "hunter2" {
			t.Errorf("the deploy read returned %+v, want the real value the container needs", e)
		}
	}
}

func TestCurrentReleaseConfigIsWhatTheRunningReleaseRanWith(t *testing.T) {
	// covers: spec 0010 AC-11
	// This read is where a rotated secret survives: the pod that is running
	// printed the old value, and once the key is set again nothing else holds it.
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	// An app that has never run is an empty configuration rather than an error.
	before, err := s.CurrentReleaseConfig(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the release configuration of an app with no release: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("an app that never ran reports %+v", before)
	}

	if err := s.SetConfig(ctx, f.app.ID, "API_KEY", "oldsecretvalue", true); err != nil {
		t.Fatalf("setting the secret: %v", err)
	}
	deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	// Rotating the key leaves the running release untouched, which is the point.
	if err := s.SetConfig(ctx, f.app.ID, "API_KEY", "newsecretvalue", true); err != nil {
		t.Fatalf("rotating the secret: %v", err)
	}
	ran, err := s.CurrentReleaseConfig(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the release configuration: %v", err)
	}
	if ran["API_KEY"] != "oldsecretvalue" {
		t.Errorf("the running release reports API_KEY as %q, want the value the pod was started with", ran["API_KEY"])
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
