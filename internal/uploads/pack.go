package uploads

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// ErrBadPath means a caller named a file this package will not pack: an empty
// set, an empty name, an absolute one, or one that climbs out of the tree it is
// packed into.
var ErrBadPath = errors.New("uploads: unusable file path")

// Pack composes a gzipped tarball from a set of text files, keyed by their path
// relative to the app's root. It exists so a caller with no shell can hand its
// source straight to deploy_app: an agent that just wrote an app already holds
// it as text, and the alternative is a plain HTTP upload it cannot make.
//
// Nothing here writes to disk and nothing is bounded here either. The result is
// a reader for Accept, which applies the same size cap and the same unclaimed
// cap the upload endpoint does, so this path costs the volume no more than that
// one does.
//
// Entries are regular files only, mode 0644, with no directory entries: the
// extractor in internal/source creates each file's parent itself, and a tar
// carrying no directories cannot carry a directory's permissions either.
func Pack(files map[string]string) (io.Reader, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no files", ErrBadPath)
	}

	// Every path is judged before a byte is written, and the entries are sorted,
	// so the same set of files packs to the same bytes every time.
	packed := make(map[string]string, len(files))
	names := make([]string, 0, len(files))
	for name, body := range files {
		clean, err := packPath(name)
		if err != nil {
			return nil, err
		}
		packed[clean] = body
		names = append(names, clean)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, clean := range names {
		body := packed[clean]
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     clean,
			Mode:     0o644,
			Size:     int64(len(body)),
		}); err != nil {
			return nil, fmt.Errorf("uploads: writing the header for %s: %w", clean, err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			return nil, fmt.Errorf("uploads: writing %s: %w", clean, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("uploads: closing the archive: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("uploads: closing the gzip stream: %w", err)
	}
	return &buf, nil
}

// packPath is the one rule about what a caller may name. It refuses here as
// well as at extraction, deliberately: the extractor's refusal ends a
// deployment the platform has already spent a build pod on, and this one is a
// straight answer to the call that made the mistake.
func packPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: %q", ErrBadPath, name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return "", fmt.Errorf("%w: %q", ErrBadPath, name)
	}
	return clean, nil
}
