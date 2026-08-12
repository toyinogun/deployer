package reconcile_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// A deploy of the same app while the loop is mid drive cancels the row under
// the drive. The drive has to notice and stop, rather than walk on into a
// transition the store will refuse.
func TestADriveStopsWhenItsDeploymentIsSuperseded(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	errs := captureErrorLogs(t)

	// The supersession lands while the loop is watching the build, which is the
	// window a real redeploy hits: the build is the long phase.
	superseded := false
	w.clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !superseded {
			superseded = true
			supersede(t, w)
		}
		name := action.(k8stesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}},
		}, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	// A redeploy is ordinary platform behaviour, so it reads as nothing at all in
	// the log. Errors here would page somebody for a supersession.
	if got := errs(); len(got) != 0 {
		t.Errorf("the drive logged %d errors, want none:\n  %s", len(got), strings.Join(got, "\n  "))
	}

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateCancelled) {
		t.Fatalf("state = %s, want cancelled", dep.State)
	}
	// The reason the caller reads stays the supersession. A drive that walked on
	// and failed would overwrite it, or try to and log that it could not.
	if dep.FailureReason == nil || *dep.FailureReason != string(domain.ReasonSuperseded) {
		t.Errorf("failure_reason = %v, want superseded", dep.FailureReason)
	}

	// No event beyond the cancel: the drive wrote nothing after losing the row.
	events, err := w.store.ListDeploymentEvents(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	want := []string{"queued", "building", "cancelled"}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i, state := range want {
		if events[i].ToState != state {
			t.Errorf("event %d = %s, want %s", i, events[i].ToState, state)
		}
	}
}

// The same race one phase later: the supersession lands while the app is coming
// up, so it is MarkHealthy rather than a plain transition that gets refused.
func TestADriveStopsWhenItIsSupersededWhileTheAppComesUp(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	errs := captureErrorLogs(t)
	w.buildEnds(batchv1.JobComplete)

	superseded := false
	w.clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !superseded {
			superseded = true
			supersede(t, w)
		}
		name := action.(k8stesting.GetAction).GetName()
		return true, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: appsv1.DeploymentStatus{
				UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1,
			},
		}, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	if got := errs(); len(got) != 0 {
		t.Errorf("the drive logged %d errors, want none:\n  %s", len(got), strings.Join(got, "\n  "))
	}

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateCancelled) {
		t.Fatalf("state = %s, want cancelled", dep.State)
	}
	// No release was minted for a deployment that never became healthy.
	if _, err := w.store.GetReleaseByDeployment(ctx, w.deployment.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the release returned %v, want ErrNotFound", err)
	}
}

// supersede creates a second deployment for the same app, which is what
// deploy_app does and what cancels the one in flight.
func supersede(t *testing.T, w *world) {
	t.Helper()
	ctx := context.Background()
	body := strings.NewReader("\x1f\x8b" + strings.Repeat("x", 64))
	up, err := w.uploads.Accept(ctx, w.deployment.AccountID, body)
	if err != nil {
		t.Fatalf("accepting the second upload: %v", err)
	}
	if _, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.deployment.AccountID, UploadID: &up.ID,
	}); err != nil {
		t.Fatalf("creating the superseding deployment: %v", err)
	}
}

// captureErrorLogs swaps the default logger for one that records error messages,
// and returns a reader for them. The logger is restored when the test ends.
func captureErrorLogs(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var msgs []string
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(recorder{fn: func(r slog.Record) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, r.Message)
	}}))
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), msgs...)
	}
}

// recorder is a slog handler that keeps error records and drops the rest.
type recorder struct{ fn func(slog.Record) }

func (h recorder) Enabled(context.Context, slog.Level) bool { return true }

func (h recorder) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.fn(r)
	}
	return nil
}

func (h recorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h recorder) WithGroup(string) slog.Handler { return h }
