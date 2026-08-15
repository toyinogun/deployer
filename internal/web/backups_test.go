package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/backup"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// fakeBackups stands in for the backup service. It invents no behaviour the real
// one does not have: it answers whether backups are configured, and it answers a
// run with one of the three things a real run answers with, a success, a closed
// reason code, or the index's refusal.
type fakeBackups struct {
	configured bool
	reason     domain.BackupReason
	err        error
	calls      []string
}

func (f *fakeBackups) Configured() bool { return f.configured }

func (f *fakeBackups) Run(_ context.Context, accountID string) (domain.BackupReason, error) {
	f.calls = append(f.calls, accountID)
	return f.reason, f.err
}

// TestBackupsPageIsAdminOnly is AC-17 on the page: the same gate every other
// admin page uses, so a signed in non admin gets exactly what that gate already
// gives them and no hint of what the page holds.
// covers: AC-17
func TestBackupsPageIsAdminOnly(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "admin@example.test") // the first account is the admin
	ordinary := h.signIn(t, "ordinary@example.test")

	res := h.get(t, "/admin/backups", ordinary)
	if res.Code != http.StatusForbidden {
		t.Fatalf("a non admin: got %d, want 403", res.Code)
	}
	if strings.Contains(res.Body.String(), "Back up now") {
		t.Error("a refusal leaked the page's contents")
	}
}

// TestBackupsPageSaysWhenBackupsAreOff is AC-18: an unconfigured platform gets a
// stated absence rather than an empty table, which would read as healthy.
// covers: AC-18
func TestBackupsPageSaysWhenBackupsAreOff(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")
	h.backups.configured = false

	res := h.get(t, "/admin/backups", admin)
	if res.Code != http.StatusOK {
		t.Fatalf("the page: got %d, want 200", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, "Backups are not configured") {
		t.Errorf("the page did not name the unconfigured state: %s", body)
	}
	if strings.Contains(body, "Back up now") {
		t.Error("there should be no run button with nothing configured")
	}
}

// TestBackupRunCarriesTheSynchroniserToken is AC-19: a post without a valid
// token is refused before anything runs.
// covers: AC-19
func TestBackupRunCarriesTheSynchroniserToken(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	if rec := h.post(t, "/admin/backups/run", url.Values{}, admin, nil); rec.Code != http.StatusForbidden {
		t.Errorf("running without the token: got %d, want 403", rec.Code)
	}
	if len(h.backups.calls) != 0 {
		t.Errorf("a refused post started %d runs", len(h.backups.calls))
	}
}

// TestBackupRunNamesTheAdminAndIsAudited is AC-21 and AC-22: the run carries the
// admin's account id, and the press writes an audit row.
// covers: AC-21, AC-22
func TestBackupRunNamesTheAdminAndIsAudited(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	res := h.post(t, "/admin/backups/run", url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("running: got %d, want 200: %s", res.Code, res.Body)
	}
	if len(h.backups.calls) != 1 || h.backups.calls[0] == "" {
		t.Fatalf("the run should name the admin, got %v", h.backups.calls)
	}
	if _, ok := h.audit.last("admin"); !ok {
		t.Error("the press wrote no audit row")
	}
}

// TestBackupRunRefusedWhileInFlight is AC-20 and AC-22: the refusal comes from
// the index, the caller is told why, and the press is still audited.
// covers: AC-20, AC-22
func TestBackupRunRefusedWhileInFlight(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")
	h.backups.err = backup.ErrInFlight

	res := h.post(t, "/admin/backups/run", url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("a refusal: got %d, want 409", res.Code)
	}
	if !strings.Contains(res.Body.String(), "already running") {
		t.Errorf("the caller was not told why: %s", res.Body)
	}
	if !h.audit.hasReason("admin", "run: in_flight") {
		t.Error("a refused run wrote no audit row naming the refusal")
	}
}

// TestBackupRunFailureShowsOnlyTheReasonCode is AC-10 and AC-14 on the page: the
// closed code reaches the caller and nothing about where the backups live does.
// covers: AC-10, AC-14
func TestBackupRunFailureShowsOnlyTheReasonCode(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")
	h.backups.reason = domain.BackupUploadFailed

	res := h.post(t, "/admin/backups/run", url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	body := res.Body.String()
	if !strings.Contains(body, "upload_failed") {
		t.Errorf("the reason code was not shown: %s", body)
	}
	for _, leak := range []string{"cloudflarestorage", "AccessKey", "age1"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page leaked %q", leak)
		}
	}
}

// TestBackupsPageShowsWhenARunEnded is AC-17 on the column /check verify found
// missing: the record is only useful for judging a schedule if a row says when
// it ended as well as when it started, and a run still going says neither.
// covers: AC-17
func TestBackupsPageShowsWhenARunEnded(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	ended, err := h.store.StartBackupRun(t.Context(), "")
	if err != nil {
		t.Fatalf("starting the run that ends: %v", err)
	}
	h.clock.T = h.clock.T.Add(90 * time.Second)
	if err := h.store.FinishBackupRunSucceeded(t.Context(), ended.ID, store.BackupResult{
		ObjectKey: "db/20260814T120000Z-" + ended.ID + ".age", SizeBytes: 1 << 20, Checksum: "abc",
	}); err != nil {
		t.Fatalf("ending the run: %v", err)
	}

	res := h.get(t, "/admin/backups", admin)
	body := res.Body.String()
	if !strings.Contains(body, "Finished") {
		t.Error("the runs table has no finished column")
	}
	// The stamp the clock moved to, rendered the way the page renders one.
	if !strings.Contains(body, "12:01 UTC") {
		t.Errorf("the page did not show when the run ended: %s", body)
	}
}
