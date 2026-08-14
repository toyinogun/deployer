package store_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// TestResolveTokenOnlyAcceptsALiveToken checks that unknown, revoked, expired,
// and disabled all look the same from outside. Verifies AC-13.
func TestResolveTokenOnlyAcceptsALiveToken(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	acc, err := s.CreateAccount(ctx, "toyin")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}

	live, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "laptop", TokenHash: "hash-live", Prefix: "dep_1234",
	})
	if err != nil {
		t.Fatalf("minting the live token: %v", err)
	}
	got, tok, err := s.ResolveToken(ctx, "hash-live")
	if err != nil {
		t.Fatalf("resolving a live token: %v", err)
	}
	if got.ID != acc.ID || tok.ID != live.ID {
		t.Errorf("resolved to account %s token %s, want %s and %s", got.ID, tok.ID, acc.ID, live.ID)
	}
	// Only the hash and a readable head are stored; the raw token is not here.
	if tok.TokenHash != "hash-live" || tok.TokenPrefix != "dep_1234" {
		t.Errorf("token stored as hash %q prefix %q", tok.TokenHash, tok.TokenPrefix)
	}

	revoked, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "old", TokenHash: "hash-revoked", Prefix: "dep_2222",
	})
	if err != nil {
		t.Fatalf("minting the token to revoke: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, revoked.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	if _, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "expired", TokenHash: "hash-expired", Prefix: "dep_3333",
		ExpiresAt: ids.Stamp(testStart.Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("minting the expired token: %v", err)
	}

	for _, hash := range []string{"hash-unknown", "hash-revoked", "hash-expired"} {
		if _, _, err := s.ResolveToken(ctx, hash); !errors.Is(err, store.ErrTokenInvalid) {
			t.Errorf("resolving %q returned %v, want ErrTokenInvalid", hash, err)
		}
	}

	// A suspended account's good token still resolves here, carrying the stamp.
	// The query stopped filtering it out on purpose: telling a live token on a
	// suspended account apart from a dead token is what lets a surface answer
	// account_suspended, and that decision is made once, in auth.Authenticate,
	// rather than twice (spec 0018, AC-12).
	if _, err := s.DB().ExecContext(ctx, `UPDATE accounts SET disabled_at = ? WHERE id = ?`,
		ids.Stamp(testStart), acc.ID); err != nil {
		t.Fatalf("suspending the account: %v", err)
	}
	suspended, _, err := s.ResolveToken(ctx, "hash-live")
	if err != nil {
		t.Fatalf("a suspended account's live token no longer resolves: %v", err)
	}
	if suspended.DisabledAt == nil {
		t.Error("the resolved account carries no disabled_at, so nothing above can tell it apart")
	}
	// The token shapes that must stay indistinguishable still are.
	for _, hash := range []string{"hash-unknown", "hash-revoked", "hash-expired"} {
		if _, _, err := s.ResolveToken(ctx, hash); !errors.Is(err, store.ErrTokenInvalid) {
			t.Errorf("resolving %q on a suspended account returned %v, want ErrTokenInvalid", hash, err)
		}
	}
}

// TestAuditRecordsBothOutcomes checks that a denial with no account still
// writes a row. Verifies AC-14.
func TestAuditRecordsBothOutcomes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.RecordAudit(ctx, store.AuditEntry{
		AccountID: f.account.ID, Action: "deploy",
		TargetType: "app", TargetID: f.app.ID, Allowed: true,
	}); err != nil {
		t.Fatalf("recording an allowed outcome: %v", err)
	}
	if err := s.RecordAudit(ctx, store.AuditEntry{
		Action: "deploy", Allowed: false, Reason: "token_invalid",
	}); err != nil {
		t.Fatalf("recording a denial with no account: %v", err)
	}

	rows, err := s.DB().QueryContext(ctx,
		`SELECT account_id, outcome, reason FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("closing rows: %v", err)
		}
	}()

	type entry struct {
		account *string
		outcome string
		reason  *string
	}
	var got []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.account, &e.outcome, &e.reason); err != nil {
			t.Fatalf("scanning the audit log: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d audit rows, want 2", len(got))
	}
	if got[0].outcome != "allowed" || got[0].account == nil || *got[0].account != f.account.ID {
		t.Errorf("the allowed row is wrong: %+v", got[0])
	}
	if got[1].outcome != "denied" || got[1].account != nil {
		t.Errorf("a denial with no resolved account should carry a null account: %+v", got[1])
	}
	if got[1].reason == nil || *got[1].reason != "token_invalid" {
		t.Errorf("the denial did not record why: %v", got[1].reason)
	}
}

// TestConfigReadPathsAreSplit checks that a tool response can never carry a
// secret value while the deploy path still gets one. Verifies AC-15.
func TestConfigReadPathsAreSplit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("setting a plain key: %v", err)
	}
	if err := s.SetConfig(ctx, f.app.ID, "API_KEY", "s3cret", true); err != nil {
		t.Fatalf("setting a secret key: %v", err)
	}

	response, err := s.ListConfigForResponse(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the response path: %v", err)
	}
	if len(response) != 2 {
		t.Fatalf("the response path returned %d keys, want 2", len(response))
	}
	byKey := map[string]store.ConfigEntry{}
	for _, e := range response {
		byKey[e.Key] = e
	}
	if e := byKey["API_KEY"]; !e.IsSecret || e.Value != "" {
		t.Errorf("the response path leaked a secret value: %+v", e)
	}
	if e := byKey["LOG_LEVEL"]; e.IsSecret || e.Value != "debug" {
		t.Errorf("the response path hid a plain value: %+v", e)
	}

	deploy, err := s.ListConfigForDeploy(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the deploy path: %v", err)
	}
	for _, e := range deploy {
		if e.Value == "" {
			t.Errorf("the deploy path is missing the value for %q", e.Key)
		}
	}
}

// TestConfigKeysAreConstrained checks the CHECK constraint on key shape.
func TestConfigKeysAreConstrained(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	for _, key := range []string{"lower_case", "1LEADING_DIGIT", "HAS-DASH", "HAS SPACE", ""} {
		if err := s.SetConfig(ctx, f.app.ID, key, "x", false); !errors.Is(err, store.ErrInvalidKey) {
			t.Errorf("SetConfig(%q) returned %v, want ErrInvalidKey", key, err)
		}
	}
	for _, key := range []string{"PORT", "_PRIVATE", "DB_URL_2"} {
		if err := s.SetConfig(ctx, f.app.ID, key, "x", false); err != nil {
			t.Errorf("SetConfig(%q) was refused: %v", key, err)
		}
	}
	if err := s.UnsetConfig(ctx, f.app.ID, "PORT"); err != nil {
		t.Errorf("unsetting a key that exists: %v", err)
	}
	if err := s.UnsetConfig(ctx, f.app.ID, "PORT"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unsetting a key twice returned %v, want ErrNotFound", err)
	}
}

// TestRedeemIsSingleUse checks that a fetch token can be spent exactly once.
func TestRedeemIsSingleUse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	f := newFixture(t, s)

	up, err := s.RedeemUpload(ctx, f.upload.FetchTokenHash)
	if err != nil {
		t.Fatalf("redeeming: %v", err)
	}
	if up.RedeemedAt == nil {
		t.Error("a redeemed upload should be stamped")
	}
	if _, err := s.RedeemUpload(ctx, f.upload.FetchTokenHash); !errors.Is(err, store.ErrUploadRedeemed) {
		t.Errorf("a second redeem returned %v, want ErrUploadRedeemed", err)
	}
	if _, err := s.RedeemUpload(ctx, "hash-nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown token returned %v, want ErrNotFound", err)
	}

	stale := newUpload(t, s, f.account.ID, "hash-stale")
	clock.Advance(2 * time.Hour)
	if _, err := s.RedeemUpload(ctx, stale.FetchTokenHash); !errors.Is(err, store.ErrUploadExpired) {
		t.Errorf("an expired token returned %v, want ErrUploadExpired", err)
	}
}

// TestRetentionSweep checks what the sweep removes and, just as importantly,
// what it leaves. Verifies AC-17.
func TestRetentionSweep(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	f := newFixture(t, s)

	// A failed deployment, aged past the window, plus the upload it used.
	failed := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, failed.ID, domain.StateBuilding)
	if _, err := s.Transition(ctx, failed.ID, domain.StateFailed, "build_failed", "compile error"); err != nil {
		t.Fatalf("failing the deployment: %v", err)
	}

	// A healthy deployment with a release, which the sweep must not touch.
	good := newUpload(t, s, f.account.ID, "hash-good")
	_, rel := deployToHealthy(t, s, f, good.ID, "sha256:aaa")

	if err := s.RecordAudit(ctx, store.AuditEntry{AccountID: f.account.ID, Action: "deploy", Allowed: true}); err != nil {
		t.Fatalf("recording audit: %v", err)
	}

	// A recent failure, inside the window, which must survive.
	clock.Advance(100 * 24 * time.Hour)
	recentUpload := newUpload(t, s, f.account.ID, "hash-recent")
	recent := mustCreateDeployment(t, s, f, recentUpload.ID)
	if _, err := s.Transition(ctx, recent.ID, domain.StateFailed, "build_failed", ""); err != nil {
		t.Fatalf("failing the recent deployment: %v", err)
	}

	counts, err := s.Sweep(ctx, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if counts.Deployments != 1 {
		t.Errorf("swept %d deployments, want only the aged failure", counts.Deployments)
	}

	if _, err := s.GetDeployment(ctx, failed.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the aged failure survived: %v", err)
	}
	if _, err := s.GetDeployment(ctx, recent.ID); err != nil {
		t.Errorf("the recent failure was swept: %v", err)
	}
	if _, err := s.GetRelease(ctx, rel.ID); err != nil {
		t.Errorf("the sweep removed a release: %v", err)
	}

	events, err := s.ListDeploymentEvents(ctx, failed.ID)
	if err != nil {
		t.Fatalf("reading the swept deployment's events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("%d events outlived their deployment", len(events))
	}

	var auditRows int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&auditRows); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if auditRows != 1 {
		t.Errorf("the sweep touched the audit log: %d rows left", auditRows)
	}

	// The aged upload is gone now that no deployment names it; the one the
	// recent deployment still uses stays.
	if _, err := s.GetUpload(ctx, f.upload.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the orphaned upload survived: %v", err)
	}
	if _, err := s.GetUpload(ctx, recentUpload.ID); err != nil {
		t.Errorf("an upload a live deployment still names was swept: %v", err)
	}
}

// TestMigrationUpThenDownLeavesTheFileEmpty verifies AC-1.
func TestMigrationUpThenDownLeavesTheFileEmpty(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "deployer.db")})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	want := []string{
		"accounts", "api_tokens", "apps", "app_config", "uploads",
		"deployments", "deployment_events", "releases", "audit_log",
		// Added by spec 0007's 00002_identity.sql.
		"sessions", "email_tokens",
		// Added by spec 0015's 00003_invites.sql.
		"invites",
	}
	for _, table := range want {
		if !tableExists(t, s, table) {
			t.Errorf("%s was not created", table)
		}
	}

	// A second up on an already migrated file is a no-op, which is what makes it
	// safe on every boot.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating up twice: %v", err)
	}

	// One MigrateDown rolls back one migration, so an empty file takes as many
	// calls as there are migrations. The count is read off the embedded
	// migrations rather than typed here, because typing it is how adding the
	// third one left this test rolling back two and passing anyway.
	for range store.MigrationCount() {
		if err := s.MigrateDown(ctx); err != nil {
			t.Fatalf("migrating down: %v", err)
		}
	}
	for _, table := range want {
		if tableExists(t, s, table) {
			t.Errorf("%s survived the down migration", table)
		}
	}
}

// TestReadyFailsWhileMigrationsArePending verifies AC-4: an open but unmigrated
// database is not ready, so the readiness probe pulls the pod out of its Service
// instead of serving traffic against a schema that is not there yet.
func TestReadyFailsWhileMigrationsArePending(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "deployer.db")})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()

	// Open does not migrate, so the file is reachable but the schema is missing.
	if err := s.Ready(ctx); err == nil {
		t.Fatal("Ready said the store was ready with migrations still pending")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	if err := s.Ready(ctx); err != nil {
		t.Errorf("Ready still failed after migrating: %v", err)
	}

	// Rolling back puts the migration pending again, so readiness goes back down
	// rather than latching on the first success.
	if err := s.MigrateDown(ctx); err != nil {
		t.Fatalf("migrating down: %v", err)
	}
	if err := s.Ready(ctx); err == nil {
		t.Error("Ready stayed ready after the schema was rolled back")
	}
}

func tableExists(t *testing.T, s *store.Store, name string) bool {
	t.Helper()
	var n int
	err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		t.Fatalf("looking for table %s: %v", name, err)
	}
	return n > 0
}

// TestPragmasAreSetOnEveryConnection verifies AC-2: the settings are on the DSN,
// so a connection the pool opens later carries them too.
func TestPragmasAreSetOnEveryConnection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := store.Open(store.Options{
		Path:        filepath.Join(t.TempDir(), "deployer.db"),
		BusyTimeout: 7500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	// Hold several connections open at once so the pool has to make new ones,
	// then check each separately.
	const conns = 4
	var open []interface{ Close() error }
	for i := range conns {
		c, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatalf("opening connection %d: %v", i, err)
		}
		open = append(open, c)

		var journal string
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatalf("reading journal_mode: %v", err)
		}
		if journal != "wal" {
			t.Errorf("connection %d has journal_mode %q, want wal", i, journal)
		}
		var fk int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("reading foreign_keys: %v", err)
		}
		if fk != 1 {
			t.Errorf("connection %d does not enforce foreign keys", i)
		}
		var busy int
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("reading busy_timeout: %v", err)
		}
		if busy != 7500 {
			t.Errorf("connection %d has busy_timeout %d, want 7500", i, busy)
		}
	}
	for _, c := range open {
		if err := c.Close(); err != nil {
			t.Errorf("closing a connection: %v", err)
		}
	}
}
