package source_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/source"
)

// TestHasRootDockerfileMatchesTheTreeRootOnly walks the shapes an agent's
// archive can actually take. The rule is deliberately narrow: the entry that
// lands at the root of the unpacked tree, and nothing else, because that is the
// only path either build engine is ever pointed at.
func TestHasRootDockerfileMatchesTheTreeRootOnly(t *testing.T) {
	cases := []struct {
		name    string
		entries []entry
		want    bool
	}{
		{
			name:    "a Dockerfile at the root",
			entries: []entry{{name: "go.mod", body: "module x\n"}, {name: "Dockerfile", body: "FROM scratch\n"}},
			want:    true,
		},
		{
			// The extractor strips no prefix, so "./Dockerfile" is the same
			// entry written a different way and has to answer the same.
			name:    "the same file written as ./Dockerfile",
			entries: []entry{{name: "./Dockerfile", body: "FROM scratch\n"}},
			want:    true,
		},
		{
			// This one lands at api/Dockerfile, which neither engine is aimed
			// at, so the tree goes to Buildpacks exactly as it does today.
			name:    "a Dockerfile one directory down",
			entries: []entry{{name: "api/Dockerfile", body: "FROM scratch\n"}},
			want:    false,
		},
		{
			name:    "a lowercase dockerfile",
			entries: []entry{{name: "dockerfile", body: "FROM scratch\n"}},
			want:    false,
		},
		{
			name:    "a suffixed Dockerfile.dev",
			entries: []entry{{name: "Dockerfile.dev", body: "FROM scratch\n"}},
			want:    false,
		},
		{
			// A directory named Dockerfile cleans to the same string. Letting it
			// select the Dockerfile path would send an archive holding no
			// Dockerfile to BuildKit, to fail confusingly there instead of
			// building fine through Buildpacks (AC-7a).
			name:    "a directory named Dockerfile",
			entries: []entry{{name: "Dockerfile/", typeflag: tar.TypeDir}, {name: "Dockerfile/notes", body: "x"}},
			want:    false,
		},
		{
			// Same reasoning for a link: it is refused at extraction time, so it
			// must not choose an engine before it gets there.
			name:    "a symlink named Dockerfile",
			entries: []entry{{name: "Dockerfile", typeflag: tar.TypeSymlink, linkname: "api/Dockerfile"}},
			want:    false,
		},
		{
			name:    "no Dockerfile anywhere",
			entries: []entry{{name: "main.go", body: "package main\n"}},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := source.HasRootDockerfile(bytes.NewReader(archive(t, tc.entries...)), generous())
			if err != nil {
				t.Fatalf("detecting: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasRootDockerfile = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasRootDockerfileRefusesWhatTheExtractorWouldRefuse keeps the two readers
// of the same stream agreeing. A stream this walk cannot read fails the
// deployment as source_rejected here, before a Job or a credential exists,
// rather than being detected as one thing and refused later for another (AC-6).
func TestHasRootDockerfileRefusesWhatTheExtractorWouldRefuse(t *testing.T) {
	full := archive(t, entry{name: "main.go", body: "package main\n"})

	cases := []struct {
		name string
		body []byte
	}{
		{name: "not gzip at all", body: []byte("this is not a gzip stream")},
		{name: "a truncated archive", body: full[:len(full)/2]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := source.HasRootDockerfile(bytes.NewReader(tc.body), generous())
			if !errors.Is(err, source.ErrRejected) {
				t.Fatalf("error = %v, want it to wrap ErrRejected", err)
			}
		})
	}
}

// TestHasRootDockerfileStopsAtTheEntryLimit is the one that matters for the
// control plane specifically. Headers are cheap to write and there is no body to
// bound, so an archive of nothing but headers is small on disk and long to walk.
// It stops at the same limit the extractor enforces, not a second number (AC-3).
func TestHasRootDockerfileStopsAtTheEntryLimit(t *testing.T) {
	const limit = 50
	entries := make([]entry, 0, limit*4)
	for i := range cap(entries) {
		entries = append(entries, entry{name: fmt.Sprintf("f%d", i)})
	}
	// The Dockerfile sits past the limit, so a walk that ran to the end would
	// answer true instead of refusing.
	entries = append(entries, entry{name: "Dockerfile", body: "FROM scratch\n"})

	got, err := source.HasRootDockerfile(
		bytes.NewReader(archive(t, entries...)),
		source.Limits{MaxFiles: limit, MaxBytes: 10 << 20},
	)
	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("error = %v, want it to wrap ErrRejected", err)
	}
	if got {
		t.Fatal("a refused archive must not also report a Dockerfile")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(limit)) {
		t.Fatalf("error %q should name the limit it stopped at", err)
	}
}

// TestHasRootDockerfileStopsAtTheByteLimit pins the bound the entry count does
// not cover. Detection reads no body itself, but tar.Reader discards the
// previous entry on the way to the next header, so the bodies are decompressed
// either way: a handful of entries of highly compressible zeroes is tiny on the
// wire and unbounded coming out of gzip, inside the control plane pod. It stops
// at the same MaxBytes the extractor enforces (AC-3).
func TestHasRootDockerfileStopsAtTheByteLimit(t *testing.T) {
	const limit = 64 << 10
	// Well inside MaxFiles, so only the byte bound can stop this one.
	entries := []entry{
		{name: "zeroes", body: strings.Repeat("\x00", limit*4)},
		{name: "Dockerfile", body: "FROM scratch\n"},
	}

	got, err := source.HasRootDockerfile(
		bytes.NewReader(archive(t, entries...)),
		source.Limits{MaxFiles: 100, MaxBytes: limit},
	)
	if !errors.Is(err, source.ErrRejected) {
		t.Fatalf("error = %v, want it to wrap ErrRejected", err)
	}
	if got {
		t.Fatal("a refused archive must not also report a Dockerfile")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(limit)) {
		t.Fatalf("error %q should name the limit it stopped at", err)
	}
}
