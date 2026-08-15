package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"filippo.io/age"

	"github.com/toyinogun/deployer/internal/domain"
)

// keyTimeLayout is how the object key stamps the run's start. Compact and
// deliberately not RFC3339, which carries colons: they are legal in an S3 key
// and awkward in a shell argument, and this key is typed by hand into
// `deployer restore` (AC-5).
const keyTimeLayout = "20060102T150405Z"

// Runs is the slice of the store this package needs. It is declared here, where
// it is used, so nothing in the run imports the store package (spec 0002, and
// the layering rule in AGENTS.md).
type Runs interface {
	StartBackupRun(ctx context.Context, accountID string) (Run, error)
	FinishBackupRunSucceeded(ctx context.Context, id string, res Result) error
	FinishBackupRunFailed(ctx context.Context, id string, reason domain.BackupReason) error
	RunningBackupRun(ctx context.Context) (Run, error)
	LatestSucceededBackupRun(ctx context.Context) (Run, error)
	LatestTerminalBackupRun(ctx context.Context) (Run, error)
	StrandBackupRuns(ctx context.Context) (int64, error)
}

// Run is one row of the record, in the shape this package reads it.
type Run struct {
	ID         string
	StartedAt  time.Time
	Outcome    string
	FinishedAt *time.Time
}

// Result is what a run that reached the bucket records.
type Result struct {
	ObjectKey string
	SizeBytes int64
	Checksum  string
}

// Alerter is how a run tells somebody it failed, and that it recovered. Best
// effort: a send that fails is logged and changes nothing about the run.
type Alerter interface {
	BackupFailed(ctx context.Context, reason domain.BackupReason)
	BackupRecovered(ctx context.Context)
}

// Options is everything a Service needs to take a backup.
type Options struct {
	// DB is the live database handle the snapshot is taken through. It is the
	// running process's own, never a second one opened on the file (AC-1).
	DB *sql.DB
	// Recipient is the age public recipient, parsed once at startup in
	// internal/config. The private half has no variable, no Secret, and no code
	// path (AC-4).
	Recipient *age.X25519Recipient
	// TempDir is where both temporary files live. Fixed and exclusive so the
	// startup sweep can empty it (AC-2, AC-2a).
	TempDir string
	// Interval is how often the schedule fires, measured from the end of each
	// run (AC-12).
	Interval time.Duration
	// Now is the clock every timestamp comes from, so a test can pin it.
	Now func() time.Time
}

// Service takes backups. A nil Service is the unconfigured platform: every
// method on it is safe and does nothing, which is what lets the control plane
// boot, warn once, and work in every other respect (AC-15).
type Service struct {
	runs    Runs
	store   ObjectStore
	alerter Alerter
	opts    Options
}

// New returns a Service, or nil when there is no object store to write to. A nil
// Service is a supported state, not an error: the admin page renders its not
// configured state and no run is ever taken.
func New(runs Runs, store ObjectStore, alerter Alerter, opts Options) *Service {
	if store == nil || opts.Recipient == nil {
		return nil
	}
	if opts.TempDir == "" {
		opts.TempDir = DefaultTempDir
	}
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{runs: runs, store: store, alerter: alerter, opts: opts}
}

// Configured reports whether backups are on. It reads the parsed configuration
// rather than the absence of rows, so a configured platform that has never run
// yet reads as pending rather than unconfigured (AC-18).
func (s *Service) Configured() bool { return s != nil }

// ErrInFlight is what a caller sees when a run is already going. It is the
// store's refusal, surfaced: the unique index decides it, not a read followed by
// a write (AC-8, AC-20).
var ErrInFlight = errors.New("backup: a run is already in flight")

// ErrNoRun is the absent row every single row read here can return: no run in
// flight, no success yet, nothing ended yet. It is a state, not a fault.
var ErrNoRun = errors.New("backup: no such run")

// Run takes one backup end to end and returns the reason it failed, or the empty
// string on success.
//
// accountID names the admin who fired it by hand; empty means the schedule did.
// Every step writes its state before it acts, and every exit path removes both
// temporary files.
func (s *Service) Run(ctx context.Context, accountID string) (domain.BackupReason, error) {
	if s == nil {
		return "", fmt.Errorf("backup: not configured")
	}

	// Read what ended last before the new row is written. After the insert this
	// run is the newest, and whether a success is a recovery would be
	// unanswerable (AC-13).
	previous, hadPrevious := s.previousOutcome(ctx)

	run, err := s.runs.StartBackupRun(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrInFlight) {
			return "", ErrInFlight
		}
		// Anything that is not the unique index is a real write fault: a full
		// volume, a locked database. It is internal, and it is alerted as such
		// rather than reported to an admin as a benign refusal (AC-8a).
		slog.ErrorContext(ctx, "could not start a backup run")
		s.alert(ctx, domain.BackupInternal, previous, hadPrevious, false)
		return domain.BackupInternal, err
	}

	reason, err := s.execute(ctx, run)
	if reason != "" {
		// The wrapped error is logged here and nowhere else. It never reaches
		// the row, the page, or the alert (AC-10, AC-14).
		slog.ErrorContext(ctx, "backup run failed", "run_id", run.ID, "reason", reason.String(), "error", err)
		if ferr := s.runs.FinishBackupRunFailed(ctx, run.ID, reason); ferr != nil {
			slog.ErrorContext(ctx, "could not record the backup failure", "run_id", run.ID, "error", ferr)
		}
		s.alert(ctx, reason, previous, hadPrevious, false)
		return reason, err
	}

	slog.InfoContext(ctx, "backup run succeeded", "run_id", run.ID)
	s.alert(ctx, "", previous, hadPrevious, true)
	return "", nil
}

// execute is the run itself: snapshot, check, encrypt, confirm, upload, read
// back. It returns the closed reason code and the error behind it, and it closes
// the row on success. Both temporary files are removed on every path out,
// including the ones that return early on error (AC-2).
func (s *Service) execute(ctx context.Context, run Run) (domain.BackupReason, error) {
	snapPath := filepath.Join(s.opts.TempDir, run.ID+".db")
	cryptPath := filepath.Join(s.opts.TempDir, run.ID+".db.age")
	defer func() {
		if err := cleanup(snapPath, cryptPath); err != nil {
			// A cleanup that fails is logged and does not change the outcome,
			// but it is the one thing that can leave plaintext on the volume, so
			// it is never silent.
			slog.WarnContext(ctx, "could not remove a backup temporary file", "run_id", run.ID, "error", err)
		}
	}()

	if err := snapshot(ctx, s.opts.DB, snapPath); err != nil {
		return domain.BackupSnapshotFailed, err
	}
	if err := checkSnapshot(ctx, snapPath); err != nil {
		return domain.BackupIntegrityFailed, err
	}

	enc, err := encryptTo(snapPath, cryptPath, s.opts.Recipient)
	if err != nil {
		return domain.BackupEncryptFailed, err
	}
	if err := confirmRecipient(cryptPath, enc.stanzas); err != nil {
		return domain.BackupEncryptFailed, err
	}

	key := objectKey(run)
	if err := s.store.Put(ctx, key, enc.Path, enc.Size); err != nil {
		return domain.BackupUploadFailed, err
	}
	if err := verifyObject(ctx, s.store, key, enc); err != nil {
		return domain.BackupVerifyFailed, err
	}

	if err := s.runs.FinishBackupRunSucceeded(ctx, run.ID, Result{
		ObjectKey: key, SizeBytes: enc.Size, Checksum: enc.SHA256,
	}); err != nil {
		// The object is in the bucket and sound; only the row did not land.
		// That is a platform fault, not a backup failure, and it is reported as
		// one.
		return domain.BackupInternal, err
	}
	return "", nil
}

// objectKey composes the key from values the run already holds, so two runs
// cannot collide and no listing is needed to choose one (AC-5).
func objectKey(run Run) string {
	return "db/" + run.StartedAt.UTC().Format(keyTimeLayout) + "-" + run.ID + ".age"
}

// previousOutcome reads the outcome of the newest run that already ended. The
// second return is false on a platform that has never finished one, which is why
// the very first run it ever takes is never a recovery (AC-13).
func (s *Service) previousOutcome(ctx context.Context) (string, bool) {
	prev, err := s.runs.LatestTerminalBackupRun(ctx)
	if err != nil {
		return "", false
	}
	return prev.Outcome, true
}

// alert sends at most one message. A failure always mails. A success mails only
// when the run before it failed, so silence means healthy, and the first run the
// platform ever takes is never a recovery (AC-13).
func (s *Service) alert(ctx context.Context, reason domain.BackupReason, previous string, hadPrevious, succeeded bool) {
	if s.alerter == nil {
		return
	}
	switch {
	case !succeeded:
		s.alerter.BackupFailed(ctx, reason)
	case hadPrevious && previous == "failed":
		s.alerter.BackupRecovered(ctx)
	}
}
