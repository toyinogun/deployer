package backup

import (
	"context"
	"log/slog"

	"github.com/toyinogun/deployer/internal/domain"
)

// Mailer is the one thing the alerter needs from the mail package.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// MailAlerter mails the platform owner when a backup fails, and once more when
// it recovers. It carries the reason code and nothing else: no object key, no
// bucket name, no configuration value. A failure notification is not a place to
// leak where the backups are (AC-13, AC-14).
type MailAlerter struct {
	mailer Mailer
	to     string
}

// NewMailAlerter returns an alerter, or nil when there is no mailer or nowhere
// to send. A nil alerter is a supported state: the run still happens and still
// records what it did.
func NewMailAlerter(mailer Mailer, to string) *MailAlerter {
	if mailer == nil || to == "" {
		return nil
	}
	return &MailAlerter{mailer: mailer, to: to}
}

// BackupFailed sends the one failure message.
func (a *MailAlerter) BackupFailed(ctx context.Context, reason domain.BackupReason) {
	if a == nil {
		return
	}
	a.send(ctx, "Deployer: a backup failed",
		"A platform backup run failed.\n\nReason: "+reason.String()+
			"\n\nThe admin backups page has the run.\n")
}

// BackupRecovered sends the one recovery message, on the first success after any
// failure. A success after a success sends nothing, so silence means healthy.
func (a *MailAlerter) BackupRecovered(ctx context.Context) {
	if a == nil {
		return
	}
	a.send(ctx, "Deployer: backups are working again",
		"A platform backup run succeeded after a failure.\n")
}

// send is best effort, in the same way every other message this platform sends
// is: a failure here is logged and changes nothing about the run that triggered
// it. The recipient address never enters the log.
func (a *MailAlerter) send(ctx context.Context, subject, body string) {
	if err := a.mailer.Send(ctx, a.to, subject, body); err != nil {
		slog.ErrorContext(ctx, "could not send the backup alert", "error", err)
	}
}
