package source

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// rootDockerfile is the one entry name that selects the Dockerfile build path.
// It is the tree root and nothing else, because the extractor strips no prefix,
// so this is exactly what lands at the root of the unpacked workspace, which is
// the only path either build engine is ever pointed at.
const rootDockerfile = "Dockerfile"

// HasRootDockerfile reports whether a gzipped tar stream carries a regular file
// that unpacks to Dockerfile at the root of the tree.
//
// It keeps no body and writes nothing anywhere, and stops at both limits Extract
// enforces, so neither an archive of nothing but headers nor one whose bodies
// decompress without bound can run away with the control plane. The byte bound
// is not redundant with the entry one: tar.Reader discards the entry it is
// leaving on the way to the next header, so the bodies are decompressed here
// even though this never reads one. A stream it cannot read is refused with
// ErrRejected rather than answered, because the alternative is detecting one
// thing here and being refused for something else at extraction.
func HasRootDockerfile(r io.Reader, lim Limits) (bool, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return false, fmt.Errorf("source: reading the gzip stream: %w", errors.Join(ErrRejected, err))
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(&boundedReader{r: gz, left: lim.MaxBytes, max: lim.MaxBytes})
	var files int
	// Not an early return: the whole stream still has to prove it is readable
	// and inside the limit, or an archive that is refused at extraction would
	// have chosen an engine on its way there.
	found := false

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return false, fmt.Errorf("source: reading the archive: %w", errors.Join(ErrRejected, err))
		}

		files++
		if files > lim.MaxFiles {
			return false, fmt.Errorf("source: more than %d entries: %w", lim.MaxFiles, ErrRejected)
		}

		// Regular files only. A directory or a link whose name cleans to
		// Dockerfile is not a Dockerfile: the first holds no build instructions
		// and the second is refused at extraction, so either one selecting
		// BuildKit would send a tree with nothing to build to the wrong engine.
		if header.Typeflag == tar.TypeReg && filepath.Clean(header.Name) == rootDockerfile {
			found = true
		}
	}
}

// boundedReader fails the read once more than max uncompressed bytes have come
// out of it. It sits between the gzip reader and the tar reader so the bound is
// on what was decompressed, not on what arrived, which is the whole point.
type boundedReader struct {
	r    io.Reader
	left int64
	max  int64
}

// Read reads from the underlying reader, refusing once the bound is spent.
func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, fmt.Errorf("source: more than %d uncompressed bytes: %w", b.max, ErrRejected)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}
