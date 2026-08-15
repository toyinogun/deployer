// Package backup takes the platform's own database somewhere that is not this
// cluster: a consistent snapshot, checked while it is still plaintext,
// encrypted to a public recipient whose private half never enters the cluster,
// uploaded, then read back to confirm the bytes arrived. Governing spec is
// docs/specs/0020-platform-backup-restore.
//
// Nothing here ever holds an age identity, so the platform cannot read its own
// backups and neither can anything that compromises it.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"
)

// DefaultTempDir is the single fixed directory every temporary file a run makes
// lives in. Fixed and exclusive rather than a random path, because the startup
// sweep has to be able to empty it after a hard kill that ran no exit path
// (AC-2, AC-2a).
const DefaultTempDir = "/data/backup-tmp"

// snapshot writes a consistent copy of the live database to path with VACUUM
// INTO, executed through the running process's own handle. Nothing is quiesced
// and no request is blocked: SQLite holds a read transaction for the duration
// and the platform keeps serving throughout (AC-1).
//
// The destination is interpolated rather than bound because VACUUM INTO takes an
// expression the driver will not parameterise. It is composed from the temp
// directory and a run id the platform generated itself, and no caller supplied
// value ever reaches it.
func snapshot(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+path+`'`); err != nil {
		return fmt.Errorf("backup: snapshotting the database: %w", err)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("backup: the snapshot was not written: %w", err)
	}
	// SQLite creates the destination, not this package, so it lands at 0666
	// minus the umask: world readable on the usual one. It is the plaintext
	// copy of every password hash, session, and app secret, and it sits there
	// for as long as the encryption and the upload take, so the mode is
	// tightened as soon as it exists. The rollback journal beside it is
	// SQLite's too and is gone before this returns, which is why the temp
	// directory is 0700: that is what bounds the sub second window (AC-2a).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("backup: tightening the snapshot's mode: %w", err)
	}
	return nil
}

// checkSnapshot opens the snapshot as a separate read only database and proves
// it is sound before anything is encrypted: integrity_check must return exactly
// ok, and accounts must hold at least one row. Either failing ends the run and
// uploads nothing (AC-3).
//
// The account count is what catches a snapshot that is structurally perfect and
// empty. integrity_check is perfectly happy with one of those, and it would
// restore to a platform nobody can sign in to.
func checkSnapshot(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("backup: opening the snapshot: %w", err)
	}
	defer func() { _ = db.Close() }()

	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("backup: running integrity_check on the snapshot: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("backup: the snapshot failed integrity_check: %s", result)
	}

	var accounts int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		return fmt.Errorf("backup: counting accounts in the snapshot: %w", err)
	}
	if accounts == 0 {
		return fmt.Errorf("backup: the snapshot holds no accounts")
	}
	return nil
}

// encrypted is what a finished encryption knows about the file it wrote, which
// is everything the upload and the read back need. Encryption streams to disk,
// so the exact length and hash are known before a single byte is sent (AC-4b).
type encrypted struct {
	Path   string
	Size   int64
	SHA256 string
	// stanzas are what the configured recipient produced for this file's key.
	// Held so the header of the finished file can be checked against them.
	stanzas []*age.Stanza
}

// encryptTo streams the plaintext at src into dst, encrypted to recipient,
// hashing the ciphertext as it is written. The ciphertext is never held whole in
// memory (AC-4, AC-4a, AC-4b).
func encryptTo(src, dst string, recipient age.Recipient) (encrypted, error) {
	in, err := os.Open(src)
	if err != nil {
		return encrypted{}, fmt.Errorf("backup: opening the snapshot to encrypt: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return encrypted{}, fmt.Errorf("backup: creating the encrypted file: %w", err)
	}
	defer func() { _ = out.Close() }()

	sum := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(out, sum)}

	// The recipient is wrapped so the run keeps the stanzas it produced. They
	// are what confirmRecipient compares the finished header against: age gives
	// no way to read them back out of a file without an identity, and the whole
	// point of this design is that no identity exists here (AC-4c).
	recorder := &recordingRecipient{inner: recipient}

	w, err := age.Encrypt(counter, recorder)
	if err != nil {
		return encrypted{}, fmt.Errorf("backup: starting encryption: %w", err)
	}
	if _, err := io.Copy(w, in); err != nil {
		return encrypted{}, fmt.Errorf("backup: encrypting the snapshot: %w", err)
	}
	// age writes its trailer on Close, so closing is part of producing the file
	// rather than tidying up after it. Hashing or sizing before this point
	// measures an unfinished object.
	if err := w.Close(); err != nil {
		return encrypted{}, fmt.Errorf("backup: finishing encryption: %w", err)
	}
	if err := out.Close(); err != nil {
		return encrypted{}, fmt.Errorf("backup: writing the encrypted file: %w", err)
	}

	return encrypted{
		Path:    dst,
		Size:    counter.n,
		SHA256:  hex.EncodeToString(sum.Sum(nil)),
		stanzas: recorder.stanzas,
	}, nil
}

// recordingRecipient delegates wrapping to the configured recipient and keeps
// what it produced. It changes nothing about the encryption.
type recordingRecipient struct {
	inner   age.Recipient
	stanzas []*age.Stanza
}

// Wrap wraps the file key with the real recipient and records the result.
func (r *recordingRecipient) Wrap(fileKey []byte) ([]*age.Stanza, error) {
	s, err := r.inner.Wrap(fileKey)
	if err != nil {
		return nil, err
	}
	r.stanzas = s
	return s, nil
}

// countingWriter counts the bytes going through it, which is how the run learns
// the object's length without stat-ing a file it just wrote.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// confirmRecipient reads the age header back off the finished file and confirms
// it carries exactly the stanzas the configured recipient produced. It needs no
// private key, and it is the only check that catches the file having been
// encrypted to somebody else, which otherwise produces a well formed object that
// passes every other check and decrypts for nobody (AC-4c).
//
// It compares the recorded stanzas rather than the recipient string, because an
// X25519 stanza carries an ephemeral share rather than the recipient it is for:
// nothing without a private key can read a recipient out of a header. What can
// be proved without one is that the header on disk is the header this recipient
// made, which is the same guarantee from the other end.
func confirmRecipient(path string, want []*age.Stanza) error {
	if len(want) == 0 {
		return fmt.Errorf("backup: the recipient produced no stanza to check the header against")
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup: opening the encrypted file to check its header: %w", err)
	}
	defer func() { _ = f.Close() }()

	header, err := age.ExtractHeader(f)
	if err != nil {
		return fmt.Errorf("backup: reading the age header back: %w", err)
	}

	for _, s := range want {
		line := "-> " + s.Type
		for _, arg := range s.Args {
			line += " " + arg
		}
		if !bytes.Contains(header, []byte(line+"\n")) {
			return fmt.Errorf("backup: the age header does not name the configured recipient's %s stanza", s.Type)
		}
		body := base64.RawStdEncoding.EncodeToString(s.Body)
		if !bytes.Contains(header, []byte(body)) {
			return fmt.Errorf("backup: the age header does not carry the key this recipient wrapped")
		}
	}
	return nil
}

// cleanup removes the run's temporary files. The caller runs it on every exit
// path, success and failure alike, and logs a failure at warn: a cleanup that
// itself fails never changes the run's outcome (AC-2).
func cleanup(paths ...string) error {
	var first error
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && first == nil {
			first = fmt.Errorf("backup: removing %s: %w", filepath.Base(p), err)
		}
	}
	return first
}

// EmptyTempDir removes everything in the temp directory, creating it when it is
// not there. It runs at startup, before the process serves anything: a hard kill
// runs no exit path, so a crash mid run otherwise leaves a full unencrypted copy
// of every password hash, session, token, and app secret sitting on the volume
// indefinitely, visible to no surface this feature builds (AC-2a).
func EmptyTempDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: creating %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backup: reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("backup: emptying %s: %w", dir, err)
		}
	}
	return nil
}
