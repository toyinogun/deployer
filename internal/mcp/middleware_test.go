package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// tokenStore is the whole of auth.Store, holding one live token. Only
// ResolveToken is reached through the middleware; the rest satisfies the
// interface.
type tokenStore struct {
	liveHash string
	account  auth.Account
	err      error
}

func (t tokenStore) AccountByName(context.Context, string) (auth.Account, error) {
	return auth.Account{}, auth.ErrNoAccount
}

func (t tokenStore) CreateAccount(context.Context, string) (auth.Account, error) {
	return auth.Account{}, auth.ErrNoAccount
}

func (t tokenStore) ResolveToken(_ context.Context, hash string) (auth.Account, auth.Token, error) {
	if t.err != nil {
		return auth.Account{}, auth.Token{}, t.err
	}
	if hash != t.liveHash {
		return auth.Account{}, auth.Token{}, auth.ErrTokenInvalid
	}
	return t.account, auth.Token{ID: "tok_1", AccountID: t.account.ID}, nil
}

func (t tokenStore) RevokeTokensNamed(context.Context, string, string) (int64, error) { return 0, nil }

func (t tokenStore) CreateAPIToken(context.Context, auth.NewToken) (auth.Token, error) {
	return auth.Token{}, nil
}

// guarded wraps a handler that records whether it ran and what account reached
// it, behind the real middleware.
func guarded(store auth.Store, auditor auth.Auditor) (http.Handler, *reached) {
	seen := &reached{}
	s := &Server{auth: auth.NewAuthenticator(store, nil), auditor: auditor}
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.ran = true
		seen.account = accountFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})), seen
}

type reached struct {
	ran     bool
	account auth.Account
}

// countingAuditor keeps the rows the middleware wrote.
type countingAuditor struct{ rows []auth.Audit }

func (c *countingAuditor) RecordAudit(_ context.Context, a auth.Audit) error {
	c.rows = append(c.rows, a)
	return nil
}

func liveStore() tokenStore {
	return tokenStore{
		liveHash: auth.HashToken("good-token"),
		account:  auth.Account{ID: "acct_1", Name: "bootstrap"},
	}
}

func TestAGoodTokenReachesTheToolWithItsAccount(t *testing.T) {
	// covers: AC-3, AC-19
	t.Parallel()
	auditor := &countingAuditor{}
	handler, seen := guarded(liveStore(), auditor)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !seen.ran {
		t.Fatal("the tool handler never ran")
	}
	if seen.account.ID != "acct_1" {
		t.Errorf("the tool saw account %q, want acct_1", seen.account.ID)
	}
	if len(auditor.rows) != 0 {
		t.Errorf("wrote %d audit rows on an allowed call, want the tool to own that row", len(auditor.rows))
	}
}

func TestABadCredentialNeverReachesATool(t *testing.T) {
	// covers: AC-3, AC-19
	t.Parallel()
	tests := []struct {
		name   string
		header string
	}{
		{"no Authorization header at all", ""},
		{"a wrong token", "Bearer wrong-token"},
		{"the scheme with no token", "Bearer "},
		{"basic auth", "Basic ZGVwbG95ZXI6c2VjcmV0"},
		{"a bare token with no scheme", "good-token"},
		{"the stored hash presented as the token", "Bearer " + auth.HashToken("good-token")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handler, seen := guarded(liveStore(), &countingAuditor{})
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if seen.ran {
				t.Error("the tool handler ran for an unauthenticated caller")
			}
		})
	}
}

func TestADenialSaysNothingAboutWhichToolsExist(t *testing.T) {
	// covers: AC-3, AC-16
	t.Parallel()
	handler, _ := guarded(liveStore(), &countingAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the denial is not JSON: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("body = %v, want a bare unauthorized", body)
	}
	if len(body) != 1 {
		t.Errorf("body = %v, want only the error field", body)
	}
	if strings.Contains(rec.Body.String(), "deploy_app") {
		t.Error("the denial names a tool, so an unauthenticated caller learns the surface")
	}
}

func TestADenialIsAuditedWithNoAccount(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	auditor := &countingAuditor{}
	handler, _ := guarded(liveStore(), auditor)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(auditor.rows) != 1 {
		t.Fatalf("wrote %d audit rows, want exactly one denial", len(auditor.rows))
	}
	row := auditor.rows[0]
	if row.AccountID != "" {
		t.Errorf("account = %q, want empty: nothing resolved", row.AccountID)
	}
	if row.Action != auth.ActionDeploy {
		t.Errorf("action = %q, want %q", row.Action, auth.ActionDeploy)
	}
	if row.Allowed {
		t.Error("the denial was recorded as allowed")
	}
	if row.Reason == "" {
		t.Error("the denial carries no reason")
	}
}

func TestTheDenialBodyNeverEchoesThePresentedToken(t *testing.T) {
	// covers: AC-16, AC-19
	t.Parallel()
	presented := "a-token-worth-stealing"
	auditor := &countingAuditor{}
	handler, _ := guarded(liveStore(), auditor)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+presented)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), presented) {
		t.Errorf("the denial echoes the presented token: %s", rec.Body.String())
	}
	for _, row := range auditor.rows {
		if strings.Contains(row.Reason, presented) || strings.Contains(row.TargetID, presented) {
			t.Errorf("an audit row carries the presented token: %+v", row)
		}
	}
}

func TestAStoreOutageStillRefusesRatherThanLettingTheCallThrough(t *testing.T) {
	// covers: AC-3
	t.Parallel()
	store := liveStore()
	store.err = errStoreDown
	handler, seen := guarded(store, &countingAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if seen.ran {
		t.Error("a tool ran while the token could not be checked")
	}
}

func TestTheMiddlewareSurvivesAFailingAuditor(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	handler, _ := guarded(liveStore(), refusingAuditor{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 even when the audit write fails", rec.Code)
	}
}

func TestAccountFromIsTheZeroAccountOffAGuardedRequest(t *testing.T) {
	// covers: AC-3
	t.Parallel()
	// Nothing outside this package can put an account in a context, so a request
	// that never went through the middleware carries none.
	if got := accountFrom(context.Background()); got != (auth.Account{}) {
		t.Errorf("accountFrom = %+v, want the zero account", got)
	}
}

type refusingAuditor struct{}

func (refusingAuditor) RecordAudit(context.Context, auth.Audit) error { return errStoreDown }

var errStoreDown = errStore("the database is locked")

type errStore string

func (e errStore) Error() string { return string(e) }
