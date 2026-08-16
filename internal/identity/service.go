package identity

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The two purposes a single use link can carry. They match the CHECK constraint
// on email_tokens, and a link is always matched on its hash and its purpose
// together, never on the hash alone.
const (
	// PurposeVerify is the link that confirms an address.
	PurposeVerify = "verify_email"
	// PurposeReset is the link that sets a new password.
	PurposeReset = "password_reset"
)

// The failures the store layer reports back through the interfaces below. They
// are this package's own, so nothing here depends on internal/store.
var (
	// ErrEmailTaken means the address already has an account. It comes from a
	// losing insert, never from a read before the write.
	ErrEmailTaken = errors.New("identity: email already registered")
	// ErrNotFound means no such row, or a row belonging to somebody else.
	ErrNotFound = errors.New("identity: not found")
	// ErrLinkInvalid covers unknown, spent, expired and wrong purpose links.
	ErrLinkInvalid = errors.New("identity: link invalid")
	// ErrSessionInvalid covers unknown, revoked and expired sessions, and
	// sessions belonging to a disabled account.
	ErrSessionInvalid = errors.New("identity: session invalid")
	// ErrTokenNameTaken means the account already holds a live token by that name.
	ErrTokenNameTaken = errors.New("identity: token name taken")
)

// Account is a person as this package reads one.
type Account struct {
	ID           string
	Email        string // empty on the bootstrap account, which is what exempts it
	DisplayName  string
	PasswordHash string
	Verified     bool
	Disabled     bool
	IsAdmin      bool
	CreatedAt    string
}

// NewAccount is a registration about to be written.
type NewAccount struct {
	Email        string
	PasswordHash string
	DisplayName  string
}

// TokenView is one API token as a caller may see it: never a raw value, never a
// hash.
type TokenView struct {
	ID         string
	AccountID  string
	Name       string
	Prefix     string
	CreatedAt  string
	LastUsedAt string
	ExpiresAt  string
}

// Store is the slice of persistence this package needs. internal/store satisfies
// it through the adapter in that package.
type Store interface {
	AccountByEmail(ctx context.Context, email string) (Account, error)
	AccountByID(ctx context.Context, id string) (Account, error)
	ListAccounts(ctx context.Context) ([]Account, error)
	MarkVerified(ctx context.Context, id string) error
	SetPassword(ctx context.Context, id, passwordHash string) error
	SetDisabled(ctx context.Context, id string, disabled bool) error

	CreateSession(ctx context.Context, accountID, tokenHash string, expiresAt time.Time) (string, error)
	RevokeSession(ctx context.Context, id string) error

	CreateLink(ctx context.Context, accountID, purpose, tokenHash string, expiresAt time.Time) error
	ConsumeLink(ctx context.Context, tokenHash, purpose string) (accountID string, err error)

	CreateInvite(ctx context.Context, n NewInvite) (id string, err error)
	ListInvites(ctx context.Context) ([]InviteRow, error)
	RevokeInvite(ctx context.Context, id string) error
	LiveInvite(ctx context.Context, codeHash string) (id string, err error)
	// SpendInviteAndCreateAccount owns both writes: it stamps the invite spent
	// and inserts the account it created, in one transaction, and rolls both back
	// on either failure. The two errors it can return have to survive this
	// boundary rather than collapsing into one, because the layer above answers
	// them differently: ErrInviteInvalid is a refusal the caller sees, and
	// ErrEmailTaken is the case that must read exactly like a success.
	SpendInviteAndCreateAccount(ctx context.Context, inviteID string, n NewAccount) (Account, error)
	AnyAccountHasEmail(ctx context.Context) (bool, error)
	AnyLiveBootstrapInvite(ctx context.Context) (bool, error)

	MintToken(ctx context.Context, accountID, name, tokenHash, prefix string, expiresAt time.Time) (TokenView, error)
	ListTokens(ctx context.Context, accountID string) ([]TokenView, error)
	TokenByID(ctx context.Context, id string) (TokenView, error)
	RevokeToken(ctx context.Context, id string) error
}

// Mailer sends one transactional message. internal/mail satisfies it. A nil
// Mailer is a supported state: it means no sender is configured.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Clock supplies every timestamp, so a test owns time rather than waiting for it.
type Clock interface{ Now() time.Time }

// Options configures a Service.
type Options struct {
	// ConsoleURL is the base address a mailed link points at. It is the console's
	// public address rather than the internal one because this text is handed to a
	// person, and a person's browser cannot resolve cluster DNS. Since spec 0022
	// there are two derived public addresses and this is named for the one it
	// takes, so wiring the deploy host in here does not compile.
	ConsoleURL string
	// HashConcurrency bounds simultaneous password hashes. Zero means the default.
	HashConcurrency int
	// Hasher overrides the password hasher. Nil means one at the parameters the
	// platform runs on, which is what production always wants; a test suite
	// passes a cheap one so a hundred sign ins do not cost a hundred key
	// derivations it is not testing.
	Hasher *Hasher
}

// Service is the identity use case layer: it orchestrates the store, the hasher
// and the mailer, and holds no business rule of its own. The rules live beside
// the values that own them, in this package's pure functions.
type Service struct {
	store   Store
	mailer  Mailer
	clock   Clock
	hasher  *Hasher
	baseURL string
	limits  *Limiter
}

// NewService returns the identity surface. mailer may be nil, which is what makes
// the endpoints that need it answer mail_unavailable while everything else works.
func NewService(s Store, m Mailer, c Clock, opts Options) *Service {
	hasher := opts.Hasher
	if hasher == nil {
		hasher = NewHasher(opts.HashConcurrency)
	}
	return &Service{
		store:   s,
		mailer:  m,
		clock:   c,
		hasher:  hasher,
		baseURL: opts.ConsoleURL,
		limits:  NewLimiter(c, SignInSettings()),
	}
}

// Limits exposes the rate limiter so the HTTP layer can spend a bucket before it
// does any work.
func (s *Service) Limits() *Limiter { return s.limits }

// Register spends an invite, creates the account it authorised, and mails that
// account a verification link.
//
// The invite lookup is the first statement, ahead of CheckEmail and
// CheckPassword rather than merely ahead of the hash. A caller with no valid
// invite is refused invite_invalid whatever else is wrong with their
// submission, so the gate is never spoken past by a validation message and a
// caller who holds nothing never costs the platform a key derivation (AC-1,
// AC-11).
//
// Registering an address that already has an account is answered identically to
// a fresh registration, and takes comparable work: the password is hashed either
// way, the insert is attempted either way, and one message is sent either way.
// What differs is only which message the real owner of the address receives, and
// that the invite is not spent, because no account was created (AC-2, AC-10).
func (s *Service) Register(ctx context.Context, invite, rawEmail, password, name string) error {
	inviteID, err := s.store.LiveInvite(ctx, HashSecret(invite))
	if errors.Is(err, ErrInviteInvalid) {
		return Fail(CodeInviteInvalid, inviteRefusal)
	}
	if err != nil {
		return err
	}

	email, err := CheckEmail(rawEmail)
	if err != nil {
		return err
	}
	if err := CheckPassword(password); err != nil {
		return err
	}
	if s.mailer == nil {
		return Fail(CodeMailUnavailable, "this platform has no mail sender configured, so it cannot register anyone")
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return err
	}

	account, err := s.store.SpendInviteAndCreateAccount(ctx, inviteID, NewAccount{
		Email:        email,
		PasswordHash: hash,
		DisplayName:  DisplayNameFor(name, email),
	})
	switch {
	case errors.Is(err, ErrInviteInvalid):
		// The invite ended between the lookup and the transaction: somebody else
		// spent it, or an admin revoked it. The guard inside the transaction is
		// what decides that, not the lookup above (AC-4, AC-5).
		return Fail(CodeInviteInvalid, inviteRefusal)
	case errors.Is(err, ErrEmailTaken):
		// The address is spoken for, so the transaction rolled back whole and the
		// invite is still live. The only thing that happens differently is the
		// message its real owner gets, which no caller can observe.
		s.send(ctx, email, alreadyRegisteredSubject, alreadyRegisteredBody(s.baseURL))
		return nil
	case err != nil:
		return err
	}

	link, err := s.issueLink(ctx, account.ID, PurposeVerify)
	if err != nil {
		return err
	}
	s.send(ctx, email, verifySubject, verifyBody(link))
	return nil
}

// Verify spends a verification link and stamps the address confirmed. Every way
// it can fail, including a link minted to reset a password, is link_invalid in
// the same words (AC-5).
func (s *Service) Verify(ctx context.Context, rawToken string) error {
	accountID, err := s.store.ConsumeLink(ctx, HashSecret(rawToken), PurposeVerify)
	if errors.Is(err, ErrLinkInvalid) {
		return Fail(CodeLinkInvalid, "that link is not usable")
	}
	if err != nil {
		return err
	}
	// Already verified is not a failure worth telling anyone about: the link was
	// good and the address is confirmed, which is what the caller asked for.
	if err := s.store.MarkVerified(ctx, accountID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// Resend issues a fresh verification link, superseding whatever the account holds.
// An unknown or already verified address is answered exactly as a successful one
// is, so this endpoint tells a caller nothing either.
func (s *Service) Resend(ctx context.Context, rawEmail string) error {
	if s.mailer == nil {
		return Fail(CodeMailUnavailable, "this platform has no mail sender configured")
	}
	email, err := CheckEmail(rawEmail)
	if err != nil {
		// A malformed address cannot belong to anybody, so this is safe to say.
		return err
	}
	account, err := s.store.AccountByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case account.Verified || account.Disabled:
		return nil
	}

	link, err := s.issueLink(ctx, account.ID, PurposeVerify)
	if err != nil {
		return err
	}
	s.send(ctx, email, verifySubject, verifyBody(link))
	return nil
}

// issueLink mints a single use link and returns the URL to mail. The raw token
// exists only between here and the message; only its hash is stored.
func (s *Service) issueLink(ctx context.Context, accountID, purpose string) (string, error) {
	raw, err := NewSecret()
	if err != nil {
		return "", err
	}
	expires := s.clock.Now().Add(LinkLifetime)
	if err := s.store.CreateLink(ctx, accountID, purpose, HashSecret(raw), expires); err != nil {
		return "", err
	}
	return linkURL(s.baseURL, purpose, raw), nil
}

// send posts a message and swallows the failure into the log. Mail is best
// effort: the account or the link is already committed, and a provider being down
// must not turn a successful registration into an error (AC-25).
func (s *Service) send(ctx context.Context, to, subject, body string) {
	if s.mailer == nil {
		return
	}
	if err := s.mailer.Send(ctx, to, subject, body); err != nil {
		// The address is not in the log line. Neither is the body, which carries
		// the raw link token (AC-27).
		slog.ErrorContext(ctx, "sending a message failed", "error", err, "subject", subject)
	}
}
