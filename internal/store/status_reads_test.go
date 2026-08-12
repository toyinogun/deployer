package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/mcp"
	"github.com/toyinogun/deployer/internal/store"
)

// secondApp registers another app under the same account, with an upload of its
// own, because one upload backs one deployment.
func secondApp(t *testing.T, s *store.Store, f fixture, name, uploadHash string) (store.App, store.Upload) {
	t.Helper()
	app, err := s.CreateApp(t.Context(), f.account.ID, name)
	if err != nil {
		t.Fatalf("creating app %q: %v", name, err)
	}
	return app, newUpload(t, s, f.account.ID, uploadHash)
}

// TestTheStatusProjectionDropsEventDetail checks the leak boundary at the place
// it is enforced: detail is written on the event row and dropped when the row is
// projected for a tool response, so no write site can leak a cluster message
// through the timeline. Verifies spec 0005, AC-8.
func TestTheStatusProjectionDropsEventDetail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	// The kind of raw cluster text the loop writes into detail when it fails.
	const leak = "exec /cnb/lifecycle/builder: no such file or directory"
	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	if _, err := s.Transition(ctx, dep.ID, domain.StateBuilding, "", leak); err != nil {
		t.Fatalf("moving the deployment to building: %v", err)
	}

	events, err := store.ForMCPDeployments(s).Events(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the timeline: %v", err)
	}

	// The row is there, in occurred_at order, carrying only what AC-8 allows.
	if len(events) != 2 {
		t.Fatalf("got %d timeline entries, want queued then building", len(events))
	}
	if events[0].State != domain.StateQueued || events[1].State != domain.StateBuilding {
		t.Errorf("timeline = %+v, want queued then building", events)
	}
	if events[1].At == "" {
		t.Error("the building entry carries no timestamp")
	}
	// Nothing of the detail reaches any field of any entry, including ones a
	// later field might be added to.
	if rendered := fmt.Sprintf("%+v", events); strings.Contains(rendered, "cnb") {
		t.Errorf("the detail crossed into the timeline: %s", rendered)
	}

	// The row itself still holds it, so this is a projection boundary and not the
	// loop having stopped recording anything.
	rows, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event rows: %v", err)
	}
	if last := rows[len(rows)-1]; last.Detail == nil || *last.Detail != leak {
		t.Errorf("the event row detail is %v, want it kept internally", last.Detail)
	}
}

// TestTheLatestDeploymentIsTheAppsOwn checks that a status read by name reports
// the named app's most recent deployment and never a neighbour's, which is the
// read half of the by name path. Verifies spec 0005, AC-6.
func TestTheLatestDeploymentIsTheAppsOwn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	deployments := store.ForMCPDeployments(s)

	mine := mustCreateDeployment(t, s, f, f.upload.ID)
	other, otherUpload := secondApp(t, s, f, "Billing", "hash-billing")
	theirs, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID:     other.ID,
		AccountID: f.account.ID,
		UploadID:  &otherUpload.ID,
	})
	if err != nil {
		t.Fatalf("creating the other app's deployment: %v", err)
	}

	for name, want := range map[string]struct{ appID, deploymentID string }{
		"the first app":  {f.app.ID, mine.ID},
		"the second app": {other.ID, theirs.ID},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := deployments.LatestForApp(ctx, want.appID)
			if err != nil {
				t.Fatalf("reading the latest deployment: %v", err)
			}
			if got.ID != want.deploymentID {
				t.Errorf("latest = %s, want %s", got.ID, want.deploymentID)
			}
		})
	}

	// An app nobody has deployed reads as unknown rather than as another app's
	// row or as a fault.
	empty, _ := secondApp(t, s, f, "Never Deployed", "hash-never")
	if _, err := deployments.LatestForApp(ctx, empty.ID); !errors.Is(err, mcp.ErrNoDeployment) {
		t.Errorf("error = %v, want %v", err, mcp.ErrNoDeployment)
	}
}

// TestSupersededByIsOrderedByIdNotTimestamp checks the derivation with every row
// sharing one created_at, which is the case ordering by timestamp would get
// wrong. The clock is fixed, so the ties are real. Verifies spec 0005, AC-13.
func TestSupersededByIsOrderedByIdNotTimestamp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	deployments := store.ForMCPDeployments(s)

	// Three deploys of one app in a row: each supersedes the one before it.
	first := mustCreateDeployment(t, s, f, f.upload.ID)
	second := mustCreateDeployment(t, s, f, newUpload(t, s, f.account.ID, "hash-2").ID)
	third := mustCreateDeployment(t, s, f, newUpload(t, s, f.account.ID, "hash-3").ID)

	// The premise: nothing here is separable by time.
	if first.CreatedAt != second.CreatedAt || second.CreatedAt != third.CreatedAt {
		t.Fatalf("the rows do not share a created_at, so this proves nothing: %s/%s/%s",
			first.CreatedAt, second.CreatedAt, third.CreatedAt)
	}

	for name, want := range map[string]struct{ after, next string }{
		"the first points at the second": {first.ID, second.ID},
		"the second points at the third": {second.ID, third.ID},
		"the last points at nothing yet": {third.ID, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := deployments.NextForApp(ctx, f.app.ID, want.after)
			if err != nil {
				t.Fatalf("reading the next deployment: %v", err)
			}
			if got != want.next {
				t.Errorf("next = %q, want %q", got, want.next)
			}
		})
	}
}
