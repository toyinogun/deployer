// Package source unpacks an uploaded tarball onto a build's workspace. It is
// the one place in the platform that reads bytes an agent produced, so it is
// written to refuse rather than to cope: anything that is not a plain file or a
// plain directory inside the destination is a rejected archive, not a warning.
package source

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrRejected means the archive contained something the platform will not
// unpack. It is deliberately one error: the deployment fails with
// source_rejected either way, and telling a caller precisely which entry
// offended only helps somebody probing the extractor.
var ErrRejected = errors.New("source: archive rejected")

// Limits caps what an archive may expand into, so a small upload cannot fill a
// node.
type Limits struct {
	// MaxFiles is the total number of entries, files and directories together.
	MaxFiles int
	// MaxBytes is the total uncompressed size written.
	MaxBytes int64
}

// permMask keeps the read, write and execute bits and drops setuid, setgid and
// the sticky bit. Buildpacks need the execute bit on a script; nothing in a
// source tarball has any business carrying the others.
const permMask = 0o777

// Extract unpacks a gzipped tar stream into dest, which must already exist.
//
// Every entry is checked before anything is written, and the first refusal stops
// the whole extraction: a rejected archive never leaves a half unpacked tree for
// a builder to find. Nothing is ever written outside dest, whatever the archive
// claims its paths are.
func Extract(r io.Reader, dest string, lim Limits) error {
	root, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("source: resolving %s: %w", dest, err)
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("source: reading the gzip stream: %w", errors.Join(ErrRejected, err))
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var files int
	remaining := lim.MaxBytes

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("source: reading the archive: %w", errors.Join(ErrRejected, err))
		}

		files++
		if files > lim.MaxFiles {
			return fmt.Errorf("source: more than %d entries: %w", lim.MaxFiles, ErrRejected)
		}

		target, err := safePath(root, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&permMask|0o700); err != nil {
				return fmt.Errorf("source: creating a directory: %w", err)
			}
		case tar.TypeReg:
			written, err := writeFile(target, tr, os.FileMode(header.Mode)&permMask|0o600, remaining)
			if err != nil {
				return err
			}
			remaining -= written
		default:
			// Symlinks, hardlinks, devices, fifos and sockets, all refused. A
			// symlink is how an archive escapes its root after the path check
			// passed, and nothing else here has any legitimate use in source.
			return fmt.Errorf("source: entry type %d: %w", header.Typeflag, ErrRejected)
		}
	}
}

// safePath resolves an archive entry's name against root and refuses anything
// that is absolute, climbs out with .., or otherwise does not land inside root.
func safePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("source: absolute path %q: %w", name, ErrRejected)
	}
	// A Windows style volume or backslash separator is not something a source
	// tarball for a Linux build ever legitimately carries.
	if strings.ContainsAny(name, `\:`) {
		return "", fmt.Errorf("source: unusable path %q: %w", name, ErrRejected)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("source: path escapes the root: %w", ErrRejected)
		}
	}
	target := filepath.Join(root, filepath.Clean(name))
	// Belt and braces after the component check: prove the result is under root
	// rather than trusting Join and Clean to have agreed with the loop above.
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("source: path escapes the root: %w", ErrRejected)
	}
	return target, nil
}

// writeFile creates one file, refusing to write more than remaining bytes so a
// compression bomb stops one byte over the budget rather than filling the disk.
func writeFile(target string, r io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, fmt.Errorf("source: archive expands past the size limit: %w", ErrRejected)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, fmt.Errorf("source: creating a parent directory: %w", err)
	}
	// O_EXCL: an archive naming the same path twice is refused rather than
	// letting a later entry quietly replace an earlier one.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, fmt.Errorf("source: creating a file: %w", errors.Join(ErrRejected, err))
	}

	written, copyErr := io.Copy(f, io.LimitReader(r, remaining+1))
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		return 0, fmt.Errorf("source: writing a file: %w", copyErr)
	case closeErr != nil:
		return 0, fmt.Errorf("source: closing a file: %w", closeErr)
	case written > remaining:
		return 0, fmt.Errorf("source: archive expands past the size limit: %w", ErrRejected)
	}
	return written, nil
}
