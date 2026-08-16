package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// OAuthClient is one registered connector client. RedirectURIs comes back in the
// order it was registered and each entry is the exact string the registration
// supplied, because that string is what a requested redirect URI is compared
// against (spec 0024, AC-10b).
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    string
	ApprovedAt   string
}

// NewOAuthCode is one authorization code about to be written. The raw code
// itself never reaches this layer: what is stored is its hash.
type NewOAuthCode struct {
	CodeHash      string
	ClientID      string
	AccountID     string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
}

// OAuthCode is one authorization code as the token endpoint reads it back.
type OAuthCode struct {
	CodeHash      string
	ClientID      string
	AccountID     string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	TokenID       string
	ExpiresAt     string
	ConsumedAt    string
}

// ClientGrant is one exchange: the code being spent and the token that replaces
// whatever this client held before.
type ClientGrant struct {
	CodeHash    string
	AccountID   string
	ClientID    string
	Name        string
	TokenHash   string
	TokenPrefix string
}

// CreateOAuthClient registers a client. It grants nothing: the row is inert
// until an account approves it, and an unapproved row is swept.
func (s *Store) CreateOAuthClient(ctx context.Context, name string, redirectURIs []string) (OAuthClient, error) {
	encoded, err := json.Marshal(redirectURIs)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("store: encoding redirect uris: %w", err)
	}
	row, err := s.q.CreateOAuthClient(ctx, sqlcgen.CreateOAuthClientParams{
		ID:           ids.New(ids.OAuthClient, s.clock.Now()),
		Name:         name,
		RedirectUris: string(encoded),
		Now:          s.now(),
	})
	if err != nil {
		return OAuthClient{}, fmt.Errorf("store: registering oauth client: %w", err)
	}
	return toOAuthClient(row)
}

// OAuthClient reads one registered client.
func (s *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	row, err := s.q.GetOAuthClient(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("store: reading oauth client: %w", err)
	}
	return toOAuthClient(row)
}

// ApproveOAuthClient stamps the client approved. The statement is conditional,
// so a second approval by anyone matches no row and changes nothing: the column
// records that some account once approved this client, never which one, and a
// zero row count is the ordinary case rather than a failure.
func (s *Store) ApproveOAuthClient(ctx context.Context, id string) error {
	_, err := s.q.ApproveOAuthClient(ctx, sqlcgen.ApproveOAuthClientParams{
		Now: ptr(s.now()),
		ID:  id,
	})
	if err != nil {
		return fmt.Errorf("store: approving oauth client %s: %w", id, err)
	}
	return nil
}

// SweepUnapprovedOAuthClients deletes every client nobody ever approved that was
// registered before cutoff, and reports how many went (spec 0024, AC-8). A
// stamped client is never touched here.
func (s *Store) SweepUnapprovedOAuthClients(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.q.DeleteUnapprovedOAuthClients(ctx, ids.Stamp(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: sweeping unapproved oauth clients: %w", err)
	}
	return int(n), nil
}

// ApproveOAuthClientAndCreateCode stamps the client approved and writes the code
// that approval issued, together (spec 0024, AC-8, AC-16).
//
// They are one move rather than two writes that happen to follow each other. The
// stamp is the whole reason the sweep leaves a client alone, so a stamp that
// landed without its code would exempt a row from the sweep forever while
// nothing was ever able to use it. The order inside the transaction is still
// stamp then code, so a client that reaches the redirect is always one the sweep
// will leave alone.
func (s *Store) ApproveOAuthClientAndCreateCode(ctx context.Context, c NewOAuthCode) error {
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		now := s.now()
		if _, err := q.ApproveOAuthClient(ctx, sqlcgen.ApproveOAuthClientParams{
			Now: ptr(now),
			ID:  c.ClientID,
		}); err != nil {
			return fmt.Errorf("store: approving oauth client %s: %w", c.ClientID, err)
		}
		if _, err := q.CreateOAuthCode(ctx, sqlcgen.CreateOAuthCodeParams{
			CodeHash:      c.CodeHash,
			ClientID:      c.ClientID,
			AccountID:     c.AccountID,
			RedirectUri:   c.RedirectURI,
			CodeChallenge: c.CodeChallenge,
			Resource:      c.Resource,
			ExpiresAt:     ids.Stamp(c.ExpiresAt),
			Now:           now,
		}); err != nil {
			return fmt.Errorf("store: writing oauth code: %w", err)
		}
		return nil
	})
}

// CreateOAuthCode writes the code an approval issued. The approval path does not
// call this: it takes ApproveOAuthClientAndCreateCode above, so the stamp and the
// code land together.
func (s *Store) CreateOAuthCode(ctx context.Context, c NewOAuthCode) error {
	_, err := s.q.CreateOAuthCode(ctx, sqlcgen.CreateOAuthCodeParams{
		CodeHash:      c.CodeHash,
		ClientID:      c.ClientID,
		AccountID:     c.AccountID,
		RedirectUri:   c.RedirectURI,
		CodeChallenge: c.CodeChallenge,
		Resource:      c.Resource,
		ExpiresAt:     ids.Stamp(c.ExpiresAt),
		Now:           s.now(),
	})
	if err != nil {
		return fmt.Errorf("store: writing oauth code: %w", err)
	}
	return nil
}

// OAuthCode reads one code back, consumed or not. The consumed case is a real
// answer rather than a miss: presenting a spent code is what costs the token
// that code issued (spec 0024, AC-16a).
func (s *Store) OAuthCode(ctx context.Context, codeHash string) (OAuthCode, error) {
	row, err := s.q.GetOAuthCode(ctx, codeHash)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthCode{}, ErrNotFound
	}
	if err != nil {
		return OAuthCode{}, fmt.Errorf("store: reading oauth code: %w", err)
	}
	return OAuthCode{
		CodeHash:      row.CodeHash,
		ClientID:      row.ClientID,
		AccountID:     row.AccountID,
		RedirectURI:   row.RedirectUri,
		CodeChallenge: row.CodeChallenge,
		Resource:      row.Resource,
		TokenID:       deref(row.TokenID),
		ExpiresAt:     row.ExpiresAt,
		ConsumedAt:    deref(row.ConsumedAt),
	}, nil
}

// GrantClientToken spends a code and mints the token it issued, in one
// transaction (spec 0024, AC-18a, AC-19a).
//
// Three writes have to hold together. The code is spent by one conditional
// UPDATE and only a row count of one goes on to mint, so two token requests
// arriving together cannot both read the code as unconsumed and both mint from
// it. Whatever this client already held for this account is revoked in the same
// transaction as the new token is inserted, so the partial unique index never
// sees two live rows and a mint that fails never leaves the person revoked with
// nothing in hand. And the code records which token it issued, so presenting it
// a second time can revoke that one.
//
// A code that is already spent, or past its 60 seconds, matches no row and comes
// back as ErrTokenInvalid: the caller is told only that the grant did not work,
// never which check refused it.
func (s *Store) GrantClientToken(ctx context.Context, g ClientGrant) (APIToken, error) {
	var minted APIToken
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		now := s.now()

		if _, err := q.ConsumeOAuthCode(ctx, sqlcgen.ConsumeOAuthCodeParams{
			Now:      ptr(now),
			CodeHash: g.CodeHash,
		}); errors.Is(err, sql.ErrNoRows) {
			return ErrTokenInvalid
		} else if err != nil {
			return fmt.Errorf("store: spending oauth code: %w", err)
		}

		if _, err := q.RevokeLiveClientTokens(ctx, sqlcgen.RevokeLiveClientTokensParams{
			Now:           ptr(now),
			AccountID:     g.AccountID,
			OauthClientID: ptr(g.ClientID),
		}); err != nil {
			return fmt.Errorf("store: revoking the previous grant: %w", err)
		}

		tok, err := q.CreateClientAPIToken(ctx, sqlcgen.CreateClientAPITokenParams{
			ID:            ids.New(ids.APIToken, s.clock.Now()),
			AccountID:     g.AccountID,
			Name:          g.Name,
			TokenHash:     g.TokenHash,
			TokenPrefix:   g.TokenPrefix,
			OauthClientID: ptr(g.ClientID),
			Now:           now,
		})
		if err != nil {
			// Wrapped rather than translated: the caller tells a name
			// collision apart with isUniqueViolation, exactly as the hand
			// minted path does, and %w keeps that reachable.
			return fmt.Errorf("store: minting the granted token: %w", err)
		}

		if _, err := q.SetOAuthCodeToken(ctx, sqlcgen.SetOAuthCodeTokenParams{
			TokenID:  ptr(tok.ID),
			CodeHash: g.CodeHash,
		}); err != nil {
			return fmt.Errorf("store: recording which token the code issued: %w", err)
		}

		minted = tok
		return nil
	})
	if err != nil {
		return APIToken{}, err
	}
	return minted, nil
}

// toOAuthClient decodes the stored JSON array back into the exact strings the
// registration supplied.
func toOAuthClient(row sqlcgen.OauthClient) (OAuthClient, error) {
	var uris []string
	if err := json.Unmarshal([]byte(row.RedirectUris), &uris); err != nil {
		return OAuthClient{}, fmt.Errorf("store: decoding redirect uris for %s: %w", row.ID, err)
	}
	return OAuthClient{
		ID:           row.ID,
		Name:         row.Name,
		RedirectURIs: uris,
		CreatedAt:    row.CreatedAt,
		ApprovedAt:   deref(row.ApprovedAt),
	}, nil
}
