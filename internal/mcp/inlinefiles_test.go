package mcp_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// TestDeployAcceptsSourceCarriedInline is the whole point of the files argument:
// a caller with no shell, and so no way to make the upload request, deploys in
// one tool call. It runs over a real client and server session, because the
// argument schema is what a handler test would never cross.
func TestDeployAcceptsSourceCarriedInline(t *testing.T) {
	t.Parallel()
	h := newOwnershipHarness(t)

	out, said, refused := h.call(t, h.a, "deploy_app", map[string]any{
		"name": "inline",
		"files": map[string]any{
			"Dockerfile": "FROM nginxinc/nginx-unprivileged:1.29-alpine\nCOPY . /usr/share/nginx/html\n",
			"index.html": "<!doctype html><title>hi</title>",
		},
	})
	if refused {
		t.Fatalf("the deploy was refused: %s", said)
	}
	if out["state"] != string(domain.StateQueued) || out["deployment_id"] == "" {
		t.Errorf("deploy answered %v, want a queued deployment", out)
	}

	// The deployment names a real upload: the row's foreign key is what would
	// refuse an id the platform never recorded.
	if _, said, refused := h.call(t, h.a, "deployment_status",
		map[string]any{"deployment_id": out["deployment_id"]}); refused {
		t.Errorf("the recorded deployment does not read back: %s", said)
	}
}

// TestDeployRefusesTheTwoWaysOfNamingNoSource is one rule, stated once: exactly
// one of upload_id and files. Both is as unusable as neither, because the two
// name different trees and nothing here would choose between them.
func TestDeployRefusesTheTwoWaysOfNamingNoSource(t *testing.T) {
	t.Parallel()
	h := newOwnershipHarness(t)

	cases := map[string]map[string]any{
		"neither": {"name": "nothing"},
		"both": {
			"name":      "everything",
			"upload_id": h.upload(t, h.a),
			"files":     map[string]any{"Dockerfile": "FROM scratch\n"},
		},
		"a path that climbs out of the tree": {
			"name":  "escape",
			"files": map[string]any{"../etc/passwd": "root"},
		},
		"an empty set": {
			"name":  "empty",
			"files": map[string]any{},
		},
	}
	for name, args := range cases {
		_, said, refused := h.call(t, h.a, "deploy_app", args)
		if !refused || !strings.Contains(said, string(domain.ReasonUploadInvalid)) {
			t.Errorf("%s: refused=%v said=%q, want %s", name, refused, said, domain.ReasonUploadInvalid)
		}
	}
}

// TestInlineSourceIsBoundedByTheSameCeiling proves the inline path is a second
// way to reach the upload service rather than a way around it: the harness caps
// an upload at 4096 bytes, and a larger set of files is refused in the endpoint's
// own words.
func TestInlineSourceIsBoundedByTheSameCeiling(t *testing.T) {
	t.Parallel()
	h := newOwnershipHarness(t)

	// Pseudo random from a fixed seed, so the bytes are both incompressible and
	// the same on every run: a pattern here would gzip down under the cap and the
	// test would prove nothing.
	big := make([]byte, 64<<10)
	rng := rand.New(rand.NewSource(1))
	for i := range big {
		big[i] = byte(rng.Intn(256))
	}

	_, said, refused := h.call(t, h.a, "deploy_app", map[string]any{
		"name":  "heavy",
		"files": map[string]any{"blob.bin": string(big)},
	})
	if !refused || !strings.Contains(said, string(domain.ReasonUploadTooLarge)) {
		t.Errorf("refused=%v said=%q, want %s", refused, said, domain.ReasonUploadTooLarge)
	}
}
