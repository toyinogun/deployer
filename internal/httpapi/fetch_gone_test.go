package httpapi_test

import (
	"net/http"
	"testing"
)

// The loop deletes a tarball the moment its deployment reaches a terminal
// state, so a build that was already running when its deployment was cancelled
// or timed out arrives here to find nothing. That is the platform working, and
// it reads the same as an upload that expired.
func TestFetchUploadIsGoneOnceItsFileWasRemoved(t *testing.T) {
	h := newHarness(t)
	id := h.upload(t)
	token, err := h.uploads.MintFetchToken(t.Context(), id)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	up, err := h.uploads.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("reading the upload: %v", err)
	}
	h.uploads.Remove(t.Context(), up.Path)

	rec := h.fetch(t, id, token)

	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", rec.Code)
	}
}
