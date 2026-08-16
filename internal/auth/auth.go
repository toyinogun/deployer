// Package auth turns a presented bearer token into an account, and seeds the
// one bootstrap account this slice authenticates with. It never imports the
// store: it declares the narrow interface it needs and the store satisfies it
// (spec 0002, AC-18).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// BootstrapAccountName is the account the seeding creates, and the only account
// that exists in this slice.
const BootstrapAccountName = "bootstrap"

// bootstrapTokenName names the seeded token inside that account. A live name is
// unique per account, which is what makes the seeding idempotent.
const bootstrapTokenName = "bootstrap"

// tokenPrefixLen is how much of a raw token is kept in the clear so a row can be
// recognised without being usable.
const tokenPrefixLen = 8

// The failures callers branch on. They mirror the store's sentinels rather than
// being them, so nothing here depends on the store package.
var (
	// ErrNoAccount means no account exists under that name.
	ErrNoAccount = errors.New("auth: no such account")

	// ErrTokenInvalid covers unknown, revoked, and expired tokens, and tokens
	// belonging to an account that never confirmed its address. They are
	// deliberately indistinguishable.
	ErrTokenInvalid = errors.New("auth: token invalid")

	// ErrAccountSuspended is a good token on an account an admin suspended. It is
	// deliberately distinguishable from ErrTokenInvalid: the caller already holds
	// a valid credential for that account, so it learns only about itself, and
	// telling it apart is what lets a surface answer account_suspended instead of
	// a blank credential error (spec 0018, AC-12).
	ErrAccountSuspended = errors.New("auth: account suspended")

	// ErrTooManyAttempts is a run of bad bearer tokens from one address, inside
	// the penalty window that run earned. It is told apart from ErrTokenInvalid
	// because the caller can act on it by waiting, and because a surface has to
	// answer 429 rather than 401 for a client to back off correctly
	// (spec 0022, AC-16).
	ErrTooManyAttempts = errors.New("auth: too many attempts")
)

// Account is the caller identity, carrying only what this package reads.
//
// Email, Verified, Disabled and IsAdmin were added by spec 0007. They live here
// rather than beside the surfaces that read them because the gate they feed is
// applied in exactly one place, for both the bearer route and the session route.
type Account struct {
	ID   string
	Name string
	// Email is empty on an account that was never a person, which is exactly the
	// bootstrap account. That emptiness is what exempts it from the verified gate.
	Email string
	// Verified is whether the address has been confirmed. Meaningless, and
	// ignored, when Email is empty.
	Verified bool
	// Disabled is whether an admin has locked the account out.
	Disabled bool
	// IsAdmin carries visibility over accounts, and nothing over apps.
	IsAdmin bool
}

// Token is one bearer credential, carrying only what this package reads. The
// raw token is never part of it.
type Token struct {
	ID        string
	AccountID string
}

// NewToken describes a token being minted. The raw token never reaches the
// store: the caller hashes it and keeps the plaintext only long enough to use it.
type NewToken struct {
	AccountID string
	Name      string
	TokenHash string
	Prefix    string
}

// Store is the slice of persistence this package needs. internal/store
// satisfies it through the adapter in that package.
type Store interface {
	// AccountByName returns the account with that name, or ErrNoAccount.
	AccountByName(ctx context.Context, name string) (Account, error)
	// CreateAccount registers a caller identity.
	CreateAccount(ctx context.Context, name string) (Account, error)
	// ResolveToken turns a token hash into the account it belongs to, or
	// ErrTokenInvalid. Unknown, revoked and expired are the same error. A
	// suspended account resolves normally, carrying Disabled, so Authenticate
	// can tell it apart (spec 0018, AC-12).
	ResolveToken(ctx context.Context, tokenHash string) (Account, Token, error)
	// RevokeTokensNamed kills every live token an account holds under one name,
	// returning how many it killed.
	RevokeTokensNamed(ctx context.Context, accountID, name string) (int64, error)
	// CreateAPIToken stores a token as its hash plus a non secret prefix.
	CreateAPIToken(ctx context.Context, t NewToken) (Token, error)
}

// HashToken is the one way a raw bearer token ever becomes a stored value.
// SHA-256, hex encoded, matching what the store's token_hash column holds.
//
// Deliberately a plain hash rather than a password hash: these are 256 bit
// random values the platform mints, not passwords a person chose, so there is no
// guessable space for a work factor to defend.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TokenPrefix is the readable head of a token, stored beside the hash so a row
// can be told apart from another without being usable. Short tokens yield a
// short prefix rather than a panic.
func TokenPrefix(raw string) string {
	if len(raw) < tokenPrefixLen {
		return raw
	}
	return raw[:tokenPrefixLen]
}

// Bootstrap makes sure the single account and token this slice authenticates
// with exist, and that the token is the one in raw (spec 0004, AC-1).
//
// It is safe to run on every boot. The same token twice changes nothing. A
// changed token revokes the old row before minting the new one, so rotating the
// sealed secret leaves exactly one working credential rather than two.
//
// raw is never logged here and never returned in an error, at any level.
func Bootstrap(ctx context.Context, s Store, raw string) error {
	if raw == "" {
		// No token configured. The caller decides how loudly to say so; this is
		// a supported local run, not a failure.
		return nil
	}
	hash := HashToken(raw)

	account, err := s.AccountByName(ctx, BootstrapAccountName)
	if errors.Is(err, ErrNoAccount) {
		account, err = s.CreateAccount(ctx, BootstrapAccountName)
	}
	if err != nil {
		return fmt.Errorf("auth: ensuring the %s account: %w", BootstrapAccountName, err)
	}

	// Already seeded with this exact token, so there is nothing to do. Asked by
	// hash, so the raw value is never compared or carried further.
	switch _, _, err := s.ResolveToken(ctx, hash); {
	case err == nil:
		return nil
	case errors.Is(err, ErrTokenInvalid):
		// Not seeded yet, or seeded with a different token. Fall through.
	default:
		return fmt.Errorf("auth: checking the bootstrap token: %w", err)
	}

	if _, err := s.RevokeTokensNamed(ctx, account.ID, bootstrapTokenName); err != nil {
		return fmt.Errorf("auth: retiring the previous bootstrap token: %w", err)
	}
	if _, err := s.CreateAPIToken(ctx, NewToken{
		AccountID: account.ID,
		Name:      bootstrapTokenName,
		TokenHash: hash,
		Prefix:    TokenPrefix(raw),
	}); err != nil {
		return fmt.Errorf("auth: seeding the bootstrap token: %w", err)
	}
	return nil
}
