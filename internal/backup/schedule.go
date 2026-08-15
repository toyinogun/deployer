package backup

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Sweep is the startup work that has to happen before the process serves
// anything: the temp directory emptied, and any row a dead predecessor left
// running ended.
//
// One replica and a Recreate strategy make a running row definitionally dead, so
// there is no sweeper, no grace period, and no timer (AC-9). The directory is
// emptied because a hard kill runs no exit path, and what it leaves behind is a
// full unencrypted copy of the database (AC-2a).
//
// Neither half stops the platform starting. A backup that cannot run is worth
// far less than a platform that will not boot, and both failures are loud.
func (s *Service) Sweep(ctx context.Context) {
	if s == nil {
		return
	}
	if err := EmptyTempDir(s.opts.TempDir); err != nil {
		slog.ErrorContext(ctx, "could not empty the backup temporary directory", "error", err)
	}
	n, err := s.runs.StrandBackupRuns(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "could not end the backup runs a previous process left behind", "error", err)
		return
	}
	if n > 0 {
		slog.WarnContext(ctx, "ended backup runs a previous process left behind", "runs", n)
	}
}

// Schedule fires a backup every interval, measured from the end of each run so a
// tick never lands on the heels of one that just finished (AC-12).
//
// It runs one immediately when the newest success is older than the interval, so
// a pod that restarts daily still gets backed up. It returns when ctx is done.
func (s *Service) Schedule(ctx context.Context) {
	if s == nil {
		return
	}
	slog.InfoContext(ctx, "backup schedule started", "interval", s.opts.Interval)

	if s.dueNow(ctx) {
		s.tick(ctx)
	}
	for {
		// The timer is reset after each run rather than left ticking, which is
		// what makes the interval measure from the end of a run rather than from
		// the start of the previous one.
		timer := time.NewTimer(s.opts.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.tick(ctx)
		}
	}
}

// dueNow reports whether the platform is overdue at startup. No success on
// record means yes: a platform that has never been backed up is as overdue as
// one can be.
func (s *Service) dueNow(ctx context.Context) bool {
	last, err := s.runs.LatestSucceededBackupRun(ctx)
	switch {
	case errors.Is(err, ErrNoRun):
		return true
	case err != nil:
		// Unreadable is not the same as overdue, and running a backup on a
		// database that cannot answer a read is not the move.
		slog.ErrorContext(ctx, "could not read the newest successful backup", "error", err)
		return false
	}
	// finished_at rather than started_at: the interval is measured from the end
	// of a run everywhere else too.
	end := last.StartedAt
	if last.FinishedAt != nil {
		end = *last.FinishedAt
	}
	return s.opts.Now().Sub(end) >= s.opts.Interval
}

// tick takes one scheduled backup, unless one is already going.
//
// The in flight check is a read before any insert is attempted, deliberately.
// A backup is already happening, which is what the tick wanted, and the startup
// catch up covers the case where that run then fails. The unique index refusal
// exists for the button, which has a caller to answer to; a scheduled tick has
// none, so it must not be able to vanish into a swallowed constraint error
// (AC-12a).
func (s *Service) tick(ctx context.Context) {
	running, err := s.runs.RunningBackupRun(ctx)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "a backup is already running, so this tick is skipped", "run_id", running.ID)
		return
	case !errors.Is(err, ErrNoRun):
		slog.ErrorContext(ctx, "could not check whether a backup is already running", "error", err)
		return
	}
	if _, err := s.Run(ctx, ""); err != nil && !errors.Is(err, ErrInFlight) {
		// Run has already recorded the row and alerted. Nothing more to do here
		// than not treat it as fatal to the schedule.
		slog.DebugContext(ctx, "the scheduled backup did not succeed", "error", err)
	}
}
