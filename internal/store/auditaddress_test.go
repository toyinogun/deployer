package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/store"
)

// isReserved is the sentinel a reserved name create answers with.
func isReserved(err error) bool { return errors.Is(err, store.ErrAppNameReserved) }

// TestAnAuditRowCarriesTheAddressAndAPlatformRowDoesNot is AC-17. A request
// surface sets the address where it builds the entry; a platform initiated write
// leaves it unset, which is written as null.
func TestAnAuditRowCarriesTheAddressAndAPlatformRowDoesNot(t *testing.T) {
	// covers: AC-17
	t.Parallel()
	s, _ := newStore(t)
	ctx := t.Context()

	if err := s.RecordAudit(ctx, store.AuditEntry{
		Action: "login", Allowed: true, ClientAddress: "203.0.113.7",
	}); err != nil {
		t.Fatalf("recording the visitor's row: %v", err)
	}
	// A scheduled backup run, a suspension sweep and a reconcile drive all look
	// like this: no request, so no address.
	if err := s.RecordAudit(ctx, store.AuditEntry{Action: "admin", Allowed: true}); err != nil {
		t.Fatalf("recording the platform's row: %v", err)
	}

	rows, err := s.DB().QueryContext(ctx, `SELECT action, client_address FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the rows back: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("closing the rows: %v", err)
		}
	}()

	var got []struct {
		action string
		addr   *string
	}
	for rows.Next() {
		var r struct {
			action string
			addr   *string
		}
		if err := rows.Scan(&r.action, &r.addr); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].addr == nil || *got[0].addr != "203.0.113.7" {
		t.Errorf("the visitor's row carries %v, want 203.0.113.7", got[0].addr)
	}
	if got[1].addr != nil {
		t.Errorf("the platform's row carries %q, want null", *got[1].addr)
	}
}

// TestTheSweepNullsOldAddressesAndKeepsTheRows is AC-18 and AC-18a. The address
// goes and the trail stays, so a person reading an old denial still sees the
// denial, just not who it was.
func TestTheSweepNullsOldAddressesAndKeepsTheRows(t *testing.T) {
	// covers: AC-18, AC-18a
	t.Parallel()
	s, _ := newStore(t)
	ctx := t.Context()
	const retention = 90 * 24 * time.Hour

	if err := s.RecordAudit(ctx, store.AuditEntry{
		Action: "login", Allowed: false, ClientAddress: "203.0.113.7",
	}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	// Aged past the window by hand. The clock the store writes with is fixed, so
	// moving the row is the only way to reach the other side of the cutoff.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE audit_log SET occurred_at = '2020-01-01T00:00:00Z'`); err != nil {
		t.Fatalf("ageing the row: %v", err)
	}
	if err := s.RecordAudit(ctx, store.AuditEntry{
		Action: "login", Allowed: true, ClientAddress: "198.51.100.4",
	}); err != nil {
		t.Fatalf("recording the recent row: %v", err)
	}

	n, err := s.ClearOldAuditAddresses(ctx, retention)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep cleared %d rows, want 1", n)
	}

	var rows int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 2 {
		t.Errorf("the table holds %d rows after the sweep, want 2: the row itself is never deleted", rows)
	}
	var cleared, kept *string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT client_address FROM audit_log WHERE occurred_at = '2020-01-01T00:00:00Z'`).Scan(&cleared); err != nil {
		t.Fatalf("reading the old row: %v", err)
	}
	if cleared != nil {
		t.Errorf("the old row still carries %q", *cleared)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT client_address FROM audit_log WHERE occurred_at > '2020-01-01T00:00:00Z'`).Scan(&kept); err != nil {
		t.Fatalf("reading the recent row: %v", err)
	}
	if kept == nil || *kept != "198.51.100.4" {
		t.Errorf("the recent row carries %v, want 198.51.100.4: a row inside the window is untouched", kept)
	}

	// Running again clears nothing, which is what the IS NOT NULL guard is for.
	again, err := s.ClearOldAuditAddresses(ctx, retention)
	if err != nil {
		t.Fatalf("sweeping again: %v", err)
	}
	if again != 0 {
		t.Errorf("a second sweep rewrote %d rows, want 0", again)
	}
}

// TestCreateAppRefusesAReservedName is AC-6 and AC-7 at the one call that inserts
// an app row, which is where the cap already lives for the same reason.
func TestCreateAppRefusesAReservedName(t *testing.T) {
	// covers: AC-6, AC-7
	t.Parallel()
	s, _ := newStore(t)
	ctx := t.Context()
	acc, err := s.CreateAccount(ctx, "somebody")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}

	for _, name := range []string{"console", "Console", "registry"} {
		if _, err := s.CreateApp(ctx, acc.ID, name, 10); err == nil {
			t.Errorf("%q was created, so an app could claim a name the platform keeps", name)
		} else if !isReserved(err) {
			t.Errorf("%q was refused with %v, want the reserved sentinel", name, err)
		}
	}
	if _, err := s.CreateApp(ctx, acc.ID, "console-shop", 10); err != nil {
		t.Errorf("a name that merely starts with a reserved label was refused: %v", err)
	}

	var apps int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&apps); err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if apps != 1 {
		t.Errorf("the table holds %d app rows, want 1: a refused create writes nothing", apps)
	}
}
