package identity_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
)

func TestCheckEmail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		code identity.Code
	}{
		{"plain", "a@example.com", "a@example.com", ""},
		{"upper cased is normalized", "  A@Example.COM ", "a@example.com", ""},
		{"empty", "", "", identity.CodeEmailInvalid},
		{"no at sign", "not-an-address", "", identity.CodeEmailInvalid},
		{"display name is refused, not stripped", "Someone <a@example.com>", "", identity.CodeEmailInvalid},
		{"too long", strings.Repeat("a", 250) + "@example.com", "", identity.CodeEmailInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := identity.CheckEmail(tc.in)
			code, isRefusal := identity.CodeOf(err)
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case tc.code != "" && (!isRefusal || code != tc.code):
				t.Fatalf("got code %q, want %q", code, tc.code)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckPassword(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"eleven is short", strings.Repeat("a", 11), false},
		{"twelve is the floor", strings.Repeat("a", 12), true},
		{"long is fine", strings.Repeat("a", 200), true},
		{"absurd is refused", strings.Repeat("a", 2000), false},
		{"no composition rule applies", "aaaaaaaaaaaa", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := identity.CheckPassword(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if !tc.ok {
				code, isRefusal := identity.CodeOf(err)
				if !isRefusal || code != identity.CodePasswordTooShort {
					t.Fatalf("got code %q, want %q", code, identity.CodePasswordTooShort)
				}
			}
		})
	}
}

func TestDisplayNameFor(t *testing.T) {
	if got := identity.DisplayNameFor("  Toyin ", "a@example.com"); got != "Toyin" {
		t.Errorf("given name: got %q, want %q", got, "Toyin")
	}
	if got := identity.DisplayNameFor("", "toyin@example.com"); got != "toyin" {
		t.Errorf("absent name: got %q, want the local part %q", got, "toyin")
	}
}

func TestHashVerifiesAndIsSalted(t *testing.T) {
	h := identity.NewHasher(2)
	const password = "correct horse battery"

	first, err := h.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	second, err := h.Hash(password)
	if err != nil {
		t.Fatalf("hashing again: %v", err)
	}
	if first == second {
		t.Error("two hashes of one password are identical, so the salt is not random")
	}
	if strings.Contains(first, password) {
		t.Error("the encoded hash carries the password")
	}
	if !h.Verify(password, first) {
		t.Error("the right password did not verify")
	}
	if h.Verify("correct horse batteryy", first) {
		t.Error("a wrong password verified")
	}
}

// TestVerifyRefusesAnEmptyHash is AC-11 at the unit level: the bootstrap account
// holds no password hash, and signing in as it must be refused.
func TestVerifyRefusesAnEmptyHash(t *testing.T) {
	h := identity.NewHasher(2)
	if h.Verify("anything at all", "") {
		t.Error("an account with no password hash was signed into")
	}
	if h.Verify("anything at all", "not-an-argon2id-string") {
		t.Error("a malformed stored hash verified")
	}
}

// TestVerifyReadsBackItsOwnParameters proves the encoded string carries them, so
// a later parameter change leaves existing hashes verifiable.
func TestVerifyReadsBackItsOwnParameters(t *testing.T) {
	h := identity.NewHasher(1)
	encoded, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=") {
		t.Fatalf("encoded hash is %q, want the argon2id encoding", encoded)
	}
	if !strings.Contains(encoded, "m=19456,t=2,p=1") {
		t.Errorf("encoded hash does not carry the OWASP minimum parameters: %q", encoded)
	}
}

func TestSecretsAreDistinctAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s, err := identity.NewSecret()
		if err != nil {
			t.Fatalf("drawing a secret: %v", err)
		}
		if seen[s] {
			t.Fatal("two secrets came out the same")
		}
		seen[s] = true
	}
	tok, err := identity.NewAPIToken()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if !strings.HasPrefix(tok, identity.APITokenPrefix) {
		t.Errorf("token %q is not marked with %q", tok, identity.APITokenPrefix)
	}
	if identity.HashSecret(tok) == tok {
		t.Error("hashing a token returned it unchanged")
	}
}

func TestTokenExpiry(t *testing.T) {
	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		days int
		set  bool
		code identity.Code
	}{
		{"absent means never expires", 0, false, ""},
		{"one day is the floor", 1, true, ""},
		{"a year is the ceiling", 365, true, ""},
		{"beyond the ceiling", 366, false, identity.CodeInvalidExpiry},
		{"negative", -1, false, identity.CodeInvalidExpiry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at, set, err := identity.TokenExpiry(now, tc.days)
			if tc.code != "" {
				code, isRefusal := identity.CodeOf(err)
				if !isRefusal || code != tc.code {
					t.Fatalf("got code %q, want %q", code, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if set != tc.set {
				t.Fatalf("set is %v, want %v", set, tc.set)
			}
			if set && !at.Equal(now.AddDate(0, 0, tc.days)) {
				t.Errorf("expiry is %s, want %d days on", at, tc.days)
			}
		})
	}
}

func TestCodeOfTreatsAFaultAsInternal(t *testing.T) {
	code, isRefusal := identity.CodeOf(errFake{})
	if isRefusal {
		t.Error("a plain error was reported as an access decision")
	}
	if code != identity.CodeInternal {
		t.Errorf("got %q, want %q", code, identity.CodeInternal)
	}
}

type errFake struct{}

func (errFake) Error() string { return "something broke" }
