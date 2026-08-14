package httpapi_test

import (
	"net/http"
	"testing"
)

// TestTheJSONAdminRoutesSuspendAndRestore is AC-19 on the JSON surface: it goes
// through the same use case the admin page does, so a suspension made here means
// the same thing as one made in the browser. covers: AC-19
func TestTheJSONAdminRoutesSuspendAndRestore(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	victim := h.registerAndVerify(t, "victim@example.com")
	target := h.accountIDOf(t, "victim@example.com")

	if got := h.do(t, "POST", "/v1/admin/accounts/"+target+"/disable", nil, admin); got.Code != http.StatusNoContent {
		t.Fatalf("suspending: got %d %s, want 204", got.Code, got.Body)
	}
	// The lockout landed: the session it held is dead.
	if got := h.do(t, "GET", "/v1/auth/me", nil, victim); got.Code == http.StatusOK {
		t.Error("a suspended account's session still resolves")
	}

	if got := h.do(t, "POST", "/v1/admin/accounts/"+target+"/enable", nil, admin); got.Code != http.StatusNoContent {
		t.Fatalf("restoring: got %d %s, want 204", got.Code, got.Body)
	}
}

// TestTheJSONAdminRouteRefusesSelfSuspension is AC-17 on the JSON surface. Both
// surfaces answer for the same rule, so neither may be the soft way round it.
// covers: AC-17, AC-19
func TestTheJSONAdminRouteRefusesSelfSuspension(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	self := h.accountIDOf(t, "admin@example.com")

	got := h.do(t, "POST", "/v1/admin/accounts/"+self+"/disable", nil, admin)

	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("suspending yourself over JSON: got %d %s, want 422", got.Code, got.Body)
	}
	// Still an admin, still signed in.
	if still := h.do(t, "GET", "/v1/admin/accounts", nil, admin); still.Code != http.StatusOK {
		t.Errorf("the admin lost their own access: got %d", still.Code)
	}
}

// TestSuspensionAddedNoMigration is AC-1. Suspension is a column that has existed
// since the first migration, so a previous binary runs against the same database
// unharmed. covers: AC-1
func TestSuspensionAddedNoMigration(t *testing.T) {
	h := newIDHarness(t, true)
	var version int64
	if err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("reading the migration version: %v", err)
	}
	// The number this feature found. Raising it means a migration was added,
	// which this spec decided against: there is no second suspension state to
	// store, and a migration is the one thing that stops the previous image from
	// starting against the same file.
	const versionBeforeSuspension = 3
	if version != versionBeforeSuspension {
		t.Errorf("the schema is at version %d, want %d: suspension adds no migration",
			version, versionBeforeSuspension)
	}
}

// accountIDOf reads an account's id straight out of the database, because the
// admin routes are addressed by id and the registration response is not.
func (h *idHarness) accountIDOf(t *testing.T, email string) string {
	t.Helper()
	acc, err := h.store.GetAccountByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("reading the account for %s: %v", email, err)
	}
	return acc.ID
}
