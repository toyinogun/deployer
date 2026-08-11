package domain_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

func TestCanTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from domain.State
		to   domain.State
		want bool
	}{
		{"queued starts a build", domain.StateQueued, domain.StateBuilding, true},
		{"queued takes the rollback shortcut", domain.StateQueued, domain.StateDeploying, true},
		{"queued cannot skip to healthy", domain.StateQueued, domain.StateHealthy, false},
		{"building pushes", domain.StateBuilding, domain.StatePushing, true},
		{"building cannot go straight to deploying", domain.StateBuilding, domain.StateDeploying, false},
		{"pushing deploys", domain.StatePushing, domain.StateDeploying, true},
		{"deploying becomes healthy", domain.StateDeploying, domain.StateHealthy, true},
		{"healthy never goes back to building", domain.StateHealthy, domain.StateBuilding, false},
		{"failed is terminal", domain.StateFailed, domain.StateQueued, false},
		{"cancelled is terminal", domain.StateCancelled, domain.StateDeploying, false},
		{"nothing moves to itself", domain.StateBuilding, domain.StateBuilding, false},
		{"an unknown target is refused", domain.StateQueued, domain.State("pending"), false},
		{"an unknown source is refused", domain.State("pending"), domain.StateBuilding, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestEveryStateReachesFailedAndCancelled(t *testing.T) {
	t.Parallel()
	for _, from := range []domain.State{domain.StateQueued, domain.StateBuilding, domain.StatePushing, domain.StateDeploying} {
		for _, to := range []domain.State{domain.StateFailed, domain.StateCancelled} {
			if !domain.CanTransition(from, to) {
				t.Errorf("a deployment stuck in %q cannot reach %q", from, to)
			}
		}
	}
}

func TestTerminalAndValid(t *testing.T) {
	t.Parallel()
	all := []domain.State{
		domain.StateQueued, domain.StateBuilding, domain.StatePushing,
		domain.StateDeploying, domain.StateHealthy, domain.StateFailed, domain.StateCancelled,
	}
	for _, s := range all {
		if !s.Valid() {
			t.Errorf("%q should be a valid state", s)
		}
	}
	if domain.State("pending").Valid() {
		t.Error("pending is not one of the seven states")
	}
	for _, s := range []domain.State{domain.StateHealthy, domain.StateFailed, domain.StateCancelled} {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []domain.State{domain.StateQueued, domain.StateBuilding, domain.StatePushing, domain.StateDeploying} {
		if s.Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

func TestDeriveSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string // the part before the suffix
	}{
		{"lower cases", "MyApp", "myapp"},
		{"collapses a run of punctuation to one dash", "my   fancy___app!!", "my-fancy-app"},
		{"drops leading punctuation", "!!hello", "hello"},
		{"drops trailing punctuation", "hello!!", "hello"},
		{"keeps digits", "app2you", "app2you"},
		{"falls back when nothing survives", "!!!", "app"},
		{"trims a long name", strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := domain.DeriveSlugWithSuffix(tt.in, "abc123")
			if want := tt.want + "-abc123"; got != want {
				t.Errorf("DeriveSlugWithSuffix(%q) = %q, want %q", tt.in, got, want)
			}
		})
	}
}

func TestDeriveSlugAlwaysAddsAFreshSuffix(t *testing.T) {
	t.Parallel()
	// Two apps with the same name must not derive the same slug, which is what
	// makes the collision retry a rare event rather than a certainty.
	a, b := domain.DeriveSlug("checkout"), domain.DeriveSlug("checkout")
	if a == b {
		t.Fatalf("two slugs for the same name collided: %q", a)
	}
	for _, s := range []string{a, b} {
		if !strings.HasPrefix(s, "checkout-") {
			t.Errorf("%q lost its readable base", s)
		}
		if got := len(s) - len("checkout-"); got != domain.SlugSuffixLen {
			t.Errorf("%q has a %d character suffix, want %d", s, got, domain.SlugSuffixLen)
		}
	}
}
