package domain_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// backupCodes is every code a backup run is allowed to end on. The list is
// written out here rather than derived, so adding a constant without deciding
// it belongs in the closed set fails this file rather than passing quietly.
var backupCodes = []domain.BackupReason{
	domain.BackupSnapshotFailed,
	domain.BackupIntegrityFailed,
	domain.BackupEncryptFailed,
	domain.BackupUploadFailed,
	domain.BackupVerifyFailed,
	domain.BackupStranded,
	domain.BackupInternal,
}

// covers: AC-10 - every code the package declares is accepted, and each one
// stores and renders as the string the row and the page carry.
func TestBackupReasonValid_acceptsEveryDeclaredCode(t *testing.T) {
	t.Parallel()

	want := map[domain.BackupReason]string{
		domain.BackupSnapshotFailed:  "snapshot_failed",
		domain.BackupIntegrityFailed: "integrity_failed",
		domain.BackupEncryptFailed:   "encrypt_failed",
		domain.BackupUploadFailed:    "upload_failed",
		domain.BackupVerifyFailed:    "verify_failed",
		domain.BackupStranded:        "stranded",
		domain.BackupInternal:        "internal",
	}
	if len(want) != len(backupCodes) {
		t.Fatalf("the closed set has %d codes, this test names %d", len(backupCodes), len(want))
	}

	for _, code := range backupCodes {
		if !code.Valid() {
			t.Errorf("%q should be a valid backup reason", code)
		}
		if got := code.String(); got != want[code] {
			t.Errorf("String: got %q, want %q", got, want[code])
		}
	}
}

// covers: AC-10 - the set is closed, so anything outside it is refused rather
// than reaching backup_runs.failure_reason or the admin page.
func TestBackupReasonValid_refusesAnythingOutsideTheSet(t *testing.T) {
	t.Parallel()

	for _, code := range []domain.BackupReason{
		"",
		"snapshot failed",
		"Snapshot_Failed",
		"snapshot_failed ",
		"unknown",
		"failed",
	} {
		if code.Valid() {
			t.Errorf("%q should not be a valid backup reason", code)
		}
	}
}

// covers: AC-10 - the two reason sets answer different questions to different
// callers, so a deploy failure code is not a backup one. "internal" is the one
// deliberate overlap: both sets name the same platform fault, and each type
// still validates only against its own list.
func TestBackupReasonValid_refusesDeployReasonCodes(t *testing.T) {
	t.Parallel()

	for _, deploy := range []domain.Reason{
		domain.ReasonBuildFailed,
		domain.ReasonUploadInvalid,
		domain.ReasonAppNeverReady,
		domain.ReasonTimeout,
		domain.ReasonSuperseded,
	} {
		if domain.BackupReason(deploy).Valid() {
			t.Errorf("the deploy code %q should not pass as a backup reason", deploy)
		}
	}

	if !domain.BackupReason(domain.ReasonInternal).Valid() {
		t.Error("internal is the one shared spelling and should stay valid on both types")
	}
}

// covers: AC-10 - the codes are what a machine reads out of a row, so they stay
// lower case snake with nothing a caller has to trim or unquote.
func TestBackupReasonCodes_areMachineReadable(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, code := range backupCodes {
		s := code.String()
		if s == "" {
			t.Error("a code should never be empty")
			continue
		}
		if s != strings.ToLower(s) || strings.ContainsAny(s, " \t\n\"'") {
			t.Errorf("%q should be lower case with no spaces or quotes", s)
		}
		if seen[s] {
			t.Errorf("%q is declared twice, so two failures would be indistinguishable", s)
		}
		seen[s] = true
	}
}
