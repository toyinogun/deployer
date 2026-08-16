package auth_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// mcpHost is the public deploy hostname, the second name the tunnel points
// straight at the control plane Service.
const mcpHost = "mcp.apps.example.org"

// TestCFConnectingIPIsReadOnTheTwoPublicHostsAndNoOther is spec 0022's AC-13.
//
// The header is trusted on the console host and the deploy host, and on nothing
// else. What makes that safe is the path rather than the header: each of those
// two names has the control plane Service as its tunnel origin, so network policy
// can name the tunnel namespace as the only outside peer on 8080. Every other
// host the platform can be reached on, the in cluster Service address included,
// falls back to the ordinary derivation.
func TestCFConnectingIPIsReadOnTheTwoPublicHostsAndNoOther(t *testing.T) {
	// covers: AC-13
	t.Parallel()
	trusted := []string{consoleHost, mcpHost}
	for _, tc := range []struct {
		name string
		host string
		want string
	}{
		{"on the console host it is used", consoleHost, "203.0.113.7"},
		{"on the deploy host it is used", mcpHost, "203.0.113.7"},
		{"on the deploy host with a port it is still used", mcpHost + ":8080", "203.0.113.7"},
		{"on the tailnet host it is ignored", "deployer.example.ts.net", "10.42.0.9"},
		{"on the in cluster Service address it is ignored", "deployer.deployer-system.svc", "10.42.0.9"},
		{"on a bare pod address it is ignored", "10.42.0.3:8080", "10.42.0.9"},
		{"on a lookalike of the deploy host it is ignored", mcpHost + ".evil.test", "10.42.0.9"},
		{"with no Host at all it is ignored", "", "10.42.0.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(tc.host, map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}})
			if tc.host == "" {
				r.Host = ""
			}
			if got := auth.ClientAddress(r, trusted...); got != tc.want {
				t.Errorf("ClientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoTrustedHostsMeansTheHeaderIsNeverRead is AC-13. A platform with no public
// edge configured passes an empty set, and an empty entry in that set must never
// match: trusting the header on a request with no Host would be exactly backwards.
func TestNoTrustedHostsMeansTheHeaderIsNeverRead(t *testing.T) {
	// covers: AC-13
	t.Parallel()
	r := request(consoleHost, map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}})

	if got := auth.ClientAddress(r); got != "10.42.0.9" {
		t.Errorf("ClientAddress = %q with no trusted hosts, want the connection address", got)
	}
	if got := auth.ClientAddress(r, "", ""); got != "10.42.0.9" {
		t.Errorf("ClientAddress = %q with only empty trusted hosts, want the connection address", got)
	}
}

// TestEverySurfaceDerivesOneAddressForOneVisitor is AC-14.
//
// The upload endpoint, the MCP endpoint and the pages all pass the same set to
// the same function, so one visitor is one bucket rather than one bucket per
// surface. A limit a second surface resets is not a limit, and the way that
// property is lost is a second copy of this derivation.
func TestEverySurfaceDerivesOneAddressForOneVisitor(t *testing.T) {
	// covers: AC-14
	t.Parallel()
	trusted := []string{consoleHost, mcpHost}
	header := map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}}

	// The same visitor, arriving on the deploy host for an upload and for an MCP
	// call, and on the console for a page.
	upload := auth.ClientAddress(request(mcpHost, header), trusted...)
	tools := auth.ClientAddress(request(mcpHost, header), trusted...)
	page := auth.ClientAddress(request(consoleHost, header), trusted...)

	if upload != tools || upload != page {
		t.Errorf("one visitor derived three addresses: upload %q, mcp %q, page %q: "+
			"they would spend from three buckets rather than one", upload, tools, page)
	}
	if upload != "203.0.113.7" {
		t.Errorf("ClientAddress = %q, want the visitor rather than the tunnel", upload)
	}
}
