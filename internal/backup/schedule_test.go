package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/backup"
)

// runSchedule starts the schedule, gives the startup catch up time to decide,
// then stops it and returns. The schedule owns its own timer, so the only way to
// observe the decision is to let it make one.
func runSchedule(t *testing.T, svc *backup.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		svc.Schedule(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Schedule did not return after its context was cancelled")
	}
}

// objectCount is how many objects are sitting in the fake bucket.
func (h *harness) objectCount() int {
	h.bucket.mu.Lock()
	defer h.bucket.mu.Unlock()
	return len(h.bucket.objects)
}

// covers: AC-12 - a platform that has never been backed up is as overdue as one
// can be, so the schedule takes one immediately rather than waiting out a full
// interval first.
func TestSchedule_backsUpImmediatelyWhenNothingHasEverSucceeded(t *testing.T) {
	h := newHarness(t, 1)

	runSchedule(t, h.svc)

	if got := h.objectCount(); got != 1 {
		t.Fatalf("the startup catch up should have taken one backup, %d objects landed", got)
	}
}

// covers: AC-12 - the interval is measured from the end of the last run, so a
// pod that restarts often does not back up on every restart. This is the half of
// the catch up that is easy to lose: firing on start is trivial, not firing is
// the part that keeps a crash looping pod from filling the bucket.
func TestSchedule_doesNotBackUpAgainWhenTheLastSuccessIsRecent(t *testing.T) {
	h := newHarness(t, 1)
	// The harness clock is pinned, so this success is zero seconds old against
	// the one hour interval.
	if reason, err := h.svc.Run(t.Context(), ""); reason != "" || err != nil {
		t.Fatalf("seeding a recent success: reason %q, err %v", reason, err)
	}

	runSchedule(t, h.svc)

	if got := h.objectCount(); got != 1 {
		t.Fatalf("a recent success means not due, so the bucket should still hold 1 object, holds %d", got)
	}
}

// failingRuns delegates every call to the real in memory record and fails one
// named read. It invents no store behaviour: it is the passthrough that reaches
// a fault inside the platform, which is the only way to see what the schedule
// does when the database cannot answer.
type failingRuns struct {
	*memRuns
	latestErr  error
	strandErr  error
	strandRead bool
}

func (f *failingRuns) LatestSucceededBackupRun(ctx context.Context) (backup.Run, error) {
	if f.latestErr != nil {
		return backup.Run{}, f.latestErr
	}
	return f.memRuns.LatestSucceededBackupRun(ctx)
}

func (f *failingRuns) StrandBackupRuns(ctx context.Context) (int64, error) {
	f.strandRead = true
	if f.strandErr != nil {
		return 0, f.strandErr
	}
	return f.memRuns.StrandBackupRuns(ctx)
}

// withRuns builds a service over the given record, reusing the harness for
// everything else so the only difference is the fault under test.
func withRuns(t *testing.T, h *harness, runs backup.Runs) *backup.Service {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC) }
	svc := backup.New(runs, h.bucket, h.alerter, backup.Options{
		DB:        liveDB(t, 1),
		Recipient: h.identity.Recipient(),
		TempDir:   h.tempDir,
		Interval:  time.Hour,
		Now:       now,
	})
	if svc == nil {
		t.Fatal("the service should be configured")
	}
	return svc
}

// covers: AC-12 - a read that cannot be answered is not the same as being
// overdue. Taking a backup of a database that just failed a read is not the
// move, so the schedule waits out an interval instead.
func TestSchedule_doesNotBackUpWhenTheRecordCannotBeRead(t *testing.T) {
	h := newHarness(t, 1)
	runs := &failingRuns{memRuns: h.runs, latestErr: errors.New("database is locked")}

	runSchedule(t, withRuns(t, h, runs))

	if got := h.objectCount(); got != 0 {
		t.Fatalf("an unreadable record should take no backup, %d objects landed", got)
	}
}

// covers: AC-2a, AC-9 - neither half of the sweep stops the platform starting. A
// record that will not answer is logged and the process carries on, because a
// backup that cannot run is worth far less than a platform that will not boot.
func TestSweep_survivesARecordThatWillNotAnswer(t *testing.T) {
	h := newHarness(t, 1)
	runs := &failingRuns{memRuns: h.runs, strandErr: errors.New("database is locked")}

	backup.Sweep(t.Context(), runs, h.tempDir)

	if !runs.strandRead {
		t.Fatal("the sweep should still have tried to end stranded runs")
	}
}

// covers: AC-2a - a temporary directory that cannot be emptied is logged, and
// the stranded rows are still ended. Off cluster, where /data is not writable,
// this is the ordinary case rather than the exotic one.
func TestSweep_endsStrandedRunsEvenWhenTheDirectoryCannotBeEmptied(t *testing.T) {
	h := newHarness(t, 1)
	// A file where the directory should be: MkdirAll fails on it the same way an
	// unwritable mount does.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("in the way"), 0o600); err != nil {
		t.Fatalf("placing a file where the directory goes: %v", err)
	}
	if _, err := h.runs.StartBackupRun(t.Context(), ""); err != nil {
		t.Fatalf("leaving a running row behind: %v", err)
	}

	backup.Sweep(t.Context(), h.runs, blocked)

	if _, err := h.runs.RunningBackupRun(t.Context()); !errors.Is(err, backup.ErrNoRun) {
		t.Fatalf("the stranded row should still have been ended, got %v", err)
	}
}

// EmptyTempDir creates the directory when it is not there, which is what makes
// the startup sweep safe on a volume that has never held a backup.
func TestEmptyTempDir_createsTheDirectoryAtTheModeItNeeds(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "backup-tmp")

	if err := backup.EmptyTempDir(dir); err != nil {
		t.Fatalf("EmptyTempDir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat of the temp directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("EmptyTempDir should have made a directory")
	}
	// 0700 is what bounds the sub second window where SQLite's rollback journal
	// sits beside the snapshot at a mode this package cannot reach.
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("the temp directory is %#o, want %#o", got, 0o700)
	}
}

// covers: AC-2a - a hard kill runs no exit path, so whatever it left behind is a
// full unencrypted copy of the database. Everything goes, including a
// subdirectory, which os.Remove alone would refuse.
func TestEmptyTempDir_removesEverythingAKilledRunLeftBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bkp_dead.db"), []byte("every password hash"), 0o600); err != nil {
		t.Fatalf("leaving a snapshot behind: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatalf("leaving a directory behind: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "deeper.db"), []byte("more of it"), 0o600); err != nil {
		t.Fatalf("leaving a nested file behind: %v", err)
	}

	if err := backup.EmptyTempDir(dir); err != nil {
		t.Fatalf("EmptyTempDir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the temp directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the directory should be empty, holds %d entries", len(entries))
	}
}

// A directory that cannot be created is an error the caller decides about, not a
// panic and not a silent success.
func TestEmptyTempDir_errorsWhenTheDirectoryCannotBeMade(t *testing.T) {
	t.Parallel()

	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatalf("placing a file where the directory goes: %v", err)
	}

	if err := backup.EmptyTempDir(blocked); err == nil {
		t.Fatal("want an error when the path is not a directory")
	}
}
