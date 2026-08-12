package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// fakeStore is the whole of auth.Store, kept in memory. The real store is
// exercised in internal/store; what matters here is the order of calls
// Bootstrap makes and what it does with the raw token.
type fakeStore struct {
	accounts map[string]auth.Account // name -> account
	tokens   map[string]auth.Token   // live token hash -> token
	names    map[string][]string     // accountID -> live token hashes under the bootstrap name

	created []string // account names created, in order
	minted  []auth.NewToken
	revoked []string // accountID+"/"+name, in order

	accountErr error
	resolveErr error
	revokeErr  error
	mintErr    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]auth.Account{},
		tokens:   map[string]auth.Token{},
		names:    map[string][]string{},
	}
}

func (f *fakeStore) AccountByName(_ context.Context, name string) (auth.Account, error) {
	if f.accountErr != nil {
		return auth.Account{}, f.accountErr
	}
	if a, ok := f.accounts[name]; ok {
		return a, nil
	}
	return auth.Account{}, auth.ErrNoAccount
}

func (f *fakeStore) CreateAccount(_ context.Context, name string) (auth.Account, error) {
	if f.accountErr != nil {
		return auth.Account{}, f.accountErr
	}
	f.created = append(f.created, name)
	a := auth.Account{ID: "acct_1", Name: name}
	f.accounts[name] = a
	return a, nil
}

func (f *fakeStore) ResolveToken(_ context.Context, hash string) (auth.Account, auth.Token, error) {
	if f.resolveErr != nil {
		return auth.Account{}, auth.Token{}, f.resolveErr
	}
	t, ok := f.tokens[hash]
	if !ok {
		return auth.Account{}, auth.Token{}, auth.ErrTokenInvalid
	}
	for _, a := range f.accounts {
		if a.ID == t.AccountID {
			return a, t, nil
		}
	}
	return auth.Account{}, auth.Token{}, auth.ErrTokenInvalid
}

func (f *fakeStore) RevokeTokensNamed(_ context.Context, accountID, name string) (int64, error) {
	if f.revokeErr != nil {
		return 0, f.revokeErr
	}
	f.revoked = append(f.revoked, accountID+"/"+name)
	killed := int64(0)
	for _, hash := range f.names[accountID] {
		delete(f.tokens, hash)
		killed++
	}
	f.names[accountID] = nil
	return killed, nil
}

func (f *fakeStore) CreateAPIToken(_ context.Context, t auth.NewToken) (auth.Token, error) {
	if f.mintErr != nil {
		return auth.Token{}, f.mintErr
	}
	f.minted = append(f.minted, t)
	tok := auth.Token{ID: "tok_1", AccountID: t.AccountID}
	f.tokens[t.TokenHash] = tok
	f.names[t.AccountID] = append(f.names[t.AccountID], t.TokenHash)
	return tok, nil
}

// liveTokenCount is what the verify step counts in the database: how many usable
// bootstrap credentials exist right now.
func (f *fakeStore) liveTokenCount() int { return len(f.tokens) }

func TestHashTokenIsTheSHA256TheStoreColumnHolds(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	raw := "a-bootstrap-token"
	want := sha256.Sum256([]byte(raw))

	if got := auth.HashToken(raw); got != hex.EncodeToString(want[:]) {
		t.Errorf("HashToken = %q, want the hex SHA-256 of the raw value", got)
	}
}

func TestHashTokenNeverReturnsTheRawValue(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	raw := "super-secret-bootstrap-token"

	if got := auth.HashToken(raw); strings.Contains(got, raw) {
		t.Errorf("HashToken leaked the raw token: %q", got)
	}
}

func TestTokenPrefixKeepsOnlyTheReadableHead(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"a full length token keeps eight characters", "abcdefghijklmnop", "abcdefgh"},
		{"a token exactly eight long keeps all of it", "abcdefgh", "abcdefgh"},
		{"a short token yields a short prefix rather than panicking", "abc", "abc"},
		{"an empty token yields an empty prefix", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := auth.TokenPrefix(tt.raw); got != tt.want {
				t.Errorf("TokenPrefix(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBootstrapSeedsOneAccountAndOneToken(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()

	if err := auth.Bootstrap(context.Background(), s, "raw-token"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if len(s.created) != 1 || s.created[0] != auth.BootstrapAccountName {
		t.Errorf("created accounts = %v, want one %q", s.created, auth.BootstrapAccountName)
	}
	if s.liveTokenCount() != 1 {
		t.Errorf("live tokens = %d, want 1", s.liveTokenCount())
	}
	if len(s.minted) != 1 {
		t.Fatalf("minted %d tokens, want 1", len(s.minted))
	}
	if got, want := s.minted[0].TokenHash, auth.HashToken("raw-token"); got != want {
		t.Errorf("stored hash = %q, want the SHA-256 of the raw token", got)
	}
}

func TestBootstrapStoresTheHashAndPrefixAndNeverTheRawToken(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()
	raw := "bootstrap-secret-value"

	if err := auth.Bootstrap(context.Background(), s, raw); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	minted := s.minted[0]
	if minted.TokenHash == raw {
		t.Error("the raw token was stored as the hash")
	}
	if minted.Prefix != auth.TokenPrefix(raw) {
		t.Errorf("prefix = %q, want %q", minted.Prefix, auth.TokenPrefix(raw))
	}
	if len(minted.Prefix) >= len(raw) {
		t.Errorf("prefix %q is not shorter than the raw token, so it is usable", minted.Prefix)
	}
}

func TestBootstrapTwiceWithTheSameTokenChangesNothing(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()
	ctx := context.Background()

	for i := range 3 {
		if err := auth.Bootstrap(ctx, s, "same-token"); err != nil {
			t.Fatalf("Bootstrap run %d: %v", i+1, err)
		}
	}

	if len(s.created) != 1 {
		t.Errorf("created %d accounts across three runs, want 1", len(s.created))
	}
	if len(s.minted) != 1 {
		t.Errorf("minted %d tokens across three runs, want 1", len(s.minted))
	}
	// The initial seed retires whatever was there, which is nothing on a fresh
	// database. The point is that runs two and three do not repeat it.
	if len(s.revoked) != 1 {
		t.Errorf("revoked %v across three runs, want only the initial seed's retire", s.revoked)
	}
	if s.liveTokenCount() != 1 {
		t.Errorf("live tokens = %d, want 1", s.liveTokenCount())
	}
}

func TestBootstrapWithADifferentTokenRevokesTheOldOneFirst(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()
	ctx := context.Background()

	if err := auth.Bootstrap(ctx, s, "first-token"); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if err := auth.Bootstrap(ctx, s, "second-token"); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	if s.liveTokenCount() != 1 {
		t.Fatalf("live tokens = %d after rotation, want exactly 1", s.liveTokenCount())
	}
	if _, ok := s.tokens[auth.HashToken("second-token")]; !ok {
		t.Error("the new token is not the live one")
	}
	if _, ok := s.tokens[auth.HashToken("first-token")]; ok {
		t.Error("the old token still resolves after rotation")
	}
	// One retire for the initial seed, one for the rotation itself.
	if len(s.revoked) != 2 {
		t.Errorf("revoked %v, want the rotation to retire the previous token exactly once", s.revoked)
	}
	if len(s.created) != 1 {
		t.Errorf("created %d accounts across a rotation, want 1", len(s.created))
	}
}

func TestBootstrapReusesAnExistingAccountRatherThanCreatingASecond(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()
	s.accounts[auth.BootstrapAccountName] = auth.Account{ID: "acct_existing", Name: auth.BootstrapAccountName}

	if err := auth.Bootstrap(context.Background(), s, "raw-token"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if len(s.created) != 0 {
		t.Errorf("created %v, want no account created when one already exists", s.created)
	}
	if got := s.minted[0].AccountID; got != "acct_existing" {
		t.Errorf("token minted against %q, want the existing account", got)
	}
}

func TestBootstrapWithNoTokenIsASupportedNoOp(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	s := newFakeStore()

	if err := auth.Bootstrap(context.Background(), s, ""); err != nil {
		t.Fatalf("Bootstrap with no token: %v, want nil", err)
	}

	if len(s.created)+len(s.minted)+len(s.revoked) != 0 {
		t.Error("an empty bootstrap token touched the store")
	}
}

func TestBootstrapErrorsNeverCarryTheRawToken(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	raw := "leak-me-if-you-can"
	tests := []struct {
		name  string
		setup func(*fakeStore)
	}{
		{"the account lookup fails", func(s *fakeStore) { s.accountErr = errors.New("db down") }},
		{"the token check fails", func(s *fakeStore) { s.resolveErr = errors.New("db down") }},
		{"the revoke fails", func(s *fakeStore) { s.revokeErr = errors.New("db down") }},
		{"the mint fails", func(s *fakeStore) { s.mintErr = errors.New("db down") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newFakeStore()
			tt.setup(s)

			err := auth.Bootstrap(context.Background(), s, raw)

			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), raw) {
				t.Errorf("the error carries the raw token: %v", err)
			}
			if !strings.Contains(err.Error(), "db down") {
				t.Errorf("the underlying cause was not wrapped: %v", err)
			}
		})
	}
}
