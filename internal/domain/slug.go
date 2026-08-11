package domain

import (
	"crypto/rand"
	"strings"
)

// SlugBaseMaxLen caps the readable part of a slug before its suffix, so a
// hostname stays comfortably inside DNS label limits.
const SlugBaseMaxLen = 40

// SlugSuffixLen is how many characters of randomness every slug carries.
const SlugSuffixLen = 6

// suffixAlphabet avoids vowels and lookalike characters, so a suffix does not
// accidentally spell something and does not get misread aloud.
const suffixAlphabet = "23456789bcdfghjkmnpqrstvwxz"

// DeriveSlug turns an app name into its permanent slug: lower case, every run of
// non alphanumeric characters collapsed to a single dash, trimmed to
// SlugBaseMaxLen, then a dash and a fresh random suffix. The suffix is always
// applied, so two apps with the same name never collide on readability alone.
// A slug is stored once at create time and never derived again.
func DeriveSlug(name string) string {
	return DeriveSlugWithSuffix(name, RandomSuffix())
}

// DeriveSlugWithSuffix is DeriveSlug with the suffix supplied, so a retry can
// pass a fresh one and a test can pin it.
func DeriveSlugWithSuffix(name, suffix string) string {
	var b strings.Builder
	dashPending := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dashPending && b.Len() > 0 {
				b.WriteByte('-')
			}
			dashPending = false
			b.WriteRune(r)
		default:
			dashPending = true
		}
	}
	base := b.String()
	if len(base) > SlugBaseMaxLen {
		base = strings.TrimRight(base[:SlugBaseMaxLen], "-")
	}
	if base == "" {
		// A name of pure punctuation still has to produce a usable hostname.
		base = "app"
	}
	return base + "-" + suffix
}

// RandomSuffix returns a fresh SlugSuffixLen character slug suffix.
func RandomSuffix() string {
	raw := make([]byte, SlugSuffixLen)
	// crypto/rand.Read never returns an error; it panics internally on failure.
	_, _ = rand.Read(raw)
	out := make([]byte, SlugSuffixLen)
	for i, b := range raw {
		out[i] = suffixAlphabet[int(b)%len(suffixAlphabet)]
	}
	return string(out)
}
