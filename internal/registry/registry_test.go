// Package registry_test drives the client against a stand in registry speaking
// the same HTTP the real one does. What is under test is how the platform reads
// a registry, so the registry itself is the one thing worth standing in for.
package registry_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/registry"
)

const (
	repo        = "apps/my-app-a1b2c3"
	imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cfgDigest   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// fakeRegistry answers the two reads the platform makes, and records whether it
// was asked with credentials.
type fakeRegistry struct {
	manifests  map[string]any // reference (tag or digest) to manifest body
	config     any            // the config blob, nil for none
	sawAuth    bool
	digestHead string
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if user, pass, ok := r.BasicAuth(); ok && user != "" && pass != "" {
		f.sawAuth = true
	} else {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case strings.Contains(r.URL.Path, "/manifests/"):
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := f.manifests[ref]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.digestHead != "" {
			w.Header().Set("Docker-Content-Digest", f.digestHead)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	case strings.Contains(r.URL.Path, "/blobs/"):
		if f.config == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.config)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serve starts a stand in registry and returns a client pointed at it.
func serve(t *testing.T, f *fakeRegistry) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return registry.New(strings.TrimPrefix(srv.URL, "http://"), "deployer", "s3cret")
}

// imageManifest is a plain single platform manifest naming a config blob.
func imageManifest() any {
	return map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config":    map[string]any{"digest": cfgDigest},
	}
}

// covers AC-9: the tag the build pushed resolves to the digest every deploy runs by.
func TestDigestResolvesTheTag(t *testing.T) {
	t.Parallel()
	f := &fakeRegistry{
		manifests:  map[string]any{"dep_123": imageManifest()},
		digestHead: imageDigest,
	}
	c := serve(t, f)

	got, err := c.Digest(t.Context(), repo, "dep_123")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	if got != imageDigest {
		t.Errorf("digest = %q, want %q", got, imageDigest)
	}
	if !f.sawAuth {
		t.Error("the registry was asked without credentials")
	}
}

// covers AC-9: a build that reported success and left no manifest is a failed
// build, reported as its own thing rather than as a mystery.
func TestDigestReportsAMissingTag(t *testing.T) {
	t.Parallel()
	c := serve(t, &fakeRegistry{manifests: map[string]any{}})

	_, err := c.Digest(t.Context(), repo, "dep_never_pushed")

	if !errors.Is(err, registry.ErrNoDigest) {
		t.Fatalf("want ErrNoDigest, got %v", err)
	}
}

// A registry that answers but names no digest is the same failure as one that
// has nothing: the platform has no image to deploy either way.
func TestDigestReportsAnEmptyDigestHeader(t *testing.T) {
	t.Parallel()
	c := serve(t, &fakeRegistry{manifests: map[string]any{"dep_123": imageManifest()}})

	_, err := c.Digest(t.Context(), repo, "dep_123")

	if !errors.Is(err, registry.ErrNoDigest) {
		t.Fatalf("want ErrNoDigest, got %v", err)
	}
}

// covers AC-10: the image's user is read from its config blob, which is what the
// non root check turns on.
func TestImageUserReadsTheConfigBlob(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		user string
		want string
	}{
		{"a named user", "cnb", "cnb"},
		{"a numeric non root user", "1000", "1000"},
		{"root by number", "0", "0"},
		{"root by name", "root", "root"},
		{"no user at all", "", ""},
		{"a user with stray whitespace", " 1000 ", "1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := serve(t, &fakeRegistry{
				manifests: map[string]any{imageDigest: imageManifest()},
				config:    map[string]any{"config": map[string]any{"User": tc.user}},
			})

			got, err := c.ImageUser(t.Context(), repo, imageDigest)
			if err != nil {
				t.Fatalf("image user: %v", err)
			}
			if got != tc.want {
				t.Errorf("user = %q, want %q", got, tc.want)
			}
		})
	}
}

// A multi platform index is followed one level down to the real manifest, so an
// image pushed as an index still gets checked rather than waved through.
func TestImageUserFollowsAnIndex(t *testing.T) {
	t.Parallel()
	c := serve(t, &fakeRegistry{
		manifests: map[string]any{
			imageDigest: map[string]any{
				"mediaType": "application/vnd.oci.image.index.v1+json",
				"manifests": []any{map[string]any{"digest": "sha256:child"}},
			},
			"sha256:child": imageManifest(),
		},
		config: map[string]any{"config": map[string]any{"User": "1000"}},
	})

	got, err := c.ImageUser(t.Context(), repo, imageDigest)
	if err != nil {
		t.Fatalf("image user: %v", err)
	}
	if got != "1000" {
		t.Errorf("user = %q, want 1000", got)
	}
}

func TestImageUserReportsAMissingManifest(t *testing.T) {
	t.Parallel()
	c := serve(t, &fakeRegistry{manifests: map[string]any{}})

	_, err := c.ImageUser(t.Context(), repo, imageDigest)

	if !errors.Is(err, registry.ErrNoDigest) {
		t.Fatalf("want ErrNoDigest, got %v", err)
	}
}

// A fully qualified repository is accepted as well as a bare one, because the
// caller composes it from the registry host and the slug together.
func TestDigestAcceptsAFullyQualifiedRepository(t *testing.T) {
	t.Parallel()
	f := &fakeRegistry{
		manifests:  map[string]any{"dep_123": imageManifest()},
		digestHead: imageDigest,
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	c := registry.New(host, "deployer", "s3cret")

	got, err := c.Digest(t.Context(), host+"/"+repo, "dep_123")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got != imageDigest {
		t.Errorf("digest = %q", got)
	}
}
