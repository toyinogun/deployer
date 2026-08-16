package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/store"
)

// oauthFixture is an account plus a registered client, the minimum a code needs.
func oauthFixture(t *testing.T, s *store.Store) (store.Account, store.OAuthClient) {
	t.Helper()
	acc, err := s.CreateAccount(t.Context(), "connector-owner")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	client, err := s.CreateOAuthClient(t.Context(), "Claude Desktop", []string{"http://localhost/callback"})
	if err != nil {
		t.Fatalf("registering the client: %v", err)
	}
	return acc, client
}

// writeCode puts one live code in front of the exchange.
func writeCode(t *testing.T, s *store.Store, clock interface{ Now() time.Time }, acc store.Account, client store.OAuthClient, hash string) {
	t.Helper()
	err := s.CreateOAuthCode(t.Context(), store.NewOAuthCode{
		CodeHash:      hash,
		ClientID:      client.ID,
		AccountID:     acc.ID,
		RedirectURI:   "http://localhost/callback",
		CodeChallenge: "challenge",
		Resource:      "https://deploy.example.org/mcp",
		ExpiresAt:     clock.Now().Add(60 * time.Second),
	})
	if err != nil {
		t.Fatalf("writing the code: %v", err)
	}
}

func TestARegisteredClientKeepsItsRedirectURIsVerbatim(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	uris := []string{"http://localhost/callback", "https://Example.ORG/cb/"}
	client, err := s.CreateOAuthClient(t.Context(), "A Client", uris)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	read, err := s.OAuthClient(t.Context(), client.ID)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(read.RedirectURIs) != len(uris) {
		t.Fatalf("read back %d uris, want %d", len(read.RedirectURIs), len(uris))
	}
	for i, want := range uris {
		if read.RedirectURIs[i] != want {
			t.Errorf("uri %d came back %q, want the registered string %q", i, read.RedirectURIs[i], want)
		}
	}
	if read.ApprovedAt != "" {
		t.Error("a fresh registration came back approved")
	}
}

// AC-8, AC-16. The stamp and the code are one write. A stamp that landed without
// its code would exempt the client from the sweep forever while leaving it
// nothing to use, so a failure on the code has to take the stamp with it.
func TestAFailedCodeWriteLeavesTheClientUnstamped(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	_, client := oauthFixture(t, s)

	// An account id no row carries: the code's foreign key refuses it, which
	// fails the second write of the pair after the stamp has already run.
	err := s.ApproveOAuthClientAndCreateCode(t.Context(), store.NewOAuthCode{
		CodeHash:      "code-that-never-lands",
		ClientID:      client.ID,
		AccountID:     "no-such-account",
		RedirectURI:   "http://localhost/callback",
		CodeChallenge: "challenge",
		Resource:      "https://deploy.example.org/mcp",
		ExpiresAt:     clock.Now().Add(60 * time.Second),
	})
	if err == nil {
		t.Fatal("writing a code against an unknown account succeeded")
	}

	read, err := s.OAuthClient(t.Context(), client.ID)
	if err != nil {
		t.Fatalf("reading the client back: %v", err)
	}
	if read.ApprovedAt != "" {
		t.Error("the client is stamped approved, so the sweep will never take it, but the approval wrote no code")
	}
	if _, err := s.OAuthCode(t.Context(), "code-that-never-lands"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the code back: got %v, want ErrNotFound", err)
	}
}

func TestApprovingAClientStampsItOnceAndIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	_, client := oauthFixture(t, s)

	if err := s.ApproveOAuthClient(t.Context(), client.ID); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	first, err := s.OAuthClient(t.Context(), client.ID)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if first.ApprovedAt == "" {
		t.Fatal("the client was not stamped approved")
	}

	clock.Advance(time.Hour)
	if err := s.ApproveOAuthClient(t.Context(), client.ID); err != nil {
		t.Fatalf("second approval: %v", err)
	}
	second, err := s.OAuthClient(t.Context(), client.ID)
	if err != nil {
		t.Fatalf("reading back again: %v", err)
	}
	if second.ApprovedAt != first.ApprovedAt {
		t.Errorf("the stamp moved from %q to %q; it records the first approval and nothing else",
			first.ApprovedAt, second.ApprovedAt)
	}
}

// AC-8. The sweep takes what nobody approved and leaves what somebody did.
func TestTheSweepTakesUnapprovedClientsAndLeavesApprovedOnes(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)

	stale, err := s.CreateOAuthClient(t.Context(), "never approved", []string{"http://localhost/cb"})
	if err != nil {
		t.Fatalf("registering the stale client: %v", err)
	}
	approved, err := s.CreateOAuthClient(t.Context(), "approved", []string{"http://localhost/cb"})
	if err != nil {
		t.Fatalf("registering the approved client: %v", err)
	}
	if err := s.ApproveOAuthClient(t.Context(), approved.ID); err != nil {
		t.Fatalf("approving: %v", err)
	}

	clock.Advance(8 * 24 * time.Hour)
	fresh, err := s.CreateOAuthClient(t.Context(), "registered just now", []string{"http://localhost/cb"})
	if err != nil {
		t.Fatalf("registering the fresh client: %v", err)
	}

	n, err := s.SweepUnapprovedOAuthClients(t.Context(), clock.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep took %d clients, want 1", n)
	}
	if _, err := s.OAuthClient(t.Context(), stale.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the unapproved client survived the sweep: %v", err)
	}
	if _, err := s.OAuthClient(t.Context(), approved.ID); err != nil {
		t.Errorf("the approved client was swept: %v", err)
	}
	if _, err := s.OAuthClient(t.Context(), fresh.ID); err != nil {
		t.Errorf("a client registered inside the window was swept: %v", err)
	}
}

func TestAGrantSpendsTheCodeAndRecordsWhichTokenItIssued(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	acc, client := oauthFixture(t, s)
	writeCode(t, s, clock, acc, client, "hash-1")

	tok, err := s.GrantClientToken(t.Context(), store.ClientGrant{
		CodeHash:    "hash-1",
		AccountID:   acc.ID,
		ClientID:    client.ID,
		Name:        "Claude Desktop 2026-08-16",
		TokenHash:   "token-hash-1",
		TokenPrefix: "dpl_abcd",
	})
	if err != nil {
		t.Fatalf("granting: %v", err)
	}

	code, err := s.OAuthCode(t.Context(), "hash-1")
	if err != nil {
		t.Fatalf("reading the code back: %v", err)
	}
	if code.ConsumedAt == "" {
		t.Error("the code was not spent")
	}
	if code.TokenID != tok.ID {
		t.Errorf("the code records token %q, want the one it issued, %q", code.TokenID, tok.ID)
	}
	if tok.OauthClientID == nil || *tok.OauthClientID != client.ID {
		t.Errorf("the token does not carry its client id: %v", tok.OauthClientID)
	}
}

// AC-18a. The race the conditional UPDATE exists for, driven rather than
// assumed: two exchanges of one code arriving together mint exactly once.
func TestTwoExchangesOfOneCodeMintExactlyOneToken(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	acc, client := oauthFixture(t, s)
	writeCode(t, s, clock, acc, client, "hash-race")

	const racers = 2
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted []store.APIToken
		refused int
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := s.GrantClientToken(t.Context(), store.ClientGrant{
				CodeHash:    "hash-race",
				AccountID:   acc.ID,
				ClientID:    client.ID,
				Name:        "racer",
				TokenHash:   "token-hash-race-" + string(rune('a'+i)),
				TokenPrefix: "dpl_race",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, store.ErrTokenInvalid):
				refused++
			case err != nil:
				t.Errorf("racer %d failed with something other than a refusal: %v", i, err)
			default:
				granted = append(granted, tok)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(granted) != 1 {
		t.Errorf("%d racers were granted a token, want exactly 1", len(granted))
	}
	if refused != racers-1 {
		t.Errorf("%d racers were refused, want %d", refused, racers-1)
	}
}

// AC-19b. Two grants for one client, one after the other and then concurrently,
// leave exactly one live token, and the database is what holds it.
func TestASecondGrantForOneClientLeavesExactlyOneLiveToken(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	acc, client := oauthFixture(t, s)

	for i, hash := range []string{"hash-a", "hash-b", "hash-c"} {
		writeCode(t, s, clock, acc, client, hash)
		_, err := s.GrantClientToken(t.Context(), store.ClientGrant{
			CodeHash:    hash,
			AccountID:   acc.ID,
			ClientID:    client.ID,
			Name:        "Connector " + string(rune('a'+i)),
			TokenHash:   "token-" + hash,
			TokenPrefix: "dpl_x",
		})
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}

	var live int
	row := s.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM api_tokens WHERE account_id = ? AND oauth_client_id = ? AND revoked_at IS NULL`,
		acc.ID, client.ID)
	if err := row.Scan(&live); err != nil {
		t.Fatalf("counting live tokens: %v", err)
	}
	if live != 1 {
		t.Errorf("%d live tokens for one client, want 1", live)
	}
}

// The partial unique index is the thing that holds AC-19b, so it is exercised
// with raw SQL that bypasses the Go layer entirely.
func TestTheDatabaseRefusesTwoLiveTokensForOneClient(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	acc, client := oauthFixture(t, s)

	insert := `INSERT INTO api_tokens (id, account_id, name, token_hash, token_prefix, oauth_client_id, created_at, updated_at)
	           VALUES (?, ?, ?, ?, 'dpl_x', ?, '2026-08-16T12:00:00Z', '2026-08-16T12:00:00Z')`
	if _, err := s.DB().ExecContext(t.Context(), insert, "tok_1", acc.ID, "first", "hash-1", client.ID); err != nil {
		t.Fatalf("the first live token was refused: %v", err)
	}
	if _, err := s.DB().ExecContext(t.Context(), insert, "tok_2", acc.ID, "second", "hash-2", client.ID); err == nil {
		t.Fatal("the database accepted a second live token for one client")
	}

	// Revoking the first frees the client for a new grant, exactly as revoking
	// a token frees its name.
	if _, err := s.DB().ExecContext(t.Context(),
		`UPDATE api_tokens SET revoked_at = '2026-08-16T12:01:00Z' WHERE id = 'tok_1'`); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := s.DB().ExecContext(t.Context(), insert, "tok_3", acc.ID, "third", "hash-3", client.ID); err != nil {
		t.Errorf("a new grant after a revoke was refused: %v", err)
	}

	// And the index applies only to granted tokens: two hand minted ones, both
	// with a null client, are none of its business.
	handMinted := `INSERT INTO api_tokens (id, account_id, name, token_hash, token_prefix, created_at, updated_at)
	               VALUES (?, ?, ?, ?, 'dpl_x', '2026-08-16T12:00:00Z', '2026-08-16T12:00:00Z')`
	if _, err := s.DB().ExecContext(t.Context(), handMinted, "tok_4", acc.ID, "hand a", "hash-4"); err != nil {
		t.Fatalf("the first hand minted token was refused: %v", err)
	}
	if _, err := s.DB().ExecContext(t.Context(), handMinted, "tok_5", acc.ID, "hand b", "hash-5"); err != nil {
		t.Errorf("two hand minted tokens collided on the client index: %v", err)
	}
}

func TestAnExpiredCodeIsSpentByNobody(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	acc, client := oauthFixture(t, s)
	writeCode(t, s, clock, acc, client, "hash-expired")

	clock.Advance(61 * time.Second)
	_, err := s.GrantClientToken(t.Context(), store.ClientGrant{
		CodeHash:    "hash-expired",
		AccountID:   acc.ID,
		ClientID:    client.ID,
		Name:        "too late",
		TokenHash:   "token-late",
		TokenPrefix: "dpl_x",
	})
	if !errors.Is(err, store.ErrTokenInvalid) {
		t.Errorf("an expired code granted: %v", err)
	}
}

// Deleting a client takes its codes with it, so a swept registration leaves
// nothing behind that could still be presented.
func TestDeletingAClientCascadesToItsCodes(t *testing.T) {
	t.Parallel()
	s, clock := newStore(t)
	acc, client := oauthFixture(t, s)
	writeCode(t, s, clock, acc, client, "hash-cascade")

	if _, err := s.SweepUnapprovedOAuthClients(t.Context(), clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if _, err := s.OAuthCode(t.Context(), "hash-cascade"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the code outlived the client it belonged to: %v", err)
	}
}
