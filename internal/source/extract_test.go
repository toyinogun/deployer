// Package source_test proves the extractor refuses rather than copes. Every
// hostile shape in spec 0004's critical test scenarios has a case here, and each
// asserts both that the archive is rejected and that nothing landed outside the
// extraction root.
package source_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/source"
)

// entry is one thing to put in a test archive.
type entry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64
}

// archive builds a gzipped tar out of the entries given.
func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Typeflag: flag,
			Linkname: e.linkname,
		}
		if flag != tar.TypeReg {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing a header: %v", err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing a body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}
	return buf.Bytes()
}

// generous limits, so a rejection in these tests is never the caps firing.
func generous() source.Limits { return source.Limits{MaxFiles: 1000, MaxBytes: 10 << 20} }

// countUnder returns every path under root, relative, so a test can assert that
// a refusal wrote nothing it should not have.
func countUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

func TestExtractUnpacksAPlainArchive(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()

	err := source.Extract(bytes.NewReader(archive(t,
		entry{name: "main.go", body: "package main\n"},
		entry{name: "cmd", typeflag: tar.TypeDir},
		entry{name: "cmd/run.sh", body: "#!/bin/sh\n", mode: 0o755},
	)), dest, generous())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dest, "main.go"))
	if err != nil || string(body) != "package main\n" {
		t.Fatalf("main.go = %q, %v", body, err)
	}
	// The execute bit survives, because a buildpack needs it on a script.
	info, err := os.Stat(filepath.Join(dest, "cmd", "run.sh"))
	if err != nil {
		t.Fatalf("stat run.sh: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh mode = %v, want the owner execute bit kept", info.Mode().Perm())
	}
}

// Every hostile archive in the spec's failure scenarios, each rejected, each
// leaving the extraction root empty (AC-8, AC-16).
func TestExtractRejectsHostileArchives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []entry
	}{
		{"an absolute path", []entry{{name: "/etc/passwd", body: "root:x:0:0\n"}}},
		{"a path climbing out", []entry{{name: "../../escape", body: "no"}}},
		{"a path climbing out mid way", []entry{{name: "src/../../escape", body: "no"}}},
		{"a symlink to the filesystem root", []entry{{name: "link", typeflag: tar.TypeSymlink, linkname: "/"}}},
		{"a symlink to a sensitive file", []entry{{name: "passwd", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}}},
		{"a hardlink", []entry{{name: "hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"}}},
		{"a character device", []entry{{name: "zero", typeflag: tar.TypeChar}}},
		{"a block device", []entry{{name: "disk", typeflag: tar.TypeBlock}}},
		{"a fifo", []entry{{name: "pipe", typeflag: tar.TypeFifo}}},
		{"a backslash separated path", []entry{{name: `..\..\escape`, body: "no"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dest := t.TempDir()

			err := source.Extract(bytes.NewReader(archive(t, tc.entries...)), dest, generous())

			if !errors.Is(err, source.ErrRejected) {
				t.Fatalf("want ErrRejected, got %v", err)
			}
			// Nothing outside the root, and nothing suspicious inside it either:
			// the only thing a rejected archive may leave is an entry it wrote
			// before the offending one, which none of these cases has.
			if left := countUnder(t, dest); len(left) != 0 {
				t.Errorf("the rejected archive left %v behind", left)
			}
		})
	}
}

// An archive naming the same path twice is refused rather than letting the
// later entry quietly replace the earlier one. The first entry is on disk by
// then, which is fine: the deployment fails and the workspace is thrown away.
func TestExtractRefusesADuplicatePath(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()

	err := source.Extract(bytes.NewReader(archive(t,
		entry{name: "dup", body: "first"},
		entry{name: "dup", body: "second"},
	)), dest, generous())

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	body, readErr := os.ReadFile(filepath.Join(dest, "dup"))
	if readErr != nil {
		t.Fatalf("reading dup: %v", readErr)
	}
	if string(body) != "first" {
		t.Errorf("dup = %q, want the second entry to have been refused, not applied", body)
	}
}

// A refusal stops the extraction rather than continuing past the bad entry.
func TestExtractStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()

	err := source.Extract(bytes.NewReader(archive(t,
		entry{name: "good.txt", body: "kept"},
		entry{name: "link", typeflag: tar.TypeSymlink, linkname: "/"},
		entry{name: "after.txt", body: "never written"},
	)), dest, generous())

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "after.txt")); err == nil {
		t.Error("the entry after the refusal was written, so extraction did not stop")
	}
}

func TestExtractRefusesTooManyEntries(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	var entries []entry
	for i := range 30 {
		entries = append(entries, entry{name: "f" + strings.Repeat("x", i), body: "."})
	}

	err := source.Extract(bytes.NewReader(archive(t, entries...)), dest,
		source.Limits{MaxFiles: 10, MaxBytes: 10 << 20})

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
}

// A gzip bomb: a small archive claiming to expand far past the cap. The write
// stops one byte over the budget rather than filling the volume.
func TestExtractRefusesAnOversizedExpansion(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	bomb := strings.Repeat("A", 1<<20)

	err := source.Extract(bytes.NewReader(archive(t, entry{name: "big", body: bomb})), dest,
		source.Limits{MaxFiles: 10, MaxBytes: 1024})

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	// The file may exist, but it must not hold more than the budget allowed.
	if info, statErr := os.Stat(filepath.Join(dest, "big")); statErr == nil && info.Size() > 1025 {
		t.Errorf("wrote %d bytes past a 1024 byte budget", info.Size())
	}
}

// Several files that individually fit but together do not.
func TestExtractRefusesAnOversizedTotal(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	chunk := strings.Repeat("B", 400)

	err := source.Extract(bytes.NewReader(archive(t,
		entry{name: "a", body: chunk},
		entry{name: "b", body: chunk},
		entry{name: "c", body: chunk},
	)), dest, source.Limits{MaxFiles: 10, MaxBytes: 1000})

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
}

func TestExtractRefusesSomethingThatIsNotGzip(t *testing.T) {
	t.Parallel()

	err := source.Extract(strings.NewReader("this is not a gzip stream"), t.TempDir(), generous())

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
}

func TestExtractRefusesATruncatedArchive(t *testing.T) {
	t.Parallel()
	full := archive(t, entry{name: "main.go", body: strings.Repeat("x", 4096)})

	err := source.Extract(bytes.NewReader(full[:len(full)/2]), t.TempDir(), generous())

	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
}
