package httpapi

import (
	"net/http"

	"github.com/toyinogun/deployer/internal/identity"
)

// protectedResource is the RFC 9728 document a client reads to find out where to
// sign in. It needs no credential: a caller that cannot authenticate is exactly
// the caller this is for (spec 0024, AC-1).
//
// resource has to be the exact address the path describes, because the client
// compares it against what the person typed. That is why there are two
// documents rather than one: /.well-known/oauth-protected-resource describes the
// host, and /.well-known/oauth-protected-resource/mcp describes the endpoint.
type protectedResource struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// resourceDocument answers the document describing the deploy host itself.
func (a *API) resourceDocument(w http.ResponseWriter, r *http.Request) {
	a.writeResource(w, r, a.opts.MCPURL)
}

// mcpResourceDocument answers the document describing the MCP endpoint. This is
// the one the WWW-Authenticate header on a 401 points at.
func (a *API) mcpResourceDocument(w http.ResponseWriter, r *http.Request) {
	a.writeResource(w, r, a.opts.MCPURL+"/mcp")
}

// writeResource composes one document. authorization_servers holds exactly one
// entry, the console, because a client uses the first and does not fall back.
func (a *API) writeResource(w http.ResponseWriter, r *http.Request, resource string) {
	writeJSON(r.Context(), w, http.StatusOK, protectedResource{
		Resource:               resource,
		AuthorizationServers:   []string{a.opts.ConsoleURL},
		ScopesSupported:        []string{identity.ConnectorScope},
		BearerMethodsSupported: []string{"header"},
	})
}
