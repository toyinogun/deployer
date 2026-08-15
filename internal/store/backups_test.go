package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// A second run is refused by the partial unique index rather than by a check in
// Go, and the refusal is its own sentinel so a caller can never read a real
// write fault as a benign one (spec 0020, AC-8, AC-8a).
func TestBackupRunOneInFlight(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	first, err := s.StartBackupRun(ctx, "")
	if err != nil {
		t.Fatalf("starting the first run: %v", err)
	}
	if first.Outcome != "running" || first.Trigger != "schedule" || first.TriggeredBy != nil {
		t.Fatalf("a scheduled run should be running with no triggered_by, got %+v", first)
	}

	if _, err := s.StartBackupRun(ctx, ""); !errors.Is(err, store.ErrBackupInFlight) {
		t.Fatalf("starting a second run: got %v, want ErrBackupInFlight", err)
	}

	// Ending the first one frees the index, so the next run inserts normally.
	if err := s.FinishBackupRunSucceeded(ctx, first.ID, store.BackupResult{
		ObjectKey: "db/20260811T120000Z-" + first.ID + ".age",
		SizeBytes: 4096,
		Checksum:  "abc",
	}); err != nil {
		t.Fatalf("ending the first run: %v", err)
	}
	if _, err := s.StartBackupRun(ctx, ""); err != nil {
		t.Fatalf("starting a run after the first ended: %v", err)
	}
}

// A terminal row is never rewritten. Whatever ended it wins, and the loser is
// told rather than silently ignored.
func TestBackupRunTerminalIsNeverRewritten(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	run, err := s.StartBackupRun(ctx, "")
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := s.FinishBackupRunFailed(ctx, run.ID, domain.BackupUploadFailed); err != nil {
		t.Fatalf("ending the run failed: %v", err)
	}
	if err := s.FinishBackupRunSucceeded(ctx, run.ID, store.BackupResult{ObjectKey: "k"}); !errors.Is(err, store.ErrTerminal) {
		t.Fatalf("ending an already ended run: got %v, want ErrTerminal", err)
	}

	rows, err := s.ListBackupRuns(ctx, 10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome != "failed" || *rows[0].FailureReason != "upload_failed" {
		t.Fatalf("the row should still be the failed one, got %+v", rows)
	}
}

// A code outside the closed set never reaches the row.
func TestBackupRunRejectsAnUnknownReason(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	run, err := s.StartBackupRun(ctx, "")
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	if err := s.FinishBackupRunFailed(ctx, run.ID, domain.BackupReason("disk_on_fire")); err == nil {
		t.Fatal("an unknown reason should be refused")
	}
}

// The startup sweep ends a row a dead predecessor left behind, and the next run
// then starts normally (AC-9).
func TestBackupRunStrandSweep(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	if _, err := s.StartBackupRun(ctx, ""); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	n, err := s.StrandBackupRuns(ctx)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep should have ended one row, ended %d", n)
	}
	rows, err := s.ListBackupRuns(ctx, 10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if *rows[0].FailureReason != "stranded" {
		t.Fatalf("the swept row should carry the stranded reason, got %+v", rows[0])
	}
	if _, err := s.StartBackupRun(ctx, ""); err != nil {
		t.Fatalf("starting a run after the sweep: %v", err)
	}
}

// A manual run names the admin who fired it; the reads the schedule and the
// alert path make answer with the rows they are meant to (AC-21, AC-12, AC-13).
func TestBackupRunTriggerAndReads(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	f := newFixture(t, s)

	if _, err := s.LatestSucceededBackupRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("on an empty table: got %v, want ErrNotFound", err)
	}
	if _, err := s.LatestTerminalBackupRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("on an empty table: got %v, want ErrNotFound", err)
	}

	manual, err := s.StartBackupRun(ctx, f.account.ID)
	if err != nil {
		t.Fatalf("starting a manual run: %v", err)
	}
	if manual.Trigger != "manual" || manual.TriggeredBy == nil || *manual.TriggeredBy != f.account.ID {
		t.Fatalf("a manual run should name its admin, got %+v", manual)
	}

	running, err := s.RunningBackupRun(ctx)
	if err != nil || running.ID != manual.ID {
		t.Fatalf("reading the run in flight: %v, %+v", err, running)
	}

	if err := s.FinishBackupRunFailed(ctx, manual.ID, domain.BackupVerifyFailed); err != nil {
		t.Fatalf("ending the manual run: %v", err)
	}
	if _, err := s.RunningBackupRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("with nothing in flight: got %v, want ErrNotFound", err)
	}
	// A failure is terminal but not a success, so only one of the two reads
	// answers with it.
	if _, err := s.LatestSucceededBackupRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after only a failure: got %v, want ErrNotFound", err)
	}
	terminal, err := s.LatestTerminalBackupRun(ctx)
	if err != nil || terminal.ID != manual.ID {
		t.Fatalf("reading the newest ended run: %v, %+v", err, terminal)
	}

	clock.Advance(time.Hour)
	next, err := s.StartBackupRun(ctx, "")
	if err != nil {
		t.Fatalf("starting the next run: %v", err)
	}
	if err := s.FinishBackupRunSucceeded(ctx, next.ID, store.BackupResult{
		ObjectKey: "db/k.age", SizeBytes: 10, Checksum: "c",
	}); err != nil {
		t.Fatalf("ending the next run: %v", err)
	}
	got, err := s.LatestSucceededBackupRun(ctx)
	if err != nil || got.ID != next.ID {
		t.Fatalf("reading the newest success: %v, %+v", err, got)
	}
}
