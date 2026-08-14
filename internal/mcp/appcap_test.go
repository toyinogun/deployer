package mcp_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/store"
)

// anotherToken mints a second API token on an account that already has one,
// which is what a person does when they point a second agent at the platform.
// The account is the same row; only the credential differs (spec 0016, AC-15).
func (h *ownershipHarness) anotherToken(t *testing.T, who person) person {
	t.Helper()
	raw, err := identity.NewAPIToken()
	if err != nil {
		t.Fatalf("minting a second token: %v", err)
	}
	if _, err := h.store.CreateAPIToken(t.Context(), store.NewToken{
		AccountID: who.account.ID, Name: "second agent",
		TokenHash: identity.HashSecret(raw), Prefix: identity.TokenPrefix(raw),
	}); err != nil {
		t.Fatalf("storing the second token: %v", err)
	}
	return person{account: who.account, token: raw}
}

// deployNamed is one deploy of one name as one person, with a fresh upload.
func (h *ownershipHarness) deployNamed(t *testing.T, who person, name string) (map[string]any, string, bool) {
	t.Helper()
	return h.call(t, who, "deploy_app", map[string]any{
		"name": name, "upload_id": h.upload(t, who),
	})
}

// fill deploys each name for one person, failing the test if any is refused, so
// a test that is about the ceiling starts at it.
func (h *ownershipHarness) fill(t *testing.T, who person, names ...string) {
	t.Helper()
	for _, name := range names {
		if out, said, isErr := h.deployNamed(t, who, name); isErr {
			t.Fatalf("filling with %q was refused: %v %s", name, out, said)
		}
	}
}

// settle ends every deployment still in flight, so a delete is not refused for
// a reason this test is not about.
func (h *ownershipHarness) settle(t *testing.T) {
	t.Helper()
	live, err := h.store.ListNonTerminalDeployments(t.Context())
	if err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	for _, dep := range live {
		if _, err := h.store.Transition(t.Context(), dep.ID, domain.StateFailed,
			string(domain.ReasonTimeout), ""); err != nil {
			t.Fatalf("ending deployment %s: %v", dep.ID, err)
		}
	}
}

// appCount is how many live apps an account holds, read straight from the
// database rather than through the surface being tested.
func appCount(t *testing.T, s *store.Store, accountID string) int {
	t.Helper()
	n, err := s.CountLiveAppsByAccount(t.Context(), accountID)
	if err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	return n
}

// deploymentCount is how many deployment rows an account holds, which is what
// proves a refused deploy started nothing.
func deploymentCount(t *testing.T, s *store.Store, accountID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM deployments WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		t.Fatalf("counting deployments: %v", err)
	}
	return n
}

// TestAnAccountAtItsCapIsRefusedANewApp is the whole refusal, proved through a
// real client and server session rather than by calling the handler: the code,
// the two numbers, the audit row, and the fact that nothing was written.
func TestAnAccountAtItsCapIsRefusedANewApp(t *testing.T) {
	// covers: AC-1, AC-2, AC-3, AC-9, AC-16
	h := newCappedHarness(t, 2)
	h.fill(t, h.a, "checkout", "billing")

	deploysBefore := deploymentCount(t, h.store, h.a.account.ID)
	auditsBefore := auditCount(t, h.store, h.a.account.ID)
	upload := h.upload(t, h.a)

	out, said, isErr := h.call(t, h.a, "deploy_app", map[string]any{
		"name": "invoices", "upload_id": upload,
	})
	if !isErr {
		t.Fatalf("the third app was accepted, want a refusal: %v", out)
	}
	// The exact line, not a prefix: the numbers are the point of it, and an agent
	// that cannot read them has to guess how much room it has (AC-3).
	want := string(domain.ReasonAppLimitReached) + ": " +
		domain.ReasonAppLimitReached.Message() + " (2 of 2 used)"
	if strings.TrimSpace(said) != want {
		t.Errorf("the refusal reads %q, want %q", strings.TrimSpace(said), want)
	}

	// Nothing was written: no app row and no deployment row.
	if got := appCount(t, h.store, h.a.account.ID); got != 2 {
		t.Errorf("the account holds %d apps after the refusal, want 2", got)
	}
	if got := deploymentCount(t, h.store, h.a.account.ID); got != deploysBefore {
		t.Errorf("the refusal wrote %d deployment rows, want none", got-deploysBefore)
	}

	// One audit row, the way every other deploy_app refusal audits (AC-9).
	if got := auditCount(t, h.store, h.a.account.ID) - auditsBefore; got != 1 {
		t.Errorf("the refusal wrote %d audit rows, want exactly 1", got)
	}
	var action, outcome string
	var reason, target *string
	if err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT action, outcome, reason, target_id FROM audit_log
		 WHERE account_id = ? ORDER BY id DESC LIMIT 1`,
		h.a.account.ID).Scan(&action, &outcome, &reason, &target); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if action != "deploy" || outcome != "denied" ||
		reason == nil || *reason != string(domain.ReasonAppLimitReached) {
		t.Errorf("the audit row is action %q outcome %q reason %v, want deploy / denied / %s",
			action, outcome, reason, domain.ReasonAppLimitReached)
	}
	if target != nil {
		t.Errorf("the audit row names app %q, but no app was created", *target)
	}

	// The upload was never redeemed, so the same id still works once a slot is
	// free. That is what makes delete and retry a real recovery rather than a
	// second upload (AC-2).
	up, err := h.store.GetUpload(t.Context(), upload)
	if err != nil {
		t.Fatalf("reading the upload back: %v", err)
	}
	if up.RedeemedAt != nil {
		t.Errorf("the refused deploy spent the upload, so the caller cannot retry with it")
	}
}

// TestAnAccountAtItsCapStillRedeploysWhatItHas is the other half of the rule:
// the cap gates creating an app, never deploying one.
func TestAnAccountAtItsCapStillRedeploysWhatItHas(t *testing.T) {
	// covers: AC-4
	h := newCappedHarness(t, 2)
	h.fill(t, h.a, "checkout", "billing")

	out, said, isErr := h.deployNamed(t, h.a, "checkout")
	if isErr {
		t.Fatalf("redeploying an app the account already holds was refused: %v %s", out, said)
	}
	if out["deployment_id"] == "" {
		t.Errorf("the redeploy started nothing: %v", out)
	}
}

// TestDeletingAnAppFreesASlotStraightAway is the recovery the refusal tells the
// caller to take: it has to work on the next call, not after a sweep.
func TestDeletingAnAppFreesASlotStraightAway(t *testing.T) {
	// covers: AC-5
	h := newCappedHarness(t, 2)
	h.fill(t, h.a, "checkout", "billing")

	if _, said, isErr := h.deployNamed(t, h.a, "invoices"); !isErr {
		t.Fatalf("the third app was accepted before anything was deleted: %s", said)
	}
	// A queued deployment blocks a delete, which is a rule of its own. Ending
	// them puts the account in the state a person deleting an app is actually in.
	h.settle(t)
	if out, said, isErr := h.call(t, h.a, "delete_app", map[string]any{"name": "billing"}); isErr {
		t.Fatalf("the delete was refused: %v %s", out, said)
	}
	if out, said, isErr := h.deployNamed(t, h.a, "invoices"); isErr {
		t.Fatalf("the deploy was still refused after a delete freed a slot: %v %s", out, said)
	}
}

// TestAnAccountOverTheCapKeepsEverythingItHas is the case a lowered number
// creates. Nothing is torn down and everything still deploys, because the cap is
// a gate on one create rather than a rule enforced backwards.
func TestAnAccountOverTheCapKeepsEverythingItHas(t *testing.T) {
	// covers: AC-14
	h := newCappedHarness(t, 3)
	h.fill(t, h.a, "checkout", "billing", "invoices")

	// The same store, the same rows, a lower number.
	h.recap(t, 1)

	if got := appCount(t, h.store, h.a.account.ID); got != 3 {
		t.Errorf("the account holds %d apps after the cap was lowered, want all 3 kept", got)
	}
	for _, name := range []string{"checkout", "billing", "invoices"} {
		if out, said, isErr := h.deployNamed(t, h.a, name); isErr {
			t.Errorf("%q no longer deploys once the cap is below the count: %v %s", name, out, said)
		}
	}
	// A new name is still refused, because being over is not being exempt.
	if _, said, isErr := h.deployNamed(t, h.a, "reports"); !isErr {
		t.Errorf("a new app was accepted while the account is over its cap: %s", said)
	}
}

// TestTheCapIsPerAccountNotPerToken pins that one account's apps never move
// another's count, which is what a shared ceiling would break.
func TestTheCapIsPerAccountNotPerToken(t *testing.T) {
	// covers: AC-15
	h := newCappedHarness(t, 2)
	h.fill(t, h.a, "checkout", "billing")

	if _, said, isErr := h.deployNamed(t, h.a, "invoices"); !isErr {
		t.Fatalf("A was not at its ceiling: %s", said)
	}
	if out, said, isErr := h.deployNamed(t, h.b, "invoices"); isErr {
		t.Fatalf("B was refused for A's apps: %v %s", out, said)
	}
	if got := appCount(t, h.store, h.b.account.ID); got != 1 {
		t.Errorf("B holds %d apps, want 1", got)
	}
}

// TestTwoTokensOfOneAccountShareOneAllowance is the other half of per account:
// the ceiling belongs to the account, so adding a second agent adds no room. A
// cap counted per token would be no cap at all, because minting one is free.
func TestTwoTokensOfOneAccountShareOneAllowance(t *testing.T) {
	// covers: AC-15
	h := newCappedHarness(t, 2)
	second := h.anotherToken(t, h.a)

	// One app through each credential, which fills the account rather than
	// filling either token.
	h.fill(t, h.a, "checkout")
	h.fill(t, second, "billing")

	// The fresh token is at the ceiling it did nothing on its own to reach.
	_, said, isErr := h.deployNamed(t, second, "invoices")
	if !isErr {
		t.Fatalf("the second token deployed past the account's ceiling: %s", said)
	}
	if !strings.Contains(said, string(domain.ReasonAppLimitReached)) {
		t.Errorf("the second token's refusal reads %q, want %s", said, domain.ReasonAppLimitReached)
	}
	// And so is the first, because there is only ever one allowance.
	if _, said, isErr := h.deployNamed(t, h.a, "reports"); !isErr {
		t.Errorf("the first token was still accepted at the ceiling: %s", said)
	}
	if got := appCount(t, h.store, h.a.account.ID); got != 2 {
		t.Errorf("the account holds %d apps, want 2: a second token bought room", got)
	}
}

// TestAnAccountAtItsCapStillOperatesWhatItHas is the rest of AC-4. Redeploying
// is proved above; this is configure and roll back. An account that could not
// set a variable or go back a release until it deleted an app would be held
// hostage by a ceiling that is only ever about creating one.
func TestAnAccountAtItsCapStillOperatesWhatItHas(t *testing.T) {
	// covers: AC-4
	h := newCappedHarness(t, 2)
	h.fill(t, h.a, "checkout", "billing")

	out, said, isErr := h.call(t, h.a, "set_config", map[string]any{
		"name": "checkout",
		"config": map[string]any{
			"GREETING": map[string]any{"value": "hello", "secret": false},
		},
	})
	if isErr {
		t.Fatalf("set_config on a held app was refused at the cap: %v %s", out, said)
	}

	// The app has never been healthy, so there is no release to go back to and
	// rollback refuses release_unknown. That is the refusal it gives below the
	// cap too, which is the point: the ceiling is not what decided it.
	_, said, isErr = h.call(t, h.a, "rollback_app", map[string]any{
		"name": "checkout", "release_number": 1,
	})
	if !isErr {
		t.Fatalf("rollback onto a release that was never minted was accepted: %s", said)
	}
	if !strings.Contains(said, string(domain.ReasonReleaseUnknown)) {
		t.Errorf("rollback at the cap reads %q, want %s", said, domain.ReasonReleaseUnknown)
	}
	if strings.Contains(said, string(domain.ReasonAppLimitReached)) {
		t.Errorf("the cap refused a rollback, which it must never gate: %s", said)
	}
}
