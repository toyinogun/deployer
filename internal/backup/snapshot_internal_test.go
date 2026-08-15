// This file is in package backup rather than backup_test because the property
// it pins belongs to snapshot itself: the temp files a run makes are deleted on
// every exit path, so nothing outside the package can ever see the mode.
package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

// TestSnapshotIsReadableOnlyByItsOwner pins the mode of the plaintext snapshot.
// SQLite creates the file, not this package, so a default umask leaves a full
// copy of every password hash, session, and app secret world readable for as
// long as the encryption and the upload take (AC-2a).
func TestSnapshotIsReadableOnlyByItsOwner(t *testing.T) {
	dir := t.TempDir()

	db, err := sql.Open("sqlite", filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatalf("opening the source database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("seeding the source database: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (id) VALUES ('acct_one')`); err != nil {
		t.Fatalf("seeding the source database: %v", err)
	}

	path := filepath.Join(dir, "snapshot.db")
	if err := snapshot(context.Background(), db, path); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat of the snapshot: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("snapshot mode is %#o, want %#o", got, 0o600)
	}
}

// encryptToNewRecipient encrypts a fixed plaintext to a freshly generated
// recipient and returns what the encryption recorded. Two calls give two files
// encrypted to two different keys, which is the whole setup confirmRecipient
// needs: nothing can read a recipient out of an X25519 header, so the only way
// to build the wrong recipient case is to hold the other one's stanzas.
func encryptToNewRecipient(t *testing.T, dir, name string) encrypted {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating an age identity: %v", err)
	}
	src := filepath.Join(dir, name+".plain")
	if err := os.WriteFile(src, []byte("a database, near enough"), 0o600); err != nil {
		t.Fatalf("writing the plaintext: %v", err)
	}
	enc, err := encryptTo(src, filepath.Join(dir, name+".age"), identity.Recipient())
	if err != nil {
		t.Fatalf("encryptTo: %v", err)
	}
	return enc
}

// TestConfirmRecipient_acceptsTheHeaderItsOwnRecipientMade is the happy path:
// the stanzas the run recorded are the ones on disk (AC-4c).
func TestConfirmRecipient_acceptsTheHeaderItsOwnRecipientMade(t *testing.T) {
	dir := t.TempDir()
	enc := encryptToNewRecipient(t, dir, "ours")

	if err := confirmRecipient(enc.Path, enc.stanzas); err != nil {
		t.Fatalf("confirmRecipient refused a header its own recipient made: %v", err)
	}
}

// TestConfirmRecipient_refusesAHeaderAnotherRecipientMade is the check the whole
// encryption design leans on. Nothing else in the run catches a file encrypted
// to a key nobody holds: the read back compares bytes, and no code in the
// cluster can decrypt. A file that passes every other check and decrypts for
// nobody is only ever caught here (AC-4c).
func TestConfirmRecipient_refusesAHeaderAnotherRecipientMade(t *testing.T) {
	dir := t.TempDir()
	ours := encryptToNewRecipient(t, dir, "ours")
	theirs := encryptToNewRecipient(t, dir, "theirs")

	if err := confirmRecipient(theirs.Path, ours.stanzas); err == nil {
		t.Fatal("confirmRecipient accepted a file encrypted to a different recipient")
	}
}

// TestConfirmRecipient_refusesWhenThereIsNothingToCheckAgainst pins the guard.
// An empty stanza list would otherwise pass the loop without comparing
// anything, which is a check that reports success for having done nothing.
func TestConfirmRecipient_refusesWhenThereIsNothingToCheckAgainst(t *testing.T) {
	dir := t.TempDir()
	enc := encryptToNewRecipient(t, dir, "ours")

	if err := confirmRecipient(enc.Path, nil); err == nil {
		t.Fatal("confirmRecipient accepted a file with no stanza to check it against")
	}
}
