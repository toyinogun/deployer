package store_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// TestTransitionUnderConcurrentWriters is the regression test for a deployment
// left non terminal by a lost write. Transition has to decide from the row it is
// moving, so it reads and then writes inside one transaction. In WAL journalling
// a deferred transaction that has already read cannot take the write lock once
// another connection has written: SQLite answers SQLITE_BUSY straight away and
// never calls the busy handler, because waiting would deadlock the writer that
// needs this reader's snapshot released. The busy timeout is no help at all, so
// under a burst of deploys the transition simply failed, and the deployment sat
// in building with its app undeletable until the process restarted.
func TestTransitionUnderConcurrentWriters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	// One app each: a second deployment on the same app supersedes the first,
	// and a superseded row is terminal, which is a different story than this one.
	const writers = 24
	deps := make([]string, 0, writers)
	for i := range writers {
		app, err := s.CreateApp(ctx, f.account.ID, fmt.Sprintf("busy app %d", i))
		if err != nil {
			t.Fatalf("creating the app: %v", err)
		}
		up := newUpload(t, s, f.account.ID, fmt.Sprintf("busy-hash-%d", i))
		dep, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
			AppID:     app.ID,
			AccountID: f.account.ID,
			UploadID:  &up.ID,
		})
		if err != nil {
			t.Fatalf("creating the deployment: %v", err)
		}
		deps = append(deps, dep.ID)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for _, id := range deps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Transition(ctx, id, domain.StateBuilding, "", ""); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("a transition lost its write to a lock it could never wait for: %v", err)
		}
		t.Fatalf("transition failed: %v", err)
	}

	// Every row has to have actually moved. A transition that reports no error
	// but leaves the row behind is the same bug wearing a quieter face.
	for _, id := range deps {
		dep, err := s.GetDeployment(ctx, id)
		if err != nil {
			t.Fatalf("reading %s back: %v", id, err)
		}
		if domain.State(dep.State) != domain.StateBuilding {
			t.Fatalf("deployment %s stayed in %s", id, dep.State)
		}
	}
}
