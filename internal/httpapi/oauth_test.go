package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// AC-1. The two documents a client reads before it holds anything. Each names
// its own exact resource, because the client compares that against the address
// the person typed, which is why one document cannot serve both paths.
func TestTheDeployHostServesBothProtectedResourceDocuments(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	h := newHarness(t)

	for path, wantResource := range map[string]string{
		"/.well-known/oauth-protected-resource":     "https://" + testMCPHost,
		"/.well-known/oauth-protected-resource/mcp": "https://" + testMCPHost + "/mcp",
	} {
		rec := h.onHost(t, http.MethodGet, testMCPHost, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: got %d, want 200 with no credential", path, rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if doc["resource"] != wantResource {
			t.Errorf("%s names resource %v, want %q", path, doc["resource"], wantResource)
		}
		servers, _ := doc["authorization_servers"].([]any)
		if len(servers) != 1 || servers[0] != "https://"+testConsoleHost {
			t.Errorf("%s points at %v, want exactly the console", path, servers)
		}
		scopes, _ := doc["scopes_supported"].([]any)
		if len(scopes) != 1 || scopes[0] != "deploy" {
			t.Errorf("%s advertises scopes %v, want [\"deploy\"]", path, scopes)
		}
		methods, _ := doc["bearer_methods_supported"].([]any)
		if len(methods) != 1 || methods[0] != "header" {
			t.Errorf("%s advertises bearer methods %v, want [\"header\"]", path, methods)
		}
	}
}

// AC-25a, one half. The discovery documents belong to the deploy host and
// answer nowhere else: the console host's catch all takes them.
func TestTheProtectedResourceDocumentsAreAbsentFromTheConsoleHost(t *testing.T) {
	// covers: AC-25a
	t.Parallel()
	h := newHarness(t)
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		if rec := h.onHost(t, http.MethodGet, testConsoleHost, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on the console host = %d, want 404", path, rec.Code)
		}
	}
}
