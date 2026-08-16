package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// The refusals this flow can hand back. They are errors rather than Code values
// because the OAuth routes answer RFC 6749 error codes rather than the closed
// identity code set, which spec 0024 records as a written exception (AC-24).
var (
	// ErrClientMetadataInvalid is a registration whose body does not parse, or
	// carries no redirect URI, or names itself at unreasonable length (AC-4a).
	ErrClientMetadataInvalid = errors.New("identity: client metadata invalid")

	// ErrRedirectURIInvalid is a registration carrying a redirect URI this
	// platform will not store. It is separate from the metadata error because
	// the two answer different OAuth codes and must never overlap (AC-4a).
	ErrRedirectURIInvalid = errors.New("identity: redirect uri invalid")

	// ErrGrantInvalid is every way an exchange can fail: an unknown code, a
	// spent one, an expired one, the wrong client, the wrong redirect URI, the
	// wrong resource, or a verifier that does not produce the stored challenge.
	// They are one error on purpose, so the response never says which check
	// refused it (AC-18).
	ErrGrantInvalid = errors.New("identity: grant invalid")
)

// OAuthClient is one registered connector client as this package sees it.
// Nothing here is a fact about who the client is: Name is whatever it called
// itself and Approved records only that some account once approved it.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	Approved     bool
}

// NewOAuthCode is one authorization code about to be written. The raw code is
// never stored; CodeHash is.
type NewOAuthCode struct {
	CodeHash      string
	ClientID      string
	AccountID     string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
}

// OAuthCode is one authorization code read back at the token endpoint.
type OAuthCode struct {
	ClientID      string
	AccountID     string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	TokenID       string
	ExpiresAt     time.Time
	Consumed      bool
}

// ClientGrant is one exchange as the store performs it: the code being spent
// and the token that replaces whatever this client held before.
type ClientGrant struct {
	CodeHash    string
	AccountID   string
	ClientID    string
	Name        string
	TokenHash   string
	TokenPrefix string
}

// grantNameAttempts bounds the ordinal fallback below. It is this package's own
// rather than the browser surface's, because a granted token is minted here.
const grantNameAttempts = 50

// GrantRequest is a token endpoint call, already parsed.
type GrantRequest struct {
	ClientID    string
	Code        string
	RedirectURI string
	Verifier    string
	// Resource is optional. An absent one takes the value stored on the code,
	// so a client that omits it is not refused for it (AC-18).
	Resource string
}

// RegisterClient stores a client anyone on the internet may create. It grants
// nothing: the row is inert until an account approves it on the console, and an
// unapproved row is swept after ClientRetention (AC-4, AC-8).
//
// The two refusals never overlap. A bad redirect URI is ErrRedirectURIInvalid
// and everything else wrong with the metadata is ErrClientMetadataInvalid, so
// the endpoint above can answer one OAuth code per case (AC-4a).
func (s *Service) RegisterClient(ctx context.Context, name string, redirectURIs []string) (OAuthClient, error) {
	if len([]rune(name)) > MaxClientNameLen {
		return OAuthClient{}, ErrClientMetadataInvalid
	}
	if len(redirectURIs) == 0 {
		return OAuthClient{}, ErrClientMetadataInvalid
	}
	if len(redirectURIs) > MaxRedirectURIs {
		return OAuthClient{}, ErrRedirectURIInvalid
	}
	for _, uri := range redirectURIs {
		if !CheckRedirectURI(uri) {
			return OAuthClient{}, ErrRedirectURIInvalid
		}
	}
	return s.store.RegisterOAuthClient(ctx, CleanClientName(name), redirectURIs)
}

// Client reads one registered client. An unknown id is ErrNotFound, which the
// authorize endpoint answers on its own page rather than by redirecting, because
// there is no registered address to redirect to (AC-10).
func (s *Service) Client(ctx context.Context, id string) (OAuthClient, error) {
	return s.store.OAuthClient(ctx, id)
}

// ApproveClient stamps a client approved and issues the authorization code the
// browser carries back. The raw code is returned once and exists nowhere else:
// what is stored is its hash (AC-16).
//
// The stamp and the code are one write, so a client that reaches the redirect is
// always one the sweep will leave alone and a client the sweep will leave alone
// always has the code it was stamped for. They used to be two calls: an approval
// that failed between them stamped a client the sweep would then never take
// (AC-8) and left it holding nothing, so the row survived forever with no use.
// Minting the secret happens before either write for the same reason, so the one
// failure that is not the database's leaves no row behind at all.
func (s *Service) ApproveClient(ctx context.Context, clientID, accountID, redirectURI, challenge, resource string) (string, error) {
	raw, err := NewSecret()
	if err != nil {
		return "", err
	}
	err = s.store.ApproveOAuthClientAndCreateCode(ctx, NewOAuthCode{
		CodeHash:      HashSecret(raw),
		ClientID:      clientID,
		AccountID:     accountID,
		RedirectURI:   redirectURI,
		CodeChallenge: challenge,
		Resource:      resource,
		ExpiresAt:     s.clock.Now().Add(CodeLifetime),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// SweepClients deletes the clients nobody ever approved that are older than
// ClientRetention, and reports how many went (AC-8).
func (s *Service) SweepClients(ctx context.Context) (int, error) {
	return s.store.SweepOAuthClients(ctx, s.clock.Now().Add(-ClientRetention))
}

// Grant exchanges an authorization code for an ordinary API token.
//
// Every way this can fail is ErrGrantInvalid, and the caller is never told which
// one (AC-18). Two of them are worth reading here rather than in the list.
//
// A code presented a second time is always refused (AC-16a). Whether it also
// revokes the token the first presentation issued is decided by PKCE rather than
// by the second arrival: the replay revokes only when its verifier does not
// produce the stored challenge. What OAuth 2.1's revoke defends against is a code
// somebody other than the client has, and on a public client the verifier is
// exactly the proof of which caller this is. A thief who captured the code from a
// redirect cannot produce it; the client retrying its own request always can, and
// must not lose the token it was just issued for a lost response or a double
// submit (AC-16b).
//
// Everything checked here is checked against the row rather than the request,
// and the checks run before the code is spent. Spending it is the store's one
// conditional statement, so two requests arriving together still mint exactly
// once (AC-18a).
func (s *Service) Grant(ctx context.Context, req GrantRequest) (Minted, error) {
	codeHash := HashSecret(req.Code)
	code, err := s.store.OAuthCode(ctx, codeHash)
	if errors.Is(err, ErrNotFound) {
		return Minted{}, ErrGrantInvalid
	}
	if err != nil {
		return Minted{}, err
	}

	if code.Consumed {
		// The verifier alone decides the revoke. On a public client it is the
		// only proof of which caller this is, so a caller that can produce it is
		// the client retrying its own request, and a caller that cannot is
		// somebody who captured the code from a redirect (AC-16b).
		if !VerifyPKCE(code.CodeChallenge, req.Verifier) {
			s.revokeReplayed(ctx, code)
		}
		return Minted{}, ErrGrantInvalid
	}
	if code.ClientID != req.ClientID ||
		code.RedirectURI != req.RedirectURI ||
		!s.clock.Now().Before(code.ExpiresAt) ||
		(req.Resource != "" && req.Resource != code.Resource) ||
		!VerifyPKCE(code.CodeChallenge, req.Verifier) {
		return Minted{}, ErrGrantInvalid
	}

	account, err := s.store.AccountByID(ctx, code.AccountID)
	if err != nil {
		return Minted{}, err
	}
	client, err := s.store.OAuthClient(ctx, code.ClientID)
	if err != nil {
		return Minted{}, err
	}

	raw, hash, prefix, err := s.prepareMint(account)
	if err != nil {
		return Minted{}, err
	}

	// The same ordinal fallback a hand minted token takes, because the live name
	// index applies to a granted token exactly as it does to any other (AC-20a).
	base := connectorTokenName(client.Name, s.clock.Now())
	for attempt := range grantNameAttempts {
		name := base
		if attempt > 0 {
			name = fmt.Sprintf("%s (%d)", base, attempt+1)
		}
		view, err := s.store.GrantClientToken(ctx, ClientGrant{
			CodeHash:    codeHash,
			AccountID:   account.ID,
			ClientID:    client.ID,
			Name:        name,
			TokenHash:   hash,
			TokenPrefix: prefix,
		})
		switch {
		case errors.Is(err, ErrTokenNameTaken):
			continue
		case errors.Is(err, ErrGrantInvalid):
			return Minted{}, ErrGrantInvalid
		case err != nil:
			return Minted{}, err
		}
		return Minted{Raw: raw, Token: view}, nil
	}
	return Minted{}, fmt.Errorf("identity: no free token name for %q after %d tries", base, grantNameAttempts)
}

// revokeReplayed kills the token a replayed code issued, and is called only for
// a replay that could not prove the PKCE verifier (AC-16b). It is best effort and
// deliberately does not change what the caller is told: the exchange is refused
// either way, and a failure to revoke is a fault worth logging rather than a
// different answer (AC-16a).
func (s *Service) revokeReplayed(ctx context.Context, code OAuthCode) {
	if code.TokenID == "" {
		return
	}
	if err := s.store.RevokeToken(ctx, code.TokenID); err != nil && !errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "revoking a replayed grant failed", "error", err, "token", code.TokenID)
	}
}

// connectorTokenName is what a granted token is called in the list: the name the
// client claimed, bounded and cleaned, plus today's date. A client that offered
// nothing usable gets a plain label rather than a blank name (AC-20a).
func connectorTokenName(clientName string, now time.Time) string {
	label := CleanClientName(clientName)
	if label == "" {
		label = "Connector"
	}
	return label + " " + now.Format(time.DateOnly)
}
