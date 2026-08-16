package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// recordingToucher records every token whose use was reported, and can be made
// to fail so a test can prove a failed touch does not refuse a good caller.
type recordingToucher struct {
	touched []string
	err     error
}

func (r *recordingToucher) TouchToken(_ context.Context, tokenID string) error {
	r.touched = append(r.touched, tokenID)
	return r.err
}

// failingAuditor refuses every row, so a test can prove auditing never changes
// what the caller is told.
type failingAuditor struct{ calls int }

func (f *failingAuditor) RecordAudit(context.Context, auth.Audit) error {
	f.calls++
	return errors.New("audit table locked")
}

// collectingAuditor keeps the rows it was given.
type collectingAuditor struct{ rows []auth.Audit }

func (c *collectingAuditor) RecordAudit(_ context.Context, a auth.Audit) error {
	c.rows = append(c.rows, a)
	return nil
}

// seeded returns a store holding one live token for one account.
func seeded(raw string) *fakeStore {
	s := newFakeStore()
	account := auth.Account{ID: "acct_1", Name: auth.BootstrapAccountName}
	s.accounts[account.Name] = account
	s.tokens[auth.HashToken(raw)] = auth.Token{ID: "tok_1", AccountID: account.ID}
	return s
}

func TestAuthenticateResolvesAGoodTokenToItsAccount(t *testing.T) {
	// covers: AC-2, AC-19
	t.Parallel()
	s := seeded("good-token")
	toucher := &recordingToucher{}

	account, err := auth.NewAuthenticator(s, toucher).Authenticate(context.Background(), "good-token", "")

	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if account.ID != "acct_1" {
		t.Errorf("account = %q, want acct_1", account.ID)
	}
	if len(toucher.touched) != 1 || toucher.touched[0] != "tok_1" {
		t.Errorf("touched %v, want the resolved token recorded once", toucher.touched)
	}
}

func TestAuthenticateRefusesEveryBadTokenTheSameWay(t *testing.T) {
	// covers: AC-2, AC-19
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{"an empty token", ""},
		{"an unknown token", "never-minted"},
		{"a token that is only whitespace", "   "},
		{"the stored hash presented as if it were the token", auth.HashToken("good-token")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := auth.NewAuthenticator(seeded("good-token"), &recordingToucher{})

			account, err := a.Authenticate(context.Background(), tt.raw, "")

			if !errors.Is(err, auth.ErrTokenInvalid) {
				t.Errorf("err = %v, want ErrTokenInvalid", err)
			}
			if account != (auth.Account{}) {
				t.Errorf("account = %+v, want the zero account", account)
			}
		})
	}
}

func TestAuthenticateNeverTouchesATokenItRefused(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	toucher := &recordingToucher{}

	_, _ = auth.NewAuthenticator(seeded("good-token"), toucher).
		Authenticate(context.Background(), "wrong-token", "")

	if len(toucher.touched) != 0 {
		t.Errorf("touched %v on a refused call, want nothing", toucher.touched)
	}
}

func TestAuthenticateAcceptsAGoodTokenEvenWhenRecordingItsUseFails(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	toucher := &recordingToucher{err: errors.New("db busy")}

	account, err := auth.NewAuthenticator(seeded("good-token"), toucher).
		Authenticate(context.Background(), "good-token", "")

	if err != nil {
		t.Fatalf("a failed touch refused a good token: %v", err)
	}
	if account.ID != "acct_1" {
		t.Errorf("account = %q, want acct_1", account.ID)
	}
}

func TestAuthenticateWorksWithNoToucherAtAll(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	if _, err := auth.NewAuthenticator(seeded("good-token"), nil).
		Authenticate(context.Background(), "good-token", ""); err != nil {
		t.Fatalf("Authenticate with no toucher: %v", err)
	}
}

func TestAuthenticateWrapsAStoreFailureRatherThanCallingItInvalid(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	s := seeded("good-token")
	s.resolveErr = errors.New("database is locked")

	_, err := auth.NewAuthenticator(s, nil).Authenticate(context.Background(), "good-token", "")

	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, auth.ErrTokenInvalid) {
		t.Error("a store outage was reported as an invalid token, which hides a real fault")
	}
	if !errors.Is(err, s.resolveErr) {
		t.Errorf("err = %v, want the store failure wrapped", err)
	}
}

func TestBearerTokenReadsOnlyABearerCredential(t *testing.T) {
	// covers: AC-2, AC-19
	t.Parallel()
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"a well formed header", "Bearer abc123", "abc123"},
		{"the scheme in any case", "bEaReR abc123", "abc123"},
		{"surrounding whitespace is trimmed", "Bearer   abc123  ", "abc123"},
		{"an empty header", "", ""},
		{"basic auth is not a bearer token", "Basic dXNlcjpwYXNz", ""},
		{"the scheme with nothing after it", "Bearer ", ""},
		{"a bare token with no scheme", "abc123", ""},
		{"a header shorter than the scheme", "Bear", ""},
		{"the scheme without its space", "Bearerabc123", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := auth.BearerToken(tt.header); got != tt.want {
				t.Errorf("BearerToken(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestRecordWritesTheRowItWasGiven(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	auditor := &collectingAuditor{}
	row := auth.Audit{
		AccountID:  "acct_1",
		Action:     auth.ActionDeploy,
		TargetType: "app",
		TargetID:   "app_1",
		Allowed:    true,
	}

	auth.Record(context.Background(), auditor, row)

	if len(auditor.rows) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(auditor.rows))
	}
	if auditor.rows[0] != row {
		t.Errorf("wrote %+v, want %+v", auditor.rows[0], row)
	}
}

func TestRecordRecordsADenialWithNoAccount(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	auditor := &collectingAuditor{}

	auth.Record(context.Background(), auditor, auth.Audit{
		Action: auth.ActionUpload,
		Reason: "token invalid",
	})

	if got := auditor.rows[0].AccountID; got != "" {
		t.Errorf("account = %q on a denial, want empty so the row reads as unattributed", got)
	}
	if auditor.rows[0].Allowed {
		t.Error("a denial was recorded as allowed")
	}
}

func TestRecordSurvivesAFailingOrAbsentAuditor(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	t.Run("a failing auditor does not panic or propagate", func(t *testing.T) {
		t.Parallel()
		auditor := &failingAuditor{}

		auth.Record(context.Background(), auditor, auth.Audit{Action: auth.ActionDeploy})

		if auditor.calls != 1 {
			t.Errorf("auditor called %d times, want 1", auditor.calls)
		}
	})
	t.Run("a nil auditor is a no-op", func(t *testing.T) {
		t.Parallel()
		auth.Record(context.Background(), nil, auth.Audit{Action: auth.ActionDeploy})
	})
}

func TestAuditActionsAreTheNamesTheLogIsQueriedBy(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	// Pinned because the audit log is only useful if the same event is named the
	// same way every time; renaming one silently orphans every prior row.
	for name, got := range map[string]string{
		"upload":       auth.ActionUpload,
		"deploy":       auth.ActionDeploy,
		"fetch_source": auth.ActionFetchSource,
		"status":       auth.ActionStatus,
	} {
		if got != name {
			t.Errorf("action = %q, want %q", got, name)
		}
	}
}
