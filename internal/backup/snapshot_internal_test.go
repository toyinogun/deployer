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
