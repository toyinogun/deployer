package backup

import (
	"context"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
)

// RestoreOptions is everything `deployer restore` needs. It is deliberately not
// the control plane's configuration: restore runs outside the cluster, on a
// machine that holds the one thing the cluster never does.
type RestoreOptions struct {
	// Key is the object key, as shown on the admin backups page.
	Key string
	// IdentityPath is the file holding the age private identity. It is read from
	// a file rather than an environment variable or an argument, so it never
	// lands in a process listing (AC-23).
	IdentityPath string
	// Out is where the plaintext database is written. It must not already exist
	// (AC-24).
	Out string
}

// Restore fetches one object, decrypts it, checks it, and writes the plaintext
// database to a local path. It restores to a file and nothing more: getting that
// file onto the volume is a documented step in deploy/README.md, not something
// this does (AC-23, AC-24).
//
// There is no attempt to detect whether it is running inside a cluster. No
// reliable signal for that exists, and a wrong guess either blocks a legitimate
// restore or fails to block anything.
func Restore(ctx context.Context, store ObjectStore, opts RestoreOptions) error {
	if opts.Key == "" || opts.IdentityPath == "" || opts.Out == "" {
		return fmt.Errorf("backup: restore needs -key, -identity and -out")
	}
	// Checked before anything is fetched, so a mistyped destination costs a
	// download rather than a database.
	if _, err := os.Stat(opts.Out); err == nil {
		return fmt.Errorf("backup: %s already exists; restore never overwrites", opts.Out)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup: checking %s: %w", opts.Out, err)
	}

	identities, err := readIdentities(opts.IdentityPath)
	if err != nil {
		return err
	}

	obj, err := store.Get(ctx, opts.Key)
	if err != nil {
		return fmt.Errorf("backup: fetching the object: %w", err)
	}
	defer func() { _ = obj.Close() }()

	plain, err := age.Decrypt(obj, identities...)
	if err != nil {
		return fmt.Errorf("backup: decrypting the object: %w", err)
	}

	// Written beside the destination and renamed into place only once it is
	// sound, so a copy that dies partway or a file that decrypts and is not a
	// database leaves nothing behind. Writing straight to opts.Out would leave a
	// partial plaintext database on the operator's disk and, worse, would make
	// the retry hit the never overwrites guard above and be refused for the
	// wrong reason. It also makes that guarantee atomic rather than a check
	// followed by a write.
	tmp := opts.Out + ".partial"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: creating %s: %w", tmp, err)
	}
	defer func() { _ = os.Remove(tmp) }()

	if _, err := io.Copy(out, plain); err != nil {
		_ = out.Close()
		return fmt.Errorf("backup: writing %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: writing %s: %w", tmp, err)
	}

	// The same check the run made before uploading, made again on what came
	// back. A file that decrypts and is not a sound database is worth knowing
	// about now rather than on the volume.
	if err := checkSnapshot(ctx, tmp); err != nil {
		return fmt.Errorf("backup: the restored file is not a sound database: %w", err)
	}
	if err := os.Rename(tmp, opts.Out); err != nil {
		return fmt.Errorf("backup: moving the restored database to %s: %w", opts.Out, err)
	}
	return nil
}

// readIdentities parses the age identity file. Comment lines and blank lines are
// skipped, which is what `age-keygen` writes above the key.
func readIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backup: opening the identity file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// The error deliberately says nothing about the file's contents: this is the
	// one place a private key passes through, and a parse error is not a place
	// to echo part of one.
	identities, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("backup: the identity file does not hold an age identity")
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("backup: the identity file holds no identity")
	}
	return identities, nil
}
