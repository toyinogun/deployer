package domain_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// TestReservedLabelRefusesTheNamesThePlatformKeeps is AC-6. The set is a
// constant, so a scratch instance and the live one refuse the same names.
func TestReservedLabelRefusesTheNamesThePlatformKeeps(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	for _, name := range []string{"console", "www", "api", "admin", "mcp", "app", "deployer", "registry"} {
		if !domain.ReservedLabel(name) {
			t.Errorf("%q is not refused, so an app could take a name the platform keeps", name)
		}
	}
}

// TestReservedLabelRunsOnTheDerivedBase is AC-7. A display name that derives to a
// reserved label is refused just as a literal one is, so the refusal cannot be
// walked around with capitals or punctuation.
func TestReservedLabelRunsOnTheDerivedBase(t *testing.T) {
	// covers: AC-7
	t.Parallel()
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"Console", true},
		{"  console  ", true},
		{"C.O.N.S.O.L.E", false},
		{"console!", true},
		{"MCP", true},
		{"console-shop", false},
		{"my console", false},
		{"consoles", false},
		{"hello", false},
		// A name with nothing usable in it falls back to the base `app`, which is
		// itself reserved, so it is refused rather than silently turned into
		// `app-x7k2q9`. That is the better answer: the caller is told to pick a
		// name instead of getting a hostname that names nothing.
		{"", true},
		{"...", true},
	} {
		if got := domain.ReservedLabel(tc.name); got != tc.want {
			t.Errorf("ReservedLabel(%q) = %v, want %v (base %q)", tc.name, got, tc.want, domain.DeriveBase(tc.name))
		}
	}
}

// TestDeriveBaseIsTheSlugWithoutItsSuffix keeps the two derivations from
// drifting. The reserved check asks about the base, so a base that stopped being
// the head of the slug would refuse names that are fine and admit names that are
// not.
func TestDeriveBaseIsTheSlugWithoutItsSuffix(t *testing.T) {
	// covers: AC-7
	t.Parallel()
	for _, name := range []string{"console", "Hello World", "...", "a-very-long-name-that-runs-past-the-cap-and-keeps-going"} {
		base := domain.DeriveBase(name)
		if got := domain.DeriveSlugWithSuffix(name, "abcdef"); got != base+"-abcdef" {
			t.Errorf("DeriveSlugWithSuffix(%q) = %q, want %q", name, got, base+"-abcdef")
		}
	}
}
