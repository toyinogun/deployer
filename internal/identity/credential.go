package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The password policy, entire. Length is the only rule: composition rules push
// people towards predictable substitutions and buy nothing a length floor does
// not already buy.
const (
	// MinPasswordLen is the floor, in bytes of UTF-8, matching current OWASP guidance.
	MinPasswordLen = 12
	// MaxPasswordLen stops a very long input from turning a hash into a denial of
	// service. Argon2 cost does not grow with input length, but reading an
	// unbounded one into memory does.
	MaxPasswordLen = 1024
	// MaxEmailLen is the longest address RFC 5321 allows.
	MaxEmailLen = 254
)

// The argon2id parameters. m = 19456 KiB, t = 2, p = 1 is the current OWASP
// minimum, which lands at roughly 50ms per hash on this hardware.
const (
	argonTime    = 2
	argonMemory  = 19456
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// tokenBytes is how much entropy a session id or a link token carries. 32 bytes
// is far past guessable, so there is no work factor on the hash that stores it.
const tokenBytes = 32

// APITokenPrefix marks a minted API token so it is recognisable in a paste.
const APITokenPrefix = "dpl_"

// NormalizeEmail lower cases and trims an address so the unique index sees one
// spelling. Only the domain is case insensitive in the standard, but treating the
// local part as case sensitive would let two people register what every mail
// client shows as the same address.
func NormalizeEmail(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// CheckEmail refuses an address that net/mail rejects or that is too long. It
// returns the normalized form to store.
func CheckEmail(raw string) (string, error) {
	normalized := NormalizeEmail(raw)
	if normalized == "" || len(normalized) > MaxEmailLen {
		return "", Fail(CodeEmailInvalid, "that is not a usable email address")
	}
	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Address != normalized {
		// A parse that succeeds but rewrites the input means the caller sent a
		// display name or angle brackets. Store the address, not the decoration.
		return "", Fail(CodeEmailInvalid, "that is not a usable email address")
	}
	return normalized, nil
}

// CheckPassword refuses anything under the length floor.
func CheckPassword(raw string) error {
	if len(raw) < MinPasswordLen || len(raw) > MaxPasswordLen {
		return Fail(CodePasswordTooShort,
			fmt.Sprintf("a password must be at least %d characters", MinPasswordLen))
	}
	return nil
}

// DisplayNameFor is the label a person is shown as: what they gave, or the local
// part of their address when they gave nothing. Never unique.
func DisplayNameFor(given, email string) string {
	if trimmed := strings.TrimSpace(given); trimmed != "" {
		return trimmed
	}
	local, _, _ := strings.Cut(email, "@")
	return local
}

// Hasher turns passwords into stored hashes and back into a yes or no. Concurrent
// hashes are bounded, because argon2id at these parameters holds 19MiB each and
// a burst of sign ins would otherwise walk the pod past its memory limit.
type Hasher struct{ slots chan struct{} }

// NewHasher returns a hasher admitting at most concurrency hashes at once. Zero
// or less means the default of 4.
func NewHasher(concurrency int) *Hasher {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Hasher{slots: make(chan struct{}, concurrency)}
}

// Hash encodes a password as an argon2id string carrying its own salt and
// parameters, so a later parameter change does not invalidate existing hashes.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: drawing a salt: %w", err)
	}
	key := h.derive(password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches encoded. A malformed stored hash is
// false rather than an error: it is not a fault the caller can act on, and
// telling the two apart would be a way to probe the store.
//
// An empty encoded hash, which is exactly the bootstrap account, still spends a
// full hash before answering false, so signing in as it is not measurably faster
// than signing in as a real account with the wrong password (AC-11).
func (h *Hasher) Verify(password, encoded string) bool {
	if encoded == "" {
		h.burn(password)
		return false
	}
	salt, want, t, m, p, err := parseArgon(encoded)
	if err != nil {
		h.burn(password)
		return false
	}
	got := h.derive(password, salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// burn spends the same work a real verification would, so a missing or unusable
// stored hash is not a timing oracle.
func (h *Hasher) burn(password string) {
	h.derive(password, make([]byte, argonSaltLen), argonTime, argonMemory, argonThreads, argonKeyLen)
}

// derive runs argon2id under the concurrency bound.
func (h *Hasher) derive(password string, salt []byte, t, m uint32, p uint8, keyLen uint32) []byte {
	h.slots <- struct{}{}
	defer func() { <-h.slots }()
	return argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
}

// parseArgon reads back the salt, the digest, and the parameters a hash was made
// with, so a hash written under older parameters still verifies.
func parseArgon(encoded string) (salt, key []byte, t, m uint32, p uint8, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, fmt.Errorf("identity: not an argon2id hash")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("identity: unreadable argon2id parameters: %w", err)
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("identity: unreadable argon2id salt: %w", err)
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("identity: unreadable argon2id digest: %w", err)
	}
	return salt, key, t, m, p, nil
}

// NewSecret draws a fresh random credential: a session id, a link token, or the
// body of an API token. base64url so it survives a URL and a header untouched.
func NewSecret() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: drawing a credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewAPIToken draws a fresh API token, marked so it is recognisable in a paste.
func NewAPIToken() (string, error) {
	body, err := NewSecret()
	if err != nil {
		return "", err
	}
	return APITokenPrefix + body, nil
}

// HashSecret is how a session id or a link token becomes a stored value. SHA-256
// hex, the same shape the api_tokens column already holds, and a plain hash for
// the same reason: these are 256 bit random values the platform minted, not
// passwords somebody chose, so there is no guessable space to defend.
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
