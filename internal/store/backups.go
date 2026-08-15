package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// BackupRun is one attempt to snapshot the platform's own database, encrypt it,
// and put it somewhere that is not this cluster.
type BackupRun = sqlcgen.BackupRun

// The two triggers a run can carry. A scheduled run leaves triggered_by null; a
// manual one names the admin who pressed the button (spec 0020, AC-21).
const (
	BackupTriggerSchedule = "schedule"
	BackupTriggerManual   = "manual"
)

// StartBackupRun inserts the running row a backup begins with. The state write
// lands before the action it describes, per the project rule (AC-7).
//
// accountID is the admin who fired it by hand; empty means the schedule fired
// it, which is what decides both the trigger and whether triggered_by is set.
//
// A run already in flight comes back as ErrBackupInFlight, and only ever from
// the partial unique index refusing the insert. Every other write fault is
// wrapped and stays a fault: a full volume or a locked database reported as a
// benign concurrency refusal is how this would lie about its own health
// (AC-8, AC-8a).
func (s *Store) StartBackupRun(ctx context.Context, accountID string) (BackupRun, error) {
	trigger := BackupTriggerSchedule
	var by *string
	if accountID != "" {
		trigger = BackupTriggerManual
		by = ptr(accountID)
	}

	now := s.clock.Now()
	row, err := s.q.InsertBackupRun(ctx, sqlcgen.InsertBackupRunParams{
		ID:          ids.New(ids.BackupRun, now),
		StartedAt:   ids.Stamp(now),
		Trigger:     trigger,
		TriggeredBy: by,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return BackupRun{}, ErrBackupInFlight
		}
		return BackupRun{}, fmt.Errorf("store: starting a backup run: %w", err)
	}
	return row, nil
}

// BackupResult is what a run that reached the bucket has to record.
type BackupResult struct {
	ObjectKey string
	SizeBytes int64
	Checksum  string
}

// FinishBackupRunSucceeded closes a running row with what actually landed. A row
// something else already ended is left exactly as it is and comes back as
// ErrTerminal, matching how a drive that finds its deployment row terminal stops
// quietly rather than writing over it.
func (s *Store) FinishBackupRunSucceeded(ctx context.Context, id string, res BackupResult) error {
	n, err := s.q.FinishBackupRunSucceeded(ctx, sqlcgen.FinishBackupRunSucceededParams{
		ID:         id,
		FinishedAt: ptr(s.now()),
		ObjectKey:  ptr(res.ObjectKey),
		SizeBytes:  &res.SizeBytes,
		Checksum:   ptr(res.Checksum),
	})
	if err != nil {
		return fmt.Errorf("store: ending backup run %s succeeded: %w", id, err)
	}
	if n == 0 {
		return ErrTerminal
	}
	return nil
}

// FinishBackupRunFailed closes a running row with one of the closed codes. The
// wrapped error behind the failure is the caller's to log; it never reaches the
// row (AC-10).
func (s *Store) FinishBackupRunFailed(ctx context.Context, id string, reason domain.BackupReason) error {
	if !reason.Valid() {
		return fmt.Errorf("store: %q is not a backup failure reason", reason)
	}
	n, err := s.q.FinishBackupRunFailed(ctx, sqlcgen.FinishBackupRunFailedParams{
		ID:            id,
		FinishedAt:    ptr(s.now()),
		FailureReason: ptr(reason.String()),
	})
	if err != nil {
		return fmt.Errorf("store: ending backup run %s failed: %w", id, err)
	}
	if n == 0 {
		return ErrTerminal
	}
	return nil
}

// RunningBackupRun returns the run in flight, or ErrNotFound when there is none.
// The ticker reads this before it attempts an insert, so a scheduled tick skips
// its turn rather than vanishing into a swallowed constraint error (AC-12a).
func (s *Store) RunningBackupRun(ctx context.Context) (BackupRun, error) {
	row, err := s.q.GetRunningBackupRun(ctx)
	return backupRunOrNotFound(row, err, "reading the running backup run")
}

// LatestSucceededBackupRun returns the newest run that succeeded, or ErrNotFound
// when the platform has never taken one. It is what the startup catch up
// compares against the interval (AC-12).
func (s *Store) LatestSucceededBackupRun(ctx context.Context) (BackupRun, error) {
	row, err := s.q.LatestSucceededBackupRun(ctx)
	return backupRunOrNotFound(row, err, "reading the newest successful backup run")
}

// LatestTerminalBackupRun returns the newest run that ended either way, or
// ErrNotFound on a platform that has never finished one. Read before the current
// row is written, it is what decides whether a success is a recovery, and its
// absence is why the very first run the platform ever takes is never one (AC-13).
func (s *Store) LatestTerminalBackupRun(ctx context.Context) (BackupRun, error) {
	row, err := s.q.LatestTerminalBackupRun(ctx)
	return backupRunOrNotFound(row, err, "reading the newest ended backup run")
}

// ListBackupRuns returns the most recent runs, newest first, for the admin page.
func (s *Store) ListBackupRuns(ctx context.Context, limit int64) ([]BackupRun, error) {
	rows, err := s.q.ListBackupRuns(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing backup runs: %w", err)
	}
	return rows, nil
}

// StrandBackupRuns ends every row still running, and returns how many it ended.
// It runs at startup, before the process serves anything: one replica and a
// Recreate strategy make such a row definitionally dead, so there is no sweeper,
// no grace period, and no timer (AC-9).
func (s *Store) StrandBackupRuns(ctx context.Context) (int64, error) {
	n, err := s.q.StrandRunningBackupRuns(ctx, sqlcgen.StrandRunningBackupRunsParams{
		FinishedAt:    ptr(s.now()),
		FailureReason: ptr(domain.BackupStranded.String()),
	})
	if err != nil {
		return 0, fmt.Errorf("store: ending stranded backup runs: %w", err)
	}
	return n, nil
}

// backupRunOrNotFound turns the empty result every single row read here can
// return into ErrNotFound, and wraps anything else.
func backupRunOrNotFound(row BackupRun, err error, what string) (BackupRun, error) {
	switch {
	case err == nil:
		return row, nil
	case errors.Is(err, sql.ErrNoRows):
		return BackupRun{}, ErrNotFound
	default:
		return BackupRun{}, fmt.Errorf("store: %s: %w", what, err)
	}
}
