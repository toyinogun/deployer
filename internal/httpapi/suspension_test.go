package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
)

// TestUploadRefusesASuspendedAccount is AC-11. A suspended account presented a
// credential that works, so it is refused as a decision rather than as a bad
// credential: 403 with the closed reason code, not the 401 an unknown token
// gets, and audited against the account it actually belongs to rather than as a
// null denial. covers: AC-11
func TestUploadRefusesASuspendedAccount(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE accounts SET disabled_at = ?`, ids.Stamp(time.Now().UTC())); err != nil {
		t.Fatalf("suspending the account: %v", err)
	}

	rec := h.do(t, post(t, goodToken, tarball(t, "source")))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a suspended account", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	if body["error"] != string(domain.ReasonAccountSuspended) {
		t.Errorf("body error = %q, want %q", body["error"], domain.ReasonAccountSuspended)
	}

	// Nothing was stored. A refused upload spends no volume and no row.
	var uploadRows int
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM uploads`).Scan(&uploadRows); err != nil {
		t.Fatalf("counting uploads: %v", err)
	}
	if uploadRows != 0 {
		t.Errorf("a refused upload stored %d row(s)", uploadRows)
	}

	// Audited against the account, unlike the null row an unknown token writes:
	// this caller is known, and which account was refused is the point of the row.
	var accountID *string
	var reason string
	err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT account_id, reason FROM audit_log WHERE action = ? AND outcome = 'denied'`,
		auth.ActionUpload).Scan(&accountID, &reason)
	if err != nil {
		t.Fatalf("reading the denial row: %v", err)
	}
	if accountID == nil || *accountID == "" {
		t.Error("the denial named no account, so the audit trail cannot say who was refused")
	}
	if !strings.Contains(reason, string(domain.ReasonAccountSuspended)) {
		t.Errorf("the denial reason is %q, want the account_suspended code", reason)
	}
}
