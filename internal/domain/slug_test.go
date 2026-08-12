package domain_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

func TestDeriveSlugWithSuffixProducesAUsableHostnameLabel(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	tests := []struct {
		name    string
		appName string
		want    string
	}{
		{"a plain name passes through", "hello", "hello-abc123"},
		{"upper case is lowered", "HelloWorld", "helloworld-abc123"},
		{"a space becomes one dash", "hello world", "hello-world-abc123"},
		{"a run of punctuation collapses to one dash", "hello   ///  world", "hello-world-abc123"},
		{"leading punctuation is dropped rather than starting with a dash", "   hello", "hello-abc123"},
		{"trailing punctuation does not double the dash", "hello!!!", "hello-abc123"},
		{"digits are kept", "app2", "app2-abc123"},
		{"underscores are separators like anything else", "my_app_name", "my-app-name-abc123"},
		{"unicode is dropped rather than transliterated", "café", "caf-abc123"},
		{"a name of pure punctuation still yields a hostname", "!!!", "app-abc123"},
		{"an empty name still yields a hostname", "", "app-abc123"},
		{"a path traversal attempt is just separators", "../../etc/passwd", "etc-passwd-abc123"},
		{"a name that is only unicode falls back", "日本語", "app-abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.DeriveSlugWithSuffix(tt.appName, "abc123"); got != tt.want {
				t.Errorf("DeriveSlugWithSuffix(%q) = %q, want %q", tt.appName, got, tt.want)
			}
		})
	}
}

func TestDeriveSlugWithSuffixTrimsALongNameWithoutLeavingATrailingDash(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	long := strings.Repeat("a", domain.SlugBaseMaxLen+20)

	got := domain.DeriveSlugWithSuffix(long, "abc123")

	base := strings.TrimSuffix(got, "-abc123")
	if len(base) != domain.SlugBaseMaxLen {
		t.Errorf("base is %d characters, want %d", len(base), domain.SlugBaseMaxLen)
	}
	if strings.HasSuffix(base, "-") {
		t.Errorf("base %q ends in a dash, which is not a valid DNS label", base)
	}
}

func TestDeriveSlugWithSuffixTrimsADashLandingOnTheCut(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	// The character at the cap is a separator, so a naive trim would leave the
	// base ending in a dash and the hostname invalid.
	name := strings.Repeat("a", domain.SlugBaseMaxLen-1) + " tail"

	got := domain.DeriveSlugWithSuffix(name, "abc123")

	if strings.Contains(got, "--") {
		t.Errorf("slug %q has a doubled dash", got)
	}
	base := strings.TrimSuffix(got, "-abc123")
	if strings.HasSuffix(base, "-") {
		t.Errorf("base %q ends in a dash", base)
	}
}

func TestASlugIsAlwaysAValidDNSLabel(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	// A slug becomes a hostname label and a namespace name, so anything outside
	// this set makes the deploy fail at the API server rather than here.
	names := []string{
		"hello", "HELLO WORLD", "my_app", "!!!", "", "../../etc/passwd",
		strings.Repeat("z", 200), "app.name.with.dots", "日本語", "a-b-c", "-leading",
	}
	for _, name := range names {
		slug := domain.DeriveSlug(name)
		if !isDNSLabel(slug) {
			t.Errorf("DeriveSlug(%q) = %q, which is not a valid DNS label", name, slug)
		}
		if len(slug) > 63 {
			t.Errorf("DeriveSlug(%q) = %q, which is %d characters and over the DNS label limit",
				name, slug, len(slug))
		}
	}
}

func TestDeriveSlugGivesTwoAppsOfTheSameNameDifferentSlugs(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	seen := map[string]bool{}

	for range 50 {
		seen[domain.DeriveSlug("hello")] = true
	}

	if len(seen) < 45 {
		t.Errorf("50 derivations produced only %d distinct slugs, so the suffix is not random enough", len(seen))
	}
}

func TestRandomSuffixIsFixedLengthAndUnambiguous(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	// No vowels and no lookalikes, so a suffix cannot spell a word and cannot be
	// misread aloud.
	const allowed = "23456789bcdfghjkmnpqrstvwxz"

	for range 200 {
		suffix := domain.RandomSuffix()
		if len(suffix) != domain.SlugSuffixLen {
			t.Fatalf("suffix %q is %d characters, want %d", suffix, len(suffix), domain.SlugSuffixLen)
		}
		for _, r := range suffix {
			if !strings.ContainsRune(allowed, r) {
				t.Fatalf("suffix %q holds %q, which is outside the alphabet", suffix, r)
			}
		}
	}
}

// isDNSLabel reports whether s is lower case alphanumeric with interior dashes,
// which is what both a hostname label and a namespace name have to be.
func isDNSLabel(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
