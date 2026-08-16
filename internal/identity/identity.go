// Package identity holds the rules a person's account turns on: what a password
// has to be, what an address has to look like, how long a link or a session
// lives, and the closed set of codes a caller is ever told.
//
// It is pure. It resolves nobody, opens nothing, and never imports the store,
// net/http, or client-go. The surfaces that do reach those things take the
// narrow interfaces this package declares.
package identity

import (
	"errors"
	"fmt"
	"time"
)

// Code is one refusal a caller can be told. The set is closed: a failure that
// reaches a caller is one of these, never a wrapped error string.
//
// This is deliberately separate from domain.Reason, which describes how a deploy
// ended. The two sets answer different questions and share no values.
type Code string

// Every code the identity surface can answer with.
const (
	// CodeEmailInvalid means the address failed net/mail parsing or was too long.
	CodeEmailInvalid Code = "email_invalid"
	// CodePasswordTooShort means the password was under the minimum length. It is
	// the only composition rule there is.
	CodePasswordTooShort Code = "password_too_short"
	// CodeNoteTooLong means an invite's note was over NoteLimit characters. It is
	// its own code rather than CodeEmailInvalid because the code is what a caller
	// branches on, and no address was involved.
	CodeNoteTooLong Code = "note_too_long"
	// CodeCredentialsInvalid covers a wrong password, an unknown address, and a
	// disabled account alike. They are deliberately indistinguishable.
	CodeCredentialsInvalid Code = "credentials_invalid"
	// CodeEmailUnverified means the account exists but has never confirmed its
	// address. It is the one place login reveals that an address is registered,
	// accepted knowingly so a person who never got the mail is sent to resend.
	CodeEmailUnverified Code = "email_unverified"
	// CodeLinkInvalid covers unknown, spent, expired, and wrong purpose links, in
	// the same words for all four.
	CodeLinkInvalid Code = "link_invalid"
	// CodeInviteInvalid covers a missing, unknown, spent, revoked and expired
	// registration invite, in the same words for all five.
	//
	// It is a 403 rather than the 400 CodeLinkInvalid carries, because this is an
	// authorisation decision about whether the caller may register at all rather
	// than a statement that their input was malformed. The closest existing
	// pairing is CodeAdminRequired, which is also a 403.
	CodeInviteInvalid Code = "invite_invalid"
	// CodeAddressRegistered means a mint named an address that already has an
	// account, so there is nobody left to invite (spec 0025, AC-3). It is a 409:
	// the request is well formed and describes a state the platform is already in.
	//
	// It is safe to say plainly because the only surface that can produce it is
	// admin only. It must never reach the register path, where the same sentence
	// would tell a stranger which addresses are registered.
	CodeAddressRegistered Code = "address_registered"
	// CodeAdminRequired means a live session that is not an admin's.
	CodeAdminRequired Code = "admin_required"
	// CodeTokenNameTaken means the account already holds a live token by that name.
	CodeTokenNameTaken Code = "token_name_taken"
	// CodeInvalidExpiry means a requested token lifetime was outside 1 to 365 days.
	CodeInvalidExpiry Code = "invalid_expiry"
	// CodeNotFound is the answer for a row that does not exist, and equally for one
	// that exists but belongs to somebody else.
	CodeNotFound Code = "not_found"
	// CodeRateLimited means the caller has spent its bucket, or the address has
	// failed sign in too many times.
	CodeRateLimited Code = "rate_limited"
	// CodeMailUnavailable means no mail sender is configured, so an endpoint that
	// exists only to send mail cannot do its job.
	CodeMailUnavailable Code = "mail_unavailable"
	// CodeInternal is the only code that stands for a fault rather than a decision.
	CodeInternal Code = "internal"
)

// Error is a refusal a caller sees: a code from the closed set and a sentence
// safe to hand back. It never carries the underlying error, because that is the
// thing that must not cross the boundary.
type Error struct {
	Code    Code
	Message string
}

// Error implements error.
func (e *Error) Error() string { return fmt.Sprintf("identity: %s: %s", e.Code, e.Message) }

// Fail builds a refusal.
func Fail(c Code, message string) *Error { return &Error{Code: c, Message: message} }

// CodeOf reports the code a caller should be told for err, and whether err was a
// refusal at all. Anything that is not an *Error is a fault, and a fault is
// CodeInternal: an internal error is never dressed up as an access decision.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return CodeInternal, false
}

// The lifetimes the platform runs on. They are constants rather than
// DEPLOYER_* configuration because they are product decisions about how long a
// person stays signed in and how long a mailed link stays good, not something an
// operator tunes per cluster.
const (
	// SessionLifetime is how long a session lives past its last use. Every
	// authenticated request pushes it forward.
	SessionLifetime = 30 * 24 * time.Hour
	// LinkLifetime is how long a verification or reset link stays spendable.
	LinkLifetime = 24 * time.Hour
	// InviteLifetime is how long a registration invite stays spendable. Longer
	// than a link because it is handed over by a person rather than mailed by the
	// platform, and short enough that a forwarded one stops working on its own.
	InviteLifetime = 7 * 24 * time.Hour
	// MinTokenDays and MaxTokenDays bound a requested API token lifetime.
	MinTokenDays = 1
	MaxTokenDays = 365
)

// TokenExpiry turns a requested lifetime in days into an absolute expiry, or
// refuses it. Zero days means no expiry, which is the documented default.
func TokenExpiry(now time.Time, days int) (time.Time, bool, error) {
	if days == 0 {
		return time.Time{}, false, nil
	}
	if days < MinTokenDays || days > MaxTokenDays {
		return time.Time{}, false, Fail(CodeInvalidExpiry,
			fmt.Sprintf("a token lifetime must be between %d and %d days", MinTokenDays, MaxTokenDays))
	}
	return now.Add(time.Duration(days) * 24 * time.Hour), true, nil
}
