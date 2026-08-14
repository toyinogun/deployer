package web

import (
	"net/http"
	"strings"
	"testing"
)

// TestTheAppsPageShowsTheAccountsUsage is the one place a person discovers their
// ceiling without hitting it. The count is the same live read the deploy refusal
// is decided from, so the page and the tool can never report different numbers.
func TestTheAppsPageShowsTheAccountsUsage(t *testing.T) {
	// covers: AC-10
	h := newHarness(t, nil)
	cookie := h.signIn(t, "usage@example.test")
	h.data.appsHeld = 3

	rec := h.get(t, "/apps", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "3 of 10 apps") {
		t.Errorf("the apps page does not show the usage line")
	}
	// Below the cap there is no notice, because a warning shown at three of ten
	// is a warning nobody reads at ten of ten.
	if strings.Contains(body, "You are at your limit") {
		t.Errorf("the at cap notice is shown while the account is below its cap")
	}
}

// TestTheAppsPageWarnsAtTheCap is the notice a person needs before their agent
// hits the refusal, in the words the refusal will use.
func TestTheAppsPageWarnsAtTheCap(t *testing.T) {
	// covers: AC-11
	for name, held := range map[string]int{"at the cap": 10, "over it": 12} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			cookie := h.signIn(t, "full@example.test")
			h.data.appsHeld = held

			body := h.get(t, "/apps", cookie).Body.String()
			want := "You are at your limit of 10 apps. Deploying a new app will be refused until you delete one."
			if !strings.Contains(body, want) {
				t.Errorf("the apps page does not carry the at cap notice with %d apps held", held)
			}
		})
	}
}

// TestTheAdminAccountsPageShowsEachAccountsAppCount is the operator's half:
// noticing somebody is running out of room before they ask.
func TestTheAdminAccountsPageShowsEachAccountsAppCount(t *testing.T) {
	// covers: AC-12
	h := newHarness(t, nil)
	// The first account registered is the administrator.
	admin := h.signIn(t, "first@example.test")
	h.signIn(t, "second@example.test")

	var adminID string
	if err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT id FROM accounts WHERE email = ?`, "first@example.test").Scan(&adminID); err != nil {
		t.Fatalf("reading the admin's id: %v", err)
	}
	h.data.perAccountApps = map[string]int{adminID: 4}

	rec := h.get(t, "/admin/accounts", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/accounts: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "4 apps") {
		t.Errorf("the admin accounts page does not show the account's app count")
	}
	// An account absent from the grouped read holds none, not an unknown number.
	if !strings.Contains(body, "0 apps") {
		t.Errorf("an account with no apps does not read as 0 apps")
	}
}
