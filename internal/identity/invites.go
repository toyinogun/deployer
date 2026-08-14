package identity

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// NoteLimit bounds the admin's own words about who an invite went to. It is a
// product decision about what fits a table cell, so it is a constant here rather
// than DEPLOYER_* configuration, the same reasoning that keeps LinkLifetime out
// of the environment.
const NoteLimit = 200

// inviteRefusal is the one sentence every bad invite is answered with. No code
// is missing, unknown, spent, revoked or expired in these words: telling the
// five apart tells a holder which kind they hold (AC-2).
const inviteRefusal = "registration on this platform is by invitation, and that invitation is not usable"

// thePlatform is who an invite the platform minted itself was issued by. It is
// shown in place of a person's name on the bootstrap row.
const thePlatform = "the platform"

// ErrInviteInvalid covers an unknown, spent, revoked and expired invite alike.
// It comes back from the lookup and, separately, from the guarded update inside
// the account transaction, which is where a race is actually decided.
var ErrInviteInvalid = errors.New("identity: invite invalid")

// InviteState is the one state an invite is in when it is read. It is derived
// from the three timestamps against the clock, never stored, so nothing sweeps
// the table and a clock change cannot leave a stale value behind.
type InviteState string

// The four states, terminal and mutually exclusive but for live.
const (
	InviteLive    InviteState = "live"
	InviteSpent   InviteState = "spent"
	InviteRevoked InviteState = "revoked"
	InviteExpired InviteState = "expired"
)

// NewInvite is an invite about to be minted. The raw code never reaches the
// store: the caller draws it, hashes it, and hands the raw value back exactly
// once.
type NewInvite struct {
	CodeHash string
	// Note may be empty, which is what the platform's own bootstrap invite
	// carries.
	Note string
	// CreatedBy is the admin who minted it. Empty means the platform did, at
	// boot.
	CreatedBy string
	ExpiresAt time.Time
}

// InviteRow is one invite as the store reads it back, with its two account
// references already resolved to the names shown. There is no hash in the shape
// at all, so one cannot leak by being forgotten.
type InviteRow struct {
	ID string
	// Note is the admin's own words, empty when none was given.
	Note string
	// IssuerName is the display name of the admin who minted it, empty when the
	// platform did.
	IssuerName string
	// SpenderEmail is the address of the account this invite created, empty
	// until it is spent.
	SpenderEmail string
	ExpiresAt    string
	ConsumedAt   string
	RevokedAt    string
	CreatedAt    string
}

// InviteView is one invite as an admin sees it: the row plus the one state it is
// in, which is the whole reason the view exists rather than the row being handed
// up as it stands.
type InviteView struct {
	ID        string
	Note      string
	IssuedBy  string
	SpentBy   string
	ExpiresAt string
	CreatedAt string
	State     InviteState
}

// IssuedInvite is a freshly minted invite: what it is, and the link that is
// shown exactly once on the page that minted it and then never again (AC-6,
// AC-14).
type IssuedInvite struct {
	ID        string
	Note      string
	ExpiresAt string
	Link      string
}

// InviteStateOf derives which of the four states a row is in.
//
// The order is the order the states are written in: a spent row was spent, a
// revoked row was revoked, and only a row that ended neither way can have run
// out of time. Nothing can be both, because both write guards carry the full
// live predicate, so this ordering settles a case the data does not produce.
func InviteStateOf(r InviteRow, now time.Time) InviteState {
	switch {
	case r.ConsumedAt != "":
		return InviteSpent
	case r.RevokedAt != "":
		return InviteRevoked
	case expired(r.ExpiresAt, now):
		return InviteExpired
	default:
		return InviteLive
	}
}

// expired reports whether a stored RFC 3339 stamp is in the past. A stamp the
// platform wrote and cannot read back is treated as expired: refusing an invite
// the platform cannot reason about is the safe direction on the front door.
func expired(stamp string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return true
	}
	return !t.After(now)
}

// CheckNote validates the admin's optional words about an invite. It is the only
// caller supplied value on this surface, so it is bounded where every other
// caller supplied value on the identity surface is bounded (AC-6).
func CheckNote(raw string) (string, error) {
	note := strings.TrimSpace(raw)
	if utf8.RuneCountInString(note) > NoteLimit {
		return "", Fail(CodeEmailInvalid,
			fmt.Sprintf("a note can be at most %d characters", NoteLimit))
	}
	return note, nil
}

// IssueInvite mints one invite for an admin and returns the link to hand over.
//
// The raw code exists between here and that one response. It is not stored, not
// logged, not audited and not mailed: only its hash reaches the database (AC-14).
func (s *Service) IssueInvite(ctx context.Context, adminID, rawNote string) (IssuedInvite, error) {
	note, err := CheckNote(rawNote)
	if err != nil {
		return IssuedInvite{}, err
	}
	return s.mintInvite(ctx, adminID, note)
}

// ListInvites reads every invite an admin may see, newest first, each carrying
// the one state it is in right now.
func (s *Service) ListInvites(ctx context.Context) ([]InviteView, error) {
	rows, err := s.store.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	out := make([]InviteView, 0, len(rows))
	for _, r := range rows {
		issuer := r.IssuerName
		if issuer == "" {
			issuer = thePlatform
		}
		out = append(out, InviteView{
			ID:        r.ID,
			Note:      r.Note,
			IssuedBy:  issuer,
			SpentBy:   r.SpenderEmail,
			ExpiresAt: r.ExpiresAt,
			CreatedAt: r.CreatedAt,
			State:     InviteStateOf(r, now),
		})
	}
	return out, nil
}

// RevokeInvite pulls a live invite back. One that is already spent or expired is
// not_found and nothing changes, because the store's guard carries the same live
// predicate the spend does (AC-7).
func (s *Service) RevokeInvite(ctx context.Context, id string) error {
	err := s.store.RevokeInvite(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Fail(CodeNotFound, "there is no live invite by that id")
	}
	return err
}

// BootstrapInvite mints the platform its own way in, once, on an empty database.
//
// It reports the link and whether anything was minted. Nothing is minted when a
// person has already registered, and nothing is minted when a live bootstrap
// invite is already outstanding, so a restarting pod cannot leave several behind
// (AC-13). The caller writes the link at info level, which is the second and last
// place a raw code is ever allowed to appear.
func (s *Service) BootstrapInvite(ctx context.Context) (string, bool, error) {
	registered, err := s.store.AnyAccountHasEmail(ctx)
	if err != nil || registered {
		return "", false, err
	}
	outstanding, err := s.store.AnyLiveBootstrapInvite(ctx)
	if err != nil || outstanding {
		return "", false, err
	}
	issued, err := s.mintInvite(ctx, "", "")
	if err != nil {
		return "", false, err
	}
	return issued.Link, true, nil
}

// mintInvite is the one path that draws a code, both for an admin and for the
// boot time bootstrap. They differ only in who is recorded as the issuer.
func (s *Service) mintInvite(ctx context.Context, adminID, note string) (IssuedInvite, error) {
	raw, err := NewSecret()
	if err != nil {
		return IssuedInvite{}, err
	}
	expires := s.clock.Now().Add(InviteLifetime)
	id, err := s.store.CreateInvite(ctx, NewInvite{
		CodeHash:  HashSecret(raw),
		Note:      note,
		CreatedBy: adminID,
		ExpiresAt: expires,
	})
	if err != nil {
		return IssuedInvite{}, err
	}
	return IssuedInvite{
		ID:        id,
		Note:      note,
		ExpiresAt: expires.UTC().Format(time.RFC3339),
		Link:      inviteURL(s.baseURL, raw),
	}, nil
}

// inviteURL builds the address an invited person clicks. PublicURL, never
// InternalURL, for the same reason a mailed link is: this text is read by a
// person, whose browser cannot resolve cluster DNS.
func inviteURL(baseURL, rawCode string) string {
	return baseURL + "/register?invite=" + url.QueryEscape(rawCode)
}
