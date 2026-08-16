package uploads_test

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"testing"

	"github.com/toyinogun/deployer/internal/uploads"
)

func TestPackWritesEveryFileAsAGzippedTarball(t *testing.T) {
	t.Parallel()

	r, err := uploads.Pack(map[string]string{
		"Dockerfile":     "FROM scratch\n",
		"src/index.html": "<!doctype html>",
	})
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}

	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	got := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Errorf("entry %q has type %v, want a regular file", h.Name, h.Typeflag)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		got[h.Name] = string(body)
	}

	if len(got) != 2 || got["Dockerfile"] != "FROM scratch\n" || got["src/index.html"] != "<!doctype html>" {
		t.Errorf("archive holds %v", got)
	}
}

func TestPackRefusesAPathThatCouldEscapeTheTree(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		"/etc/passwd",
		"../outside",
		"src/../../outside",
		"./",
		"with\x00nul",
	} {
		if _, err := uploads.Pack(map[string]string{name: "x"}); !errors.Is(err, uploads.ErrBadPath) {
			t.Errorf("Pack(%q) error = %v, want ErrBadPath", name, err)
		}
	}
}

func TestPackRefusesAnEmptySet(t *testing.T) {
	t.Parallel()

	if _, err := uploads.Pack(nil); !errors.Is(err, uploads.ErrBadPath) {
		t.Errorf("Pack(nil) error = %v, want ErrBadPath", err)
	}
}
