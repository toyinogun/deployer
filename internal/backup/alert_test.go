package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/backup"
	"github.com/toyinogun/deployer/internal/domain"
)

// sentMail is one message the alerter handed the mailer.
type sentMail struct {
	to      string
	subject string
	body    string
}

// recordingMailer keeps every message, and can be told to fail, which is the
// case that must not change a run's outcome.
type recordingMailer struct {
	sent []sentMail
	err  error
}

func (m *recordingMailer) Send(_ context.Context, to, subject, body string) error {
	m.sent = append(m.sent, sentMail{to: to, subject: subject, body: body})
	return m.err
}

// covers: AC-13 - a failure sends exactly one message, to the configured
// address, and it names the closed reason code.
func TestMailAlerterBackupFailed_sendsOneMessageNamingTheReason(t *testing.T) {
	t.Parallel()

	mailer := &recordingMailer{}
	alerter := backup.NewMailAlerter(mailer, "owner@example.test")

	alerter.BackupFailed(t.Context(), domain.BackupUploadFailed)

	if len(mailer.sent) != 1 {
		t.Fatalf("want one message, got %d", len(mailer.sent))
	}
	got := mailer.sent[0]
	if got.to != "owner@example.test" {
		t.Errorf("recipient: got %q, want the configured address", got.to)
	}
	if !strings.Contains(got.body, domain.BackupUploadFailed.String()) {
		t.Errorf("the body should name the reason code, got %q", got.body)
	}
}

// covers: AC-14 - the failure mail carries the reason code and nothing else. A
// notification that a backup failed is not a place to say where the backups
// live, so the whole message is pinned rather than merely scanned for leaks.
func TestMailAlerterBackupFailed_carriesNothingBesidesTheReason(t *testing.T) {
	t.Parallel()

	for _, reason := range []domain.BackupReason{
		domain.BackupSnapshotFailed,
		domain.BackupIntegrityFailed,
		domain.BackupEncryptFailed,
		domain.BackupUploadFailed,
		domain.BackupVerifyFailed,
		domain.BackupStranded,
		domain.BackupInternal,
	} {
		mailer := &recordingMailer{}
		backup.NewMailAlerter(mailer, "owner@example.test").BackupFailed(t.Context(), reason)

		if len(mailer.sent) != 1 {
			t.Fatalf("%s: want one message, got %d", reason, len(mailer.sent))
		}
		want := "A platform backup run failed.\n\nReason: " + reason.String() +
			"\n\nThe admin backups page has the run.\n"
		if got := mailer.sent[0].body; got != want {
			t.Errorf("%s: body is not the fixed text plus the code.\n got %q\nwant %q", reason, got, want)
		}
	}
}

// covers: AC-14 - nothing about the bucket, the object, or the credential
// reaches a message, whichever half of the pair is sent.
func TestMailAlerter_neverCarriesBucketOrCredentialDetail(t *testing.T) {
	t.Parallel()

	mailer := &recordingMailer{}
	alerter := backup.NewMailAlerter(mailer, "owner@example.test")

	alerter.BackupFailed(t.Context(), domain.BackupUploadFailed)
	alerter.BackupRecovered(t.Context())

	// Words that would only ever appear if the message started quoting the
	// configuration or the object it was about.
	forbidden := []string{"bucket", "endpoint", "db/", ".age", "secret", "access_key", "r2.", "https://"}
	for _, m := range mailer.sent {
		text := strings.ToLower(m.subject + "\n" + m.body)
		for _, word := range forbidden {
			if strings.Contains(text, word) {
				t.Errorf("a mail mentions %q, which is where the backups live: %q", word, text)
			}
		}
	}
}

// covers: AC-13 - the recovery message is the one sent on the first success
// after a failure.
func TestMailAlerterBackupRecovered_sendsOneMessage(t *testing.T) {
	t.Parallel()

	mailer := &recordingMailer{}

	backup.NewMailAlerter(mailer, "owner@example.test").BackupRecovered(t.Context())

	if len(mailer.sent) != 1 {
		t.Fatalf("want one message, got %d", len(mailer.sent))
	}
	if got := mailer.sent[0].body; got != "A platform backup run succeeded after a failure.\n" {
		t.Errorf("body: got %q", got)
	}
}

// An alerter needs both halves. Without a mailer or without somewhere to send,
// there is nothing to build, and a nil alerter is the supported state rather
// than a half configured one that fails on every send.
func TestNewMailAlerter_isNilWithoutAMailerOrARecipient(t *testing.T) {
	t.Parallel()

	if a := backup.NewMailAlerter(nil, "owner@example.test"); a != nil {
		t.Error("no mailer should give no alerter")
	}
	if a := backup.NewMailAlerter(&recordingMailer{}, ""); a != nil {
		t.Error("nowhere to send should give no alerter")
	}
}

// A platform with no alerter still takes backups: every method on a nil alerter
// is safe, which is what lets Service hold one without checking.
func TestMailAlerter_nilIsSafe(t *testing.T) {
	t.Parallel()

	var alerter *backup.MailAlerter

	alerter.BackupFailed(t.Context(), domain.BackupInternal)
	alerter.BackupRecovered(t.Context())
}

// A send that fails is logged and swallowed. The mail is best effort in the same
// way every other message the platform sends is: the run that triggered it has
// already recorded what happened, and a mail server being down must not turn a
// recorded failure into a panic.
func TestMailAlerter_aFailedSendIsSwallowed(t *testing.T) {
	t.Parallel()

	mailer := &recordingMailer{err: errors.New("the mail server refused the connection")}
	alerter := backup.NewMailAlerter(mailer, "owner@example.test")

	alerter.BackupFailed(t.Context(), domain.BackupVerifyFailed)
	alerter.BackupRecovered(t.Context())

	if len(mailer.sent) != 2 {
		t.Fatalf("both sends should still have been attempted, got %d", len(mailer.sent))
	}
}
