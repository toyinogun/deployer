package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// CreateAccount registers a caller identity.
func (s *Store) CreateAccount(ctx context.Context, name string) (Account, error) {
	now := s.now()
	acc, err := s.q.CreateAccount(ctx, sqlcgen.CreateAccountParams{
		ID:        ids.New(ids.Account, s.clock.Now()),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return Account{}, fmt.Errorf("store: creating account %q: %w", name, err)
	}
	return acc, nil
}

// GetAccount reads one account.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	acc, err := s.q.GetAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: reading account %s: %w", id, err)
	}
	return acc, nil
}

// GetAccountByName reads one account by its unique name.
func (s *Store) GetAccountByName(ctx context.Context, name string) (Account, error) {
	acc, err := s.q.GetAccountByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: reading account %q: %w", name, err)
	}
	return acc, nil
}

// NewToken describes a token being minted. The raw token never reaches the
// store: the caller hashes it and keeps the plaintext only long enough to hand
// it back once.
type NewToken struct {
	AccountID string
	Name      string
	TokenHash string
	Prefix    string
	ExpiresAt string // RFC3339, empty for no expiry
}

// CreateAPIToken stores a token as its hash plus a short non secret prefix.
func (s *Store) CreateAPIToken(ctx context.Context, t NewToken) (APIToken, error) {
	now := s.now()
	params := sqlcgen.CreateAPITokenParams{
		ID:          ids.New(ids.APIToken, s.clock.Now()),
		AccountID:   t.AccountID,
		Name:        t.Name,
		TokenHash:   t.TokenHash,
		TokenPrefix: t.Prefix,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if t.ExpiresAt != "" {
		params.ExpiresAt = ptr(t.ExpiresAt)
	}
	tok, err := s.q.CreateAPIToken(ctx, params)
	if err != nil {
		return APIToken{}, fmt.Errorf("store: creating token %q: %w", t.Name, err)
	}
	return tok, nil
}

// ResolveToken turns a token hash into the account it belongs to. Unknown,
// revoked, expired, and belonging to a disabled account are all the same
// ErrTokenInvalid, so a caller learns nothing from which one it hit.
func (s *Store) ResolveToken(ctx context.Context, tokenHash string) (Account, APIToken, error) {
	row, err := s.q.ResolveToken(ctx, sqlcgen.ResolveTokenParams{
		TokenHash: tokenHash,
		Now:       ptr(s.now()),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, APIToken{}, ErrTokenInvalid
	}
	if err != nil {
		return Account{}, APIToken{}, fmt.Errorf("store: resolving token: %w", err)
	}
	return row.Account, row.ApiToken, nil
}

// TouchToken records that a token was just used.
func (s *Store) TouchToken(ctx context.Context, tokenID string) error {
	err := s.q.TouchTokenLastUsed(ctx, sqlcgen.TouchTokenLastUsedParams{Now: ptr(s.now()), ID: tokenID})
	if err != nil {
		return fmt.Errorf("store: touching token %s: %w", tokenID, err)
	}
	return nil
}

// RevokeAPIToken kills a token. Revoking an already revoked token is not found.
func (s *Store) RevokeAPIToken(ctx context.Context, tokenID string) error {
	n, err := s.q.RevokeAPIToken(ctx, sqlcgen.RevokeAPITokenParams{Now: ptr(s.now()), ID: tokenID})
	if err != nil {
		return fmt.Errorf("store: revoking token %s: %w", tokenID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuditEntry is one authorization outcome. AccountID is empty when the presented
// token resolved to nothing, which is exactly the denial worth recording.
type AuditEntry struct {
	AccountID  string
	Action     string
	TargetType string
	TargetID   string
	Allowed    bool
	Reason     string
}

// RecordAudit writes one audit row. Every authorization outcome writes one,
// allowed or denied. A failure here is returned but callers are expected to log
// it rather than let it replace the outcome they were reporting.
func (s *Store) RecordAudit(ctx context.Context, e AuditEntry) error {
	outcome := "denied"
	if e.Allowed {
		outcome = "allowed"
	}
	params := sqlcgen.InsertAuditLogParams{
		ID:         ids.New(ids.AuditLog, s.clock.Now()),
		Action:     e.Action,
		Outcome:    outcome,
		OccurredAt: s.now(),
	}
	if e.AccountID != "" {
		params.AccountID = ptr(e.AccountID)
	}
	if e.TargetType != "" {
		params.TargetType = ptr(e.TargetType)
	}
	if e.TargetID != "" {
		params.TargetID = ptr(e.TargetID)
	}
	if e.Reason != "" {
		params.Reason = ptr(e.Reason)
	}
	if err := s.q.InsertAuditLog(ctx, params); err != nil {
		return fmt.Errorf("store: recording audit for %q: %w", e.Action, err)
	}
	return nil
}
