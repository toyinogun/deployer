package backup_test

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	_ "modernc.org/sqlite"

	"github.com/toyinogun/deployer/internal/backup"
)

// store places bytes in the fake bucket directly, for the cases that need an
// object the run itself would never produce.
func (b *fakeBucket) store(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
}

// identityFile writes an age identity out the way age-keygen does, comment line
// and all, and returns the path. This is the only shape restore accepts: a file,
// never an argument and never an environment variable (AC-23).
func identityFile(t *testing.T, id *age.X25519Identity) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.txt")
	body := "# created: 2026-08-15T00:00:00Z\n" +
		"# public key: " + id.Recipient().String() + "\n" +
		id.String() + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the identity file: %v", err)
	}
	return path
}

// backedUp takes one real backup and returns the object key it landed under.
func backedUp(t *testing.T, h *harness) string {
	t.Helper()
	if reason, err := h.svc.Run(t.Context(), ""); reason != "" || err != nil {
		t.Fatalf("the backup should have succeeded: reason %q, err %v", reason, err)
	}
	key, _ := h.bucket.only(t)
	return key
}

// covers: AC-23, AC-25 - the half that runs outside the cluster: fetch the
// object, decrypt it with the identity nothing in the cluster holds, and write a
// database the accounts are still in.
func TestRestore_writesBackADatabaseHoldingTheSourceRows(t *testing.T) {
	h := newHarness(t, 2)
	key := backedUp(t, h)
	out := filepath.Join(t.TempDir(), "restored.db")

	err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key:          key,
		IdentityPath: identityFile(t, h.identity),
		Out:          out,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	db, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatalf("opening the restored database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var accounts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatalf("counting accounts in the restored database: %v", err)
	}
	if accounts != 2 {
		t.Fatalf("the restored database should hold the source's 2 accounts, holds %d", accounts)
	}
}

// The restored file is the plaintext database, so it lands readable only by the
// person who ran the command. This is the same property the snapshot has on the
// volume, on the other side of the round trip.
func TestRestore_writesTheDatabaseReadableOnlyByItsOwner(t *testing.T) {
	h := newHarness(t, 1)
	key := backedUp(t, h)
	out := filepath.Join(t.TempDir(), "restored.db")

	if err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key: key, IdentityPath: identityFile(t, h.identity), Out: out,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat of the restored file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("the restored database is %#o, want %#o", got, 0o600)
	}
}

// covers: AC-24 - restore never overwrites. The destination is checked before
// anything is fetched, so a mistyped path costs a download rather than the
// database that was already there.
func TestRestore_refusesWhenTheDestinationExists(t *testing.T) {
	h := newHarness(t, 1)
	key := backedUp(t, h)
	out := filepath.Join(t.TempDir(), "restored.db")
	existing := []byte("the database that was already there")
	if err := os.WriteFile(out, existing, 0o600); err != nil {
		t.Fatalf("placing a file at the destination: %v", err)
	}

	err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key: key, IdentityPath: identityFile(t, h.identity), Out: out,
	})

	if err == nil {
		t.Fatal("want a refusal when the destination exists")
	}
	if !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("the refusal should say why, got %v", err)
	}
	after, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("reading the destination back: %v", readErr)
	}
	if !bytes.Equal(after, existing) {
		t.Fatalf("the existing file was touched: %q", after)
	}
}

// All three arguments are required, and a missing one is named rather than
// producing a confusing failure further down.
func TestRestore_refusesAnIncompleteRequest(t *testing.T) {
	h := newHarness(t, 1)
	identity := identityFile(t, h.identity)
	out := filepath.Join(t.TempDir(), "restored.db")

	for _, tt := range []struct {
		name string
		opts backup.RestoreOptions
	}{
		{name: "no key", opts: backup.RestoreOptions{IdentityPath: identity, Out: out}},
		{name: "no identity", opts: backup.RestoreOptions{Key: "db/object.age", Out: out}},
		{name: "no destination", opts: backup.RestoreOptions{Key: "db/object.age", IdentityPath: identity}},
		{name: "nothing at all", opts: backup.RestoreOptions{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := backup.Restore(t.Context(), h.bucket, tt.opts)

			if err == nil {
				t.Fatal("want a refusal")
			}
			if !strings.Contains(err.Error(), "-key") {
				t.Errorf("the refusal should name what it needs, got %v", err)
			}
		})
	}
}

// covers: AC-23 - a file that is not an age identity is refused, and the error
// echoes no part of it. This is the one place a private key passes through, so a
// parse failure is not a place to quote the file back.
func TestRestore_refusesAnIdentityFileThatHoldsNoIdentity(t *testing.T) {
	h := newHarness(t, 1)
	key := backedUp(t, h)

	for _, tt := range []struct {
		name    string
		content string
	}{
		{name: "a private key of the wrong kind", content: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEsecretmaterial\n"},
		{name: "the public recipient by mistake", content: h.identity.Recipient().String() + "\n"},
		{name: "an empty file", content: ""},
		{name: "comments only", content: "# created: 2026-08-15T00:00:00Z\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "identity.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("writing the identity file: %v", err)
			}
			out := filepath.Join(t.TempDir(), "restored.db")

			err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
				Key: key, IdentityPath: path, Out: out,
			})

			if err == nil {
				t.Fatal("want a refusal for a file that is not an age identity")
			}
			for _, line := range strings.Fields(tt.content) {
				if len(line) > 8 && strings.Contains(err.Error(), line) {
					t.Errorf("the error echoes the file's contents (%q): %v", line, err)
				}
			}
			if _, statErr := os.Stat(out); statErr == nil {
				t.Error("nothing should have been written without a usable identity")
			}
		})
	}
}

// An identity file that is not there is its own refusal, distinct from one that
// is there and unusable.
func TestRestore_refusesAMissingIdentityFile(t *testing.T) {
	h := newHarness(t, 1)
	key := backedUp(t, h)

	err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key:          key,
		IdentityPath: filepath.Join(t.TempDir(), "not-here.txt"),
		Out:          filepath.Join(t.TempDir(), "restored.db"),
	})

	if err == nil {
		t.Fatal("want a refusal for an identity file that is not there")
	}
	if !strings.Contains(err.Error(), "opening the identity file") {
		t.Errorf("the error should say the file could not be opened, got %v", err)
	}
}

// covers: AC-4 - only the key the object was encrypted to opens it. Another
// perfectly valid age identity does not, and nothing is written when it fails.
func TestRestore_refusesAnIdentityTheObjectWasNotEncryptedTo(t *testing.T) {
	h := newHarness(t, 1)
	key := backedUp(t, h)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating a second identity: %v", err)
	}
	out := filepath.Join(t.TempDir(), "restored.db")

	err = backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key: key, IdentityPath: identityFile(t, other), Out: out,
	})

	if err == nil {
		t.Fatal("want a refusal when the identity does not match the object")
	}
	if !strings.Contains(err.Error(), "decrypting") {
		t.Errorf("the error should name the decryption, got %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a file that cannot be decrypted should leave no destination behind")
	}
}

// An object key that is not in the bucket fails on the fetch, and writes nothing.
func TestRestore_failsWhenTheObjectIsNotThere(t *testing.T) {
	h := newHarness(t, 1)
	out := filepath.Join(t.TempDir(), "restored.db")

	err := backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key:          "db/20260815T030000Z-bkp_nope.age",
		IdentityPath: identityFile(t, h.identity),
		Out:          out,
	})

	if err == nil {
		t.Fatal("want a failure for an object that is not in the bucket")
	}
	if !strings.Contains(err.Error(), "fetching the object") {
		t.Errorf("the error should name the fetch, got %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a failed fetch should leave no destination behind")
	}
}

// covers: AC-23 - the restored file is checked the same way the snapshot was
// before it was uploaded. Something that decrypts cleanly and is not a database
// is worth knowing about now rather than on the volume.
func TestRestore_refusesSomethingThatDecryptsAndIsNotADatabase(t *testing.T) {
	h := newHarness(t, 1)

	var ciphertext bytes.Buffer
	w, err := age.Encrypt(&ciphertext, h.identity.Recipient())
	if err != nil {
		t.Fatalf("encrypting the decoy: %v", err)
	}
	if _, err := io.WriteString(w, "this is not a SQLite file at all"); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("finishing the decoy: %v", err)
	}
	h.bucket.store("db/20260815T030000Z-bkp_decoy.age", ciphertext.Bytes())
	out := filepath.Join(t.TempDir(), "restored.db")

	err = backup.Restore(t.Context(), h.bucket, backup.RestoreOptions{
		Key:          "db/20260815T030000Z-bkp_decoy.age",
		IdentityPath: identityFile(t, h.identity),
		Out:          out,
	})

	if err == nil {
		t.Fatal("want a refusal for a file that is not a sound database")
	}
	if !strings.Contains(err.Error(), "not a sound database") {
		t.Errorf("the error should say what is wrong with it, got %v", err)
	}
	// A failed restore leaves nothing at the destination. Otherwise the operator
	// is holding a plaintext file that is not a database, and the retry is
	// refused by the never overwrites guard for a reason that is not the real
	// one (AC-24).
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a failed restore should leave no file at the destination, stat gave %v", err)
	}
	if left, err := os.ReadDir(filepath.Dir(out)); err != nil {
		t.Fatalf("reading the destination directory: %v", err)
	} else if len(left) != 0 {
		t.Fatalf("a failed restore should leave nothing behind, found %d entries", len(left))
	}
}
