package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// stubCluster records the namespace deletes a tool issued, and can fail one.
type stubCluster struct {
	deleted []string
	err     error
}

func (c *stubCluster) DeleteNamespace(_ context.Context, slug string) error {
	if c.err != nil {
		return c.err
	}
	c.deleted = append(c.deleted, slug)
	return nil
}

// lifecycleServer is a tool surface over one account holding two apps: one
// serving release 4 whose newest deploy failed, and one registered but never
// deployed.
func lifecycleServer() (*Server, *silentAuditor, *stubApps, *stubCluster, auth.Account) {
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{
		owner: account.ID,
		existing: map[string]App{
			"notes": {ID: "app_1", Slug: "notes-a1b2c3", Name: "notes", CurrentReleaseID: "rel_4"},
			"fresh": {ID: "app_2", Slug: "fresh-b2c3d4", Name: "fresh"},
		},
		summaries: []AppSummary{
			{
				Name: "notes", Slug: "notes-a1b2c3", CreatedAt: "2026-08-11T09:14:02Z",
				ServingRelease:       4,
				LastDeploymentID:     "dep_9",
				LastDeploymentState:  string(domain.StateFailed),
				LastDeploymentReason: string(domain.ReasonBuildFailed),
				LastDeployedAt:       "2026-08-13T18:02:44Z",
			},
			{Name: "fresh", Slug: "fresh-b2c3d4", CreatedAt: "2026-08-12T10:00:00Z"},
		},
	}
	cluster := &stubCluster{}
	s, auditor := server(apps, &stubDeployments{}, liveUpload(account.ID))
	s.cluster = cluster
	return s, auditor, apps, cluster, account
}

// TestTheListingReportsServingAndTheLastDeployAsTwoFacts is the case the design
// exists for: an app serving release 4 whose newest deploy failed is up, and the
// response has to say both things at once.
func TestTheListingReportsServingAndTheLastDeployAsTwoFacts(t *testing.T) {
	// covers: AC-1, AC-2, AC-3, AC-4, AC-5, AC-6
	s, _, _, _, account := lifecycleServer()

	_, out, err := s.listApps(t.Context(), account, listAppsInput{})
	if err != nil {
		t.Fatalf("listing apps: %v", err)
	}
	if len(out.Apps) != 2 {
		t.Fatalf("the listing holds %d rows, want 2", len(out.Apps))
	}

	notes := out.Apps[0]
	if notes.Serving == nil || notes.Serving.ReleaseNumber != 4 {
		t.Errorf("serving = %+v, want release 4: a failed deploy does not take the app down", notes.Serving)
	}
	if notes.LastDeployment == nil {
		t.Fatal("last_deployment is absent, want the failed deploy reported beside the serving release")
	}
	if notes.LastDeployment.State != string(domain.StateFailed) {
		t.Errorf("last_deployment.state = %q, want failed", notes.LastDeployment.State)
	}
	if notes.LastDeployment.Reason != string(domain.ReasonBuildFailed) {
		t.Errorf("last_deployment.reason = %q, want build_failed", notes.LastDeployment.Reason)
	}
	if notes.URL != "https://notes-a1b2c3.deploy.example.org" {
		t.Errorf("url = %q, want it composed from the slug the same way deploy_app does", notes.URL)
	}
	if notes.LastDeployedAt != "2026-08-13T18:02:44Z" {
		t.Errorf("last_deployed_at = %q", notes.LastDeployedAt)
	}

	// An app that has never been healthy and never been deployed carries neither
	// object, rather than a zero release number and an empty state (AC-3, AC-4).
	fresh := out.Apps[1]
	if fresh.Serving != nil {
		t.Errorf("serving = %+v on an app that has never been healthy, want it absent", fresh.Serving)
	}
	if fresh.LastDeployment != nil {
		t.Errorf("last_deployment = %+v on an app never deployed, want it absent", fresh.LastDeployment)
	}
	if fresh.LastDeployedAt != "" {
		t.Errorf("last_deployed_at = %q on an app never deployed, want it absent", fresh.LastDeployedAt)
	}
}

// TestAnInFlightDeployShowsAsTheLastDeployment pins that the newest deployment
// is reported whatever state it is in, with no reason on it.
func TestAnInFlightDeployShowsAsTheLastDeployment(t *testing.T) {
	// covers: AC-4
	s, _, apps, _, account := lifecycleServer()
	apps.summaries = []AppSummary{{
		Name: "notes", Slug: "notes-a1b2c3",
		LastDeploymentID:    "dep_10",
		LastDeploymentState: string(domain.StateBuilding),
	}}

	_, out, err := s.listApps(t.Context(), account, listAppsInput{})
	if err != nil {
		t.Fatalf("listing apps: %v", err)
	}
	last := out.Apps[0].LastDeployment
	if last == nil || last.State != string(domain.StateBuilding) {
		t.Fatalf("last_deployment = %+v, want the running deploy reported as building", last)
	}
	if last.Reason != "" {
		t.Errorf("reason = %q on a deploy that has not ended, want it absent", last.Reason)
	}
}

// TestAnAccountWithNoAppsListsEmptyRatherThanRefusing pins the empty case as a
// success, and as a list rather than null.
func TestAnAccountWithNoAppsListsEmptyRatherThanRefusing(t *testing.T) {
	// covers: AC-9
	s, auditor := server(&stubApps{}, &stubDeployments{}, liveUpload("acc_1"))

	_, out, err := s.listApps(t.Context(), auth.Account{ID: "acc_1"}, listAppsInput{})
	if err != nil {
		t.Fatalf("listing an empty account's apps: %v", err)
	}
	if out.Apps == nil {
		t.Error("the listing is null, want an empty list: an agent should not have to special case it")
	}
	if len(out.Apps) != 0 {
		t.Errorf("the listing holds %d rows, want none", len(out.Apps))
	}
	// A successful read is not an access decision, so it leaves no row (AC-11).
	if len(auditor.rows) != 0 {
		t.Errorf("a successful listing wrote %d audit rows, want none", len(auditor.rows))
	}
}

// TestTheListingIsScopedToTheCallersAccount pins that the account comes from the
// token, never from an argument, and that admin widens nothing.
func TestTheListingIsScopedToTheCallersAccount(t *testing.T) {
	// covers: AC-10
	s, _, _, _, _ := lifecycleServer()

	_, out, err := s.listApps(t.Context(), auth.Account{ID: "acc_2", IsAdmin: true}, listAppsInput{})
	if err != nil {
		t.Fatalf("listing a second account's apps: %v", err)
	}
	if len(out.Apps) != 0 {
		t.Errorf("an admin from another account saw %d apps, want none: no MCP tool widens on the flag", len(out.Apps))
	}
}

// TestTheAppListingAsksForNoMoreThanTheBound checks the bound is the tool's, passed
// to the query rather than applied to what came back.
func TestTheAppListingAsksForNoMoreThanTheBound(t *testing.T) {
	// covers: AC-1
	s, _, apps, _, account := lifecycleServer()
	many := make([]AppSummary, 0, MaxAppListing+10)
	for range MaxAppListing + 10 {
		many = append(many, AppSummary{Name: "notes", Slug: "notes-a1b2c3"})
	}
	apps.summaries = many

	_, out, err := s.listApps(t.Context(), account, listAppsInput{})
	if err != nil {
		t.Fatalf("listing apps: %v", err)
	}
	if len(out.Apps) != MaxAppListing {
		t.Errorf("the listing returned %d rows, want the bound of %d", len(out.Apps), MaxAppListing)
	}
}

// TestADeleteRetiresTheRowThenTheNamespace is the whole accepted path, including
// the order the two writes happen in.
func TestADeleteRetiresTheRowThenTheNamespace(t *testing.T) {
	// covers: AC-13, AC-14, AC-16, AC-29
	s, auditor, apps, cluster, account := lifecycleServer()

	_, out, err := s.deleteApp(t.Context(), account, deleteAppInput{Name: "notes"})
	if err != nil {
		t.Fatalf("deleting the app: %v", err)
	}
	if out.Name != "notes" || out.Slug != "notes-a1b2c3" || !out.Deleted {
		t.Errorf("output = %+v", out)
	}
	if len(apps.deleted) != 1 || apps.deleted[0] != "app_1" {
		t.Errorf("retired %v, want the app's row soft deleted first", apps.deleted)
	}
	if len(cluster.deleted) != 1 || cluster.deleted[0] != "notes-a1b2c3" {
		t.Errorf("deleted namespaces for %v, want one call for the app's slug", cluster.deleted)
	}
	if len(auditor.rows) != 1 || !auditor.rows[0].Allowed ||
		auditor.rows[0].Action != auth.ActionAppDelete || auditor.rows[0].TargetID != "app_1" {
		t.Errorf("audit rows = %+v, want one allowed app_delete against the app", auditor.rows)
	}
}

// TestADeployInFlightRefusesTheWholeDelete pins that the refusal writes nothing
// and tears nothing down.
func TestADeployInFlightRefusesTheWholeDelete(t *testing.T) {
	// covers: AC-15, AC-28, AC-29
	s, auditor, apps, cluster, account := lifecycleServer()
	apps.inFlight = true

	_, _, err := s.deleteApp(t.Context(), account, deleteAppInput{Name: "notes"})
	if err == nil {
		t.Fatal("the delete was accepted, want it refused while a deployment is in flight")
	}
	if !strings.HasPrefix(err.Error(), string(domain.ReasonDeploymentInFlight)) {
		t.Errorf("the refusal reads %q, want it to start with %s", err, domain.ReasonDeploymentInFlight)
	}
	if len(apps.deleted) != 0 {
		t.Errorf("retired %v, want nothing written", apps.deleted)
	}
	if len(cluster.deleted) != 0 {
		t.Errorf("deleted %v, want nothing torn down", cluster.deleted)
	}
	if len(auditor.rows) != 1 || auditor.rows[0].Allowed ||
		auditor.rows[0].Reason != string(domain.ReasonDeploymentInFlight) || auditor.rows[0].TargetID != "app_1" {
		t.Errorf("audit rows = %+v, want one denied row carrying the code and the app", auditor.rows)
	}
}

// TestANamespaceDeleteThatFailsStillLeavesTheRowDeleted pins the asymmetry the
// spec chose: the caller is told internal, and the reaper finishes the job.
func TestANamespaceDeleteThatFailsStillLeavesTheRowDeleted(t *testing.T) {
	// covers: AC-19
	s, _, apps, cluster, account := lifecycleServer()
	cluster.err = errors.New("the API server said no")

	_, _, err := s.deleteApp(t.Context(), account, deleteAppInput{Name: "notes"})
	if err == nil {
		t.Fatal("the delete reported success, want internal after the namespace delete failed")
	}
	if !strings.HasPrefix(err.Error(), string(domain.ReasonInternal)) {
		t.Errorf("the refusal reads %q, want it to start with internal", err)
	}
	if strings.Contains(err.Error(), "API server") {
		t.Errorf("the cluster error reached the caller: %q", err)
	}
	if len(apps.deleted) != 1 {
		t.Errorf("retired %v, want the row deleted regardless", apps.deleted)
	}
}

// TestEveryUnknownAppRefusalReadsTheSame pins that a name that never existed,
// another account's app, and an already deleted one are indistinguishable.
func TestEveryUnknownAppRefusalReadsTheSame(t *testing.T) {
	// covers: AC-20
	s, _, apps, _, account := lifecycleServer()

	_, _, missing := s.deleteApp(t.Context(), account, deleteAppInput{Name: "nothing-here"})
	_, _, stranger := s.deleteApp(t.Context(), auth.Account{ID: "acc_2"}, deleteAppInput{Name: "notes"})
	// The second of two concurrent deletes updates no row, which the store
	// answers as a missing app.
	apps.deleteErr = ErrNoApp
	_, _, raced := s.deleteApp(t.Context(), account, deleteAppInput{Name: "notes"})

	for name, err := range map[string]error{"missing": missing, "stranger": stranger, "raced": raced} {
		if err == nil {
			t.Fatalf("the %s delete was accepted, want app_unknown", name)
		}
		if err.Error() != missing.Error() {
			t.Errorf("the %s refusal reads %q, want it word for word the same as %q", name, err, missing)
		}
	}
}

// TestListAppsAndDeleteAppOverTheWire drives both tools through a real client and
// server session, so a refusal the argument schema would catch first cannot pass
// as a reason code.
func TestListAppsAndDeleteAppOverTheWire(t *testing.T) {
	// covers: AC-31
	s, _, apps, _, account := lifecycleServer()

	listed := callOverTheWire(t, s, account, "list_apps", map[string]any{})
	if listed.IsError {
		t.Fatalf("list_apps was refused with %q, want it to pass with no arguments", resultText(listed))
	}
	if body := resultText(listed); !strings.Contains(body, "notes-a1b2c3") {
		t.Errorf("the listing reads %q, want the app in it", body)
	}

	deleted := callOverTheWire(t, s, account, "delete_app", map[string]any{"name": "notes"})
	if deleted.IsError {
		t.Fatalf("delete_app was refused with %q, want it to pass", resultText(deleted))
	}

	apps.inFlight = true
	refused := callOverTheWire(t, s, account, "delete_app", map[string]any{"name": "notes"})
	if !refused.IsError {
		t.Fatal("the delete was accepted over the wire, want deployment_in_flight")
	}
	if got := resultText(refused); !strings.HasPrefix(got, string(domain.ReasonDeploymentInFlight)) {
		t.Errorf("the refusal reads %q, want the reason code rather than a validation string", got)
	}

	unknown := callOverTheWire(t, s, account, "delete_app", map[string]any{"name": "nothing-here"})
	if got := resultText(unknown); !refused.IsError || !strings.HasPrefix(got, string(domain.ReasonAppUnknown)) {
		t.Errorf("the refusal reads %q, want app_unknown", got)
	}
}

// TestTheDescriptionsCarryTheirContract pins the parts of both descriptions that
// are contract rather than prose: what a caller cannot discover by trying.
func TestTheDescriptionsCarryTheirContract(t *testing.T) {
	// covers: AC-12, AC-30
	for _, want := range []string{"newest 50", "no way to page past"} {
		if !strings.Contains(listAppsDescription, want) {
			t.Errorf("list_apps's description does not mention %q", want)
		}
	}
	for _, want := range []string{"cannot be undone", "never reused", "does not wait", "deployment_in_flight", "kept rather than purged"} {
		if !strings.Contains(deleteAppDescription, want) {
			t.Errorf("delete_app's description does not mention %q", want)
		}
	}
}
