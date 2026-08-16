package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/backup"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// backupListLimit is how many runs the page shows. Rows are never pruned, so the
// page bounds itself rather than the table doing it (spec 0020, AC-11, AC-17).
const backupListLimit = 50

// Backups is what the page needs from the backup service. It is an interface so
// the page can be rendered with backups off, which is a state rather than an
// error (AC-18).
type Backups interface {
	Configured() bool
	Run(ctx context.Context, accountID string) (reason domain.BackupReason, err error)
}

// BackupRuns is the read half, the record the page lists.
type BackupRuns interface {
	ListBackupRuns(ctx context.Context, limit int64) ([]store.BackupRun, error)
}

// backupsPageData is the recent runs and whether backups are configured at all.
type backupsPageData struct {
	// Configured comes from the parsed configuration, not from the absence of
	// rows, so a configured platform that has never run yet reads as pending
	// rather than as unconfigured (AC-18).
	Configured bool
	Runs       []backupRunRow
	Message    string
}

// backupRunRow is one run as the page shows it. The bucket name and the
// credential are not here and never will be: the object key is enough to type
// into `deployer restore`, which is the whole reason it is shown.
type backupRunRow struct {
	Started   string
	Finished  string
	Outcome   string
	Size      string
	ObjectKey string
	Reason    string
	Trigger   string
}

func (s *Server) adminBackupsPage(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	s.renderBackups(w, r, admin, sess, http.StatusOK, "")
}

// adminBackupRun starts a backup outside the schedule. Every press writes an
// audit row, both when it is allowed and when it is refused (AC-19, AC-22).
func (s *Server) adminBackupRun(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	// The token is checked before anything runs, so a post without a valid one
	// costs no snapshot and writes no row (AC-19).
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	if s.backups == nil || !s.backups.Configured() {
		auth.Record(r.Context(), s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     admin.ID, Action: auth.ActionAdmin,
			TargetType: "backup", Reason: "run: not_configured",
		})
		s.renderBackups(w, r, admin, sess, http.StatusUnprocessableEntity,
			"Backups are not configured, so there is nothing to run.")
		return
	}

	// The run does not belong to the caller's connection. Go cancels a request's
	// context the moment the client disconnects, and a run cancelled halfway
	// through cannot close its own row or send its own alert on that same
	// context: the row stays running, the unique index then refuses every later
	// run, and the schedule skips every tick, silently, until the pod restarts.
	// Detaching keeps the values (the audit and log context) and drops only the
	// cancellation, so a closed tab costs an unread page rather than the backups.
	runCtx := context.WithoutCancel(r.Context())

	reason, err := s.backups.Run(runCtx, admin.ID)
	switch {
	case errors.Is(err, backup.ErrInFlight):
		// The refusal comes from the unique index, not from a read followed by a
		// write, and the caller is told with the closed code (AC-20).
		auth.Record(runCtx, s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     admin.ID, Action: auth.ActionAdmin,
			TargetType: "backup", Reason: "run: in_flight",
		})
		s.renderBackups(w, r, admin, sess, http.StatusConflict,
			"A backup is already running. Wait for it to finish, then try again.")
		return
	case reason != "":
		auth.Record(runCtx, s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     admin.ID, Action: auth.ActionAdmin, Allowed: true,
			TargetType: "backup", Reason: "run: " + reason.String(),
		})
		s.renderBackups(w, r, admin, sess, http.StatusOK,
			"The backup ran and failed: "+reason.String()+". The run is in the list below.")
		return
	}

	auth.Record(runCtx, s.auditor, auth.Audit{
		ClientAddress: s.clientAddress(r),
		AccountID:     admin.ID, Action: auth.ActionAdmin, Allowed: true,
		TargetType: "backup", Reason: "run: succeeded",
	})
	s.renderBackups(w, r, admin, sess, http.StatusOK, "The backup finished.")
}

// renderBackups reads the record and writes the page. A read that fails renders
// the page with the message rather than an empty table, which would read as
// healthy.
func (s *Server) renderBackups(w http.ResponseWriter, r *http.Request, admin auth.Account, sess auth.Session,
	status int, message string,
) {
	data := backupsPageData{
		Configured: s.backups != nil && s.backups.Configured(),
		Message:    message,
	}
	if s.backupRuns != nil {
		rows, err := s.backupRuns.ListBackupRuns(r.Context(), backupListLimit)
		if err != nil {
			// Appended rather than substituted: the admin who just pressed the
			// button is owed the answer to what happened to their run, and a
			// failed list read is a second thing that went wrong, not a
			// replacement for the first. The status is only raised when the
			// caller had none more specific.
			const readFailed = "The backup record could not be read just now."
			if data.Message == "" {
				data.Message = readFailed
			} else {
				data.Message += " " + readFailed
			}
			if status == http.StatusOK {
				status = http.StatusInternalServerError
			}
		} else {
			data.Runs = make([]backupRunRow, 0, len(rows))
			for _, row := range rows {
				data.Runs = append(data.Runs, toBackupRunRow(row))
			}
		}
	}
	s.render(w, r, admin, sess, status, "backups", "backups", data)
}

// toBackupRunRow renders one stored row for the page.
func toBackupRunRow(row store.BackupRun) backupRunRow {
	out := backupRunRow{
		Started: row.StartedAt,
		Outcome: row.Outcome,
		Trigger: row.Trigger,
	}
	if row.FinishedAt != nil {
		out.Finished = *row.FinishedAt
	}
	if row.ObjectKey != nil {
		out.ObjectKey = *row.ObjectKey
	}
	if row.FailureReason != nil {
		out.Reason = *row.FailureReason
	}
	if row.SizeBytes != nil {
		out.Size = humanBytes(*row.SizeBytes)
	}
	return out
}

// humanBytes renders a size the way a person reads one, bounded by the two units
// a database backup ever lands in.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 1, 64) + " kB"
	default:
		return strconv.FormatInt(n, 10) + " bytes"
	}
}
