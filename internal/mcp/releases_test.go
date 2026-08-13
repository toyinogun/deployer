package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// releaseServer is a tool surface over one app that already has three releases,
// the second of which is current.
func releaseServer() (*Server, *silentAuditor, *stubDeployments, auth.Account) {
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{
		owner: account.ID,
		existing: map[string]App{"hello": {
			ID: "app_1", Slug: "hello-a1b2c3", Name: "hello", CurrentReleaseID: "rel_2",
		}},
	}
	deployments := &stubDeployments{summaries: []ReleaseSummary{
		{ID: "rel_3", Number: 3, Digest: "sha256:ccc", DeploymentID: "dep_3", CreatedAt: "2026-08-13T12:00:00Z"},
		{ID: "rel_2", Number: 2, Digest: "sha256:bbb", DeploymentID: "dep_2", CreatedAt: "2026-08-12T12:00:00Z"},
		{ID: "rel_1", Number: 1, Digest: "sha256:aaa", DeploymentID: "dep_1", CreatedAt: "2026-08-11T12:00:00Z"},
	}}
	s, auditor := server(apps, deployments, liveUpload(account.ID))
	return s, auditor, deployments, account
}

// TestTheListingMarksExactlyOneCurrentRelease checks the one flag a caller
// cannot compute for itself, and that nothing configuration shaped rides along.
func TestTheListingMarksExactlyOneCurrentRelease(t *testing.T) {
	// covers: AC-1, AC-2, AC-4
	s, _, _, account := releaseServer()

	_, out, err := s.listReleases(t.Context(), account, listReleasesInput{Name: "hello"})
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	if len(out.Releases) != 3 {
		t.Fatalf("the listing holds %d rows, want 3", len(out.Releases))
	}
	if out.Releases[0].ReleaseNumber != 3 {
		t.Errorf("the first row is release %d, want 3: newest first", out.Releases[0].ReleaseNumber)
	}
	var current []int64
	for _, r := range out.Releases {
		if r.Current {
			current = append(current, r.ReleaseNumber)
		}
	}
	if len(current) != 1 || current[0] != 2 {
		t.Errorf("the listing marks %v current, want exactly release 2", current)
	}
	for _, r := range out.Releases {
		if r.ImageDigest == "" || r.DeploymentID == "" || r.CreatedAt == "" {
			t.Errorf("a row came back incomplete: %+v", r)
		}
	}
}

// TestAnAppWithNoReleasesListsEmptyRatherThanRefusing pins the empty case as a
// success: an app that has never been healthy is not an error.
func TestAnAppWithNoReleasesListsEmptyRatherThanRefusing(t *testing.T) {
	// covers: AC-3
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	s, _ := server(apps, &stubDeployments{}, liveUpload(account.ID))

	_, out, err := s.listReleases(t.Context(), account, listReleasesInput{Name: "hello"})
	if err != nil {
		t.Fatalf("listing releases of an app that has never run: %v", err)
	}
	if out.Releases == nil {
		t.Error("the listing is null, want an empty list: an agent should not have to special case it")
	}
	if len(out.Releases) != 0 {
		t.Errorf("the listing holds %d rows, want none", len(out.Releases))
	}
}

// TestTheListingAsksForNoMoreThanTheBound checks that the bound is the tool's,
// applied at the query, rather than a slice the handler takes afterwards.
func TestTheListingAsksForNoMoreThanTheBound(t *testing.T) {
	// covers: AC-1
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	many := make([]ReleaseSummary, 0, 25)
	for i := 25; i >= 1; i-- {
		many = append(many, ReleaseSummary{
			ID:           fmt.Sprintf("rel_%d", i),
			Number:       int64(i),
			Digest:       "sha256:aaa",
			DeploymentID: fmt.Sprintf("dep_%d", i),
			CreatedAt:    "2026-08-13T12:00:00Z",
		})
	}
	s, _ := server(apps, &stubDeployments{summaries: many}, liveUpload(account.ID))

	_, out, err := s.listReleases(t.Context(), account, listReleasesInput{Name: "hello"})
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	if len(out.Releases) != MaxReleaseListing {
		t.Errorf("the listing returned %d rows, want the bound of %d", len(out.Releases), MaxReleaseListing)
	}
}

// TestARollbackIsRecordedAgainstTheReleaseTheCallerNamed checks the whole
// accepted path: the number resolves to an id, the row is written against it,
// and the response says queued without waiting.
func TestARollbackIsRecordedAgainstTheReleaseTheCallerNamed(t *testing.T) {
	// covers: AC-6, AC-9, AC-20
	s, auditor, deployments, account := releaseServer()

	_, out, err := s.rollback(t.Context(), account, rollbackInput{Name: "hello", ReleaseNumber: 1})
	if err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	if out.State != string(domain.StateQueued) {
		t.Errorf("the rollback reports state %q, want queued", out.State)
	}
	if out.DeploymentID == "" {
		t.Error("the rollback reports no deployment_id, so nothing can be polled")
	}
	if out.Name != "hello" || out.Slug != "hello-a1b2c3" {
		t.Errorf("the rollback reports %q/%q, want hello/hello-a1b2c3", out.Name, out.Slug)
	}
	if out.URL != "https://hello-a1b2c3.deploy.example.org" {
		t.Errorf("the rollback reports url %q", out.URL)
	}
	if deployments.lastRelease != "rel_1" {
		t.Errorf("the rollback was written against %q, want rel_1", deployments.lastRelease)
	}

	var allowed bool
	for _, row := range auditor.rows {
		if row.Action == auth.ActionRollback && row.Allowed && row.TargetID == "app_1" {
			allowed = true
		}
	}
	if !allowed {
		t.Errorf("no allowed rollback audit row was written: %+v", auditor.rows)
	}
}

// TestRollingBackToTheCurrentReleaseIsAllowed pins that re promoting what is
// already running is an ordinary rollback rather than a refusal.
func TestRollingBackToTheCurrentReleaseIsAllowed(t *testing.T) {
	// covers: AC-18
	s, _, deployments, account := releaseServer()

	_, out, err := s.rollback(t.Context(), account, rollbackInput{Name: "hello", ReleaseNumber: 2})
	if err != nil {
		t.Fatalf("rolling back to the current release: %v", err)
	}
	if out.State != string(domain.StateQueued) {
		t.Errorf("state = %q, want queued", out.State)
	}
	if deployments.lastRelease != "rel_2" {
		t.Errorf("the rollback was written against %q, want the current rel_2", deployments.lastRelease)
	}
}

// TestABadReleaseNumberIsRefusedAndWritesNothing covers both shapes of a bad
// number, the one that does not exist and the one that could never exist.
func TestABadReleaseNumberIsRefusedAndWritesNothing(t *testing.T) {
	// covers: AC-7, AC-20, AC-21
	for _, number := range []int64{99, 0, -1} {
		s, auditor, deployments, account := releaseServer()
		_, _, err := s.rollback(t.Context(), account, rollbackInput{Name: "hello", ReleaseNumber: number})
		if err == nil {
			t.Fatalf("release_number %d was accepted, want a refusal", number)
		}
		if !strings.HasPrefix(err.Error(), string(domain.ReasonReleaseUnknown)) {
			t.Errorf("release_number %d was refused with %q, want %s",
				number, err, domain.ReasonReleaseUnknown)
		}
		if deployments.created != 0 {
			t.Errorf("release_number %d wrote %d deployments, want none", number, deployments.created)
		}
		var denied bool
		for _, row := range auditor.rows {
			if row.Action == auth.ActionRollback && !row.Allowed &&
				row.Reason == string(domain.ReasonReleaseUnknown) {
				denied = true
			}
		}
		if !denied {
			t.Errorf("release_number %d left no denial in the audit log: %+v", number, auditor.rows)
		}
	}
}

// TestAnotherAccountsAppIsUnknownToBothTools checks that ownership is decided
// first, so neither tool can be used to learn what someone else has.
func TestAnotherAccountsAppIsUnknownToBothTools(t *testing.T) {
	// covers: AC-5, AC-8
	s, _, deployments, _ := releaseServer()
	// An account that owns nothing here, so "hello" resolves to nothing for it.
	stranger := auth.Account{ID: "acc_2", Name: "stranger"}

	_, _, listErr := s.listReleases(t.Context(), stranger, listReleasesInput{Name: "hello"})
	if listErr == nil {
		t.Fatal("list_releases answered about another account's app")
	}
	_, _, rollbackErr := s.rollback(t.Context(), stranger, rollbackInput{Name: "hello", ReleaseNumber: 1})
	if rollbackErr == nil {
		t.Fatal("rollback_app acted on another account's app")
	}
	// The name that never existed at all has to read identically.
	_, _, missingErr := s.listReleases(t.Context(), stranger, listReleasesInput{Name: "never-existed"})
	if missingErr == nil {
		t.Fatal("list_releases answered about an app that does not exist")
	}

	for _, err := range []error{listErr, rollbackErr, missingErr} {
		if !strings.HasPrefix(err.Error(), string(domain.ReasonAppUnknown)) {
			t.Errorf("the refusal reads %q, want %s", err, domain.ReasonAppUnknown)
		}
	}
	if listErr.Error() != missingErr.Error() {
		t.Errorf("another account's app reads %q and a missing one %q; the two must be "+
			"indistinguishable", listErr, missingErr)
	}
	if deployments.created != 0 {
		t.Errorf("a refused rollback wrote %d deployments", deployments.created)
	}
}

// TestRollbackAppRefusesOverTheWireWithTheReasonCode drives the refusal through
// a real client and server session. Calling the handler directly never crosses
// the argument schema, so a schema that refused the call first would hand the
// caller a validation string and still pass every test above.
func TestRollbackAppRefusesOverTheWireWithTheReasonCode(t *testing.T) {
	// covers: AC-23, AC-7, AC-21
	s, _, _, account := releaseServer()
	res := callOverTheWire(t, s, account, "rollback_app", map[string]any{
		"name":           "hello",
		"release_number": 99,
	})
	if !res.IsError {
		t.Fatal("the call was accepted, want a refusal")
	}
	if got := resultText(res); !strings.HasPrefix(got, string(domain.ReasonReleaseUnknown)) {
		t.Errorf("the refusal reads %q, want it to start with %s", got, domain.ReasonReleaseUnknown)
	}
}

// TestAnUnknownAppRefusesOverTheWireOnBothTools is the same check on the other
// refusal both tools share.
func TestAnUnknownAppRefusesOverTheWireOnBothTools(t *testing.T) {
	// covers: AC-23, AC-5, AC-8
	s, _, _, account := releaseServer()
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"list_releases", map[string]any{"name": "not-mine"}},
		{"rollback_app", map[string]any{"name": "not-mine", "release_number": 1}},
	}
	for _, c := range calls {
		res := callOverTheWire(t, s, account, c.tool, c.args)
		if !res.IsError {
			t.Errorf("%s accepted an unknown app", c.tool)
			continue
		}
		if got := resultText(res); !strings.HasPrefix(got, string(domain.ReasonAppUnknown)) {
			t.Errorf("%s refused with %q, want it to start with %s", c.tool, got, domain.ReasonAppUnknown)
		}
	}
}

// TestBothToolsPassTheSchemaOnAWellFormedCall is the other half: the schemas
// must not refuse an ordinary call before the handler runs.
func TestBothToolsPassTheSchemaOnAWellFormedCall(t *testing.T) {
	// covers: AC-23, AC-1, AC-6
	s, _, _, account := releaseServer()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"list_releases", map[string]any{"name": "hello"}},
		{"rollback_app", map[string]any{"name": "hello", "release_number": 1}},
	} {
		res := callOverTheWire(t, s, account, c.tool, c.args)
		if res.IsError {
			t.Errorf("%s was refused with %q, want it to pass", c.tool, resultText(res))
		}
	}
}

// TestBothDescriptionsCarryTheirContract checks the promises the descriptions
// are the only place a caller can read: the bound, that a rollback does not
// wait, that it replaces configuration too, and that it supersedes.
func TestBothDescriptionsCarryTheirContract(t *testing.T) {
	// covers: AC-22, AC-25
	// Line breaks are collapsed first: these descriptions are wrapped prose, and
	// a promise is no less present for falling across two lines.
	listing := strings.Join(strings.Fields(listReleasesDescription), " ")
	rolling := strings.Join(strings.Fields(rollbackDescription), " ")

	for _, want := range []string{"newest 20 releases", "no way to page"} {
		if !strings.Contains(listing, want) {
			t.Errorf("list_releases does not say %q, so the bound is undiscoverable", want)
		}
	}
	for _, want := range []string{"does not wait", "environment variables", "supersedes", "reverted"} {
		if !strings.Contains(rolling, want) {
			t.Errorf("rollback_app does not say %q, which a caller cannot find out by trying", want)
		}
	}
}
