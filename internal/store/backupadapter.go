package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/toyinogun/deployer/internal/backup"
	"github.com/toyinogun/deployer/internal/domain"
)

// BackupStore adapts the store to the narrow interface internal/backup declares,
// so that package holds the rules without depending on this one.
type BackupStore struct{ s *Store }

// ForBackup returns the backup facing view of the store.
func ForBackup(s *Store) BackupStore { return BackupStore{s: s} }

// Compile time proof that the adapter is what internal/backup asked for.
var _ backup.Runs = BackupStore{}

// StartBackupRun inserts the running row. The store's concurrency refusal is
// translated here into the one the backup package branches on, and nothing else
// is: a real write fault stays a wrapped error (spec 0020, AC-8a).
func (a BackupStore) StartBackupRun(ctx context.Context, accountID string) (backup.Run, error) {
	row, err := a.s.StartBackupRun(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrBackupInFlight) {
			return backup.Run{}, backup.ErrInFlight
		}
		return backup.Run{}, err
	}
	return toBackupRun(row)
}

// FinishBackupRunSucceeded closes a running row with what landed in the bucket.
func (a BackupStore) FinishBackupRunSucceeded(ctx context.Context, id string, res backup.Result) error {
	return a.s.FinishBackupRunSucceeded(ctx, id, BackupResult{
		ObjectKey: res.ObjectKey, SizeBytes: res.SizeBytes, Checksum: res.Checksum,
	})
}

// FinishBackupRunFailed closes a running row with one of the closed codes.
func (a BackupStore) FinishBackupRunFailed(ctx context.Context, id string, reason domain.BackupReason) error {
	return a.s.FinishBackupRunFailed(ctx, id, reason)
}

// RunningBackupRun reads the run in flight, which is what a scheduled tick
// checks before it attempts anything (AC-12a).
func (a BackupStore) RunningBackupRun(ctx context.Context) (backup.Run, error) {
	return a.readRun(a.s.RunningBackupRun(ctx))
}

// LatestSucceededBackupRun reads the newest success, which the startup catch up
// compares against the interval (AC-12).
func (a BackupStore) LatestSucceededBackupRun(ctx context.Context) (backup.Run, error) {
	return a.readRun(a.s.LatestSucceededBackupRun(ctx))
}

// LatestTerminalBackupRun reads the newest run that ended either way, which is
// what decides whether a success is a recovery (AC-13).
func (a BackupStore) LatestTerminalBackupRun(ctx context.Context) (backup.Run, error) {
	return a.readRun(a.s.LatestTerminalBackupRun(ctx))
}

// StrandBackupRuns ends every row a dead predecessor left running (AC-9).
func (a BackupStore) StrandBackupRuns(ctx context.Context) (int64, error) {
	return a.s.StrandBackupRuns(ctx)
}

// readRun turns the absent row every single row read can return into
// backup.ErrNoRun, so the caller branches on one sentinel rather than two.
func (a BackupStore) readRun(row BackupRun, err error) (backup.Run, error) {
	switch {
	case err == nil:
		return toBackupRun(row)
	case errors.Is(err, ErrNotFound):
		return backup.Run{}, backup.ErrNoRun
	default:
		return backup.Run{}, err
	}
}

// toBackupRun parses the stored timestamps back into times. A row whose stamps
// do not parse is a fault rather than a run the schedule silently treats as
// ancient.
func toBackupRun(row BackupRun) (backup.Run, error) {
	started, err := time.Parse(time.RFC3339Nano, row.StartedAt)
	if err != nil {
		return backup.Run{}, fmt.Errorf("store: backup run %s has an unreadable started_at: %w", row.ID, err)
	}
	out := backup.Run{ID: row.ID, StartedAt: started, Outcome: row.Outcome}
	if row.FinishedAt != nil {
		finished, err := time.Parse(time.RFC3339Nano, *row.FinishedAt)
		if err != nil {
			return backup.Run{}, fmt.Errorf("store: backup run %s has an unreadable finished_at: %w", row.ID, err)
		}
		out.FinishedAt = &finished
	}
	return out, nil
}
