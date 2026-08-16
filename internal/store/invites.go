package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// The row types spec 0015 adds, named here so callers never import the
// generated package.
type (
	// Invite is one single use registration code, stored as a hash exactly as a
	// link token is.
	Invite = sqlcgen.Invite
	// InviteListRow is one invite as the admin list reads it: no hash, and the
	// two account references already resolved to the names shown.
	InviteListRow = sqlcgen.ListInvitesRow
)

// NewInvite is an invite about to be minted. The raw code never reaches the
// store: the caller draws it, hashes it, and shows the raw value exactly once.
type NewInvite struct {
	CodeHash string
	// Note is the admin's own words about who this went to. Empty means none,
	// which is what the platform's own bootstrap invite carries.
	Note string
	// Email is the normalized address this invite is bound to. Empty means
	// unbound, which is today's behaviour and what the bootstrap invite carries
	// permanently (spec 0025).
	Email string
	// CreatedBy is the admin who minted it. Empty means the platform minted it
	// at boot, stored as null.
	CreatedBy string
	ExpiresAt string
}

// CreateInvite mints one invite.
//
// A bound mint is a transaction, because the guard that decides whether the
// write is allowed runs inside the transaction that performs it: the accounts
// table is read for that address at the top of the same BEGIN IMMEDIATE that
// inserts the row, so a registration landing between the two cannot leave an
// invite bound to an address that now has an account. An unbound mint skips the
// read entirely and is exactly the write it always was (spec 0025, AC-3).
func (s *Store) CreateInvite(ctx context.Context, n NewInvite) (Invite, error) {
	params := sqlcgen.CreateInviteParams{
		ID:        ids.New(ids.Invite, s.clock.Now()),
		CodeHash:  n.CodeHash,
		Note:      optional(n.Note),
		Email:     optional(n.Email),
		CreatedBy: optional(n.CreatedBy),
		ExpiresAt: n.ExpiresAt,
		Now:       s.now(),
	}
	var inv Invite
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if n.Email != "" {
			taken, err := q.AccountExistsByEmail(ctx, &n.Email)
			if err != nil {
				// The address is not in the message. It is personal data and this
				// string reaches a log.
				return fmt.Errorf("store: checking whether an invited address is registered: %w", err)
			}
			if taken {
				return ErrAddressRegistered
			}
		}
		created, err := q.CreateInvite(ctx, params)
		if err != nil {
			// The hash is not in the message: an error string is one more place a
			// value derived from the raw code could end up in a log.
			return fmt.Errorf("store: minting an invite: %w", err)
		}
		inv = created
		return nil
	})
	if err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// ListInvites reads every invite, newest first, spent and revoked ones included.
// No pagination: this grows with registrations rather than with headcount, which
// at the platform's scale is tens of rows for years (spec 0015, Consequences).
func (s *Store) ListInvites(ctx context.Context) ([]InviteListRow, error) {
	rows, err := s.q.ListInvites(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing invites: %w", err)
	}
	return rows, nil
}

// LiveInvite reads the invite a code names, and only while it is live and the
// candidate address satisfies it. Unknown, spent, revoked, expired, and bound to
// somebody else are the same ErrInviteInvalid, so the lookup tells a caller
// nothing about which kind of bad code they hold (spec 0025, AC-8).
//
// candidateEmail is the normalized address the registration is for. An unbound
// invite carries null and matches whatever is passed, which is what keeps every
// invite minted before spec 0025 working.
func (s *Store) LiveInvite(ctx context.Context, codeHash, candidateEmail string) (Invite, error) {
	inv, err := s.q.LiveInviteByCodeHash(ctx, sqlcgen.LiveInviteByCodeHashParams{
		CodeHash: codeHash, Candidate: &candidateEmail, Now: s.now(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrInviteInvalid
	}
	if err != nil {
		return Invite{}, fmt.Errorf("store: reading an invite: %w", err)
	}
	return inv, nil
}

// RevokeInvite pulls an invite back. The guard is the full live predicate, so
// revoking one that is already spent or expired touches nothing and comes back
// ErrNotFound, and revoked_at is only ever set on a row that was live when it
// was revoked (AC-7).
func (s *Store) RevokeInvite(ctx context.Context, id string) error {
	n, err := s.q.RevokeInvite(ctx, sqlcgen.RevokeInviteParams{Now: ptr(s.now()), ID: id})
	if err != nil {
		return fmt.Errorf("store: revoking invite %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SpendInviteAndCreateAccount stamps an invite spent and registers the account
// it created, in one transaction.
//
// The account id is the caller's, generated before the transaction opens, so the
// guarded update and the insert can name each other. The update carries the full
// live predicate rather than trusting the lookup that preceded it, so two
// registrations racing on one code end with exactly one account (AC-4).
//
// Two failures are told apart because the layer above answers them differently:
// ErrInviteInvalid when the guard touched no row, ErrEmailTaken when the insert
// lost. Both roll the whole transaction back, so a taken address leaves the
// invite live and usable (AC-10).
func (s *Store) SpendInviteAndCreateAccount(ctx context.Context, inviteID, accountID string,
	n NewIdentityAccount,
) (Account, error) {
	now := s.now()
	var acc Account
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		spent, err := q.SpendInvite(ctx, sqlcgen.SpendInviteParams{
			Now: ptr(now), ConsumedBy: ptr(accountID), ID: inviteID,
		})
		if err != nil {
			return fmt.Errorf("store: spending invite %s: %w", inviteID, err)
		}
		if spent == 0 {
			return ErrInviteInvalid
		}
		acc, err = q.CreateIdentityAccount(ctx, sqlcgen.CreateIdentityAccountParams{
			ID:           accountID,
			Email:        ptr(n.Email),
			PasswordHash: ptr(n.PasswordHash),
			DisplayName:  ptr(n.DisplayName),
			Now:          now,
		})
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("store: creating an account: %w", err)
		}
		// The spend's own audit row, written here rather than by the handler that
		// called in. No edge may learn that an account was created: answering a
		// taken address identically to a fresh one is the whole point of AC-10, so
		// the row naming the invite and the account it made can only be written
		// where both are known. Inside the transaction it describes, so a spend
		// that rolls back leaves no row claiming it happened (AC-15).
		if err := q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
			ID:         ids.New(ids.AuditLog, s.clock.Now()),
			AccountID:  ptr(accountID),
			Action:     auth.ActionRegister,
			TargetType: ptr("invite"),
			TargetID:   ptr(inviteID),
			Outcome:    "allowed",
			OccurredAt: now,
		}); err != nil {
			return fmt.Errorf("store: recording the spend of invite %s: %w", inviteID, err)
		}
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return acc, nil
}

// AnyAccountHasEmail reports whether a person has ever registered. Half of what
// the startup bootstrap decides from: a database with a human account in it does
// not need a way in (AC-13).
func (s *Store) AnyAccountHasEmail(ctx context.Context) (bool, error) {
	yes, err := s.q.AnyAccountHasEmail(ctx)
	if err != nil {
		return false, fmt.Errorf("store: checking for a registered account: %w", err)
	}
	return yes, nil
}

// AnyLiveBootstrapInvite reports whether the platform already minted itself a
// way in that nobody has spent. The other half of the bootstrap decision, and
// what stops a restarting pod leaving several live bootstrap invites behind.
func (s *Store) AnyLiveBootstrapInvite(ctx context.Context) (bool, error) {
	yes, err := s.q.AnyLiveBootstrapInvite(ctx, s.now())
	if err != nil {
		return false, fmt.Errorf("store: checking for a live bootstrap invite: %w", err)
	}
	return yes, nil
}

// optional turns an empty string into the null the column holds.
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
