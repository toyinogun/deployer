package identity_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/identity"
)

func TestARedirectURIIsAcceptedOnlyWhenItIsHTTPSOrLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		uri  string
		want bool
	}{
		{"an https address", "https://example.org/callback", true},
		{"https with a port", "https://example.org:8443/callback", true},
		{"https with a query", "https://example.org/cb?x=1", true},
		{"loopback by name", "http://localhost/callback", true},
		{"loopback by name with a port", "http://localhost:3118/callback", true},
		{"loopback v4", "http://127.0.0.1:1455/callback", true},
		{"loopback v6", "http://[::1]:1455/callback", true},
		{"plain http elsewhere", "http://example.org/callback", false},
		{"http on a lookalike host", "http://localhost.evil.org/cb", false},
		{"http on another private address", "http://192.168.1.5/cb", false},
		{"a relative reference", "/callback", false},
		{"a scheme this platform does not speak", "javascript:alert(1)", false},
		{"a custom scheme", "myapp://callback", false},
		{"a fragment", "https://example.org/cb#frag", false},
		{"a bare fragment marker", "https://example.org/cb#", false},
		{"nothing at all", "", false},
		{"https with no host", "https:///callback", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := identity.CheckRedirectURI(tc.uri); got != tc.want {
				t.Errorf("CheckRedirectURI(%q) = %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}

// The comparison in AC-10b is the whole open redirect defence, so every shape
// that a normalizing implementation would wave through is asserted refused.
func TestAnHTTPSRedirectURIMatchesOnlyItselfExactly(t *testing.T) {
	t.Parallel()
	registered := []string{"https://example.org/callback"}
	cases := []struct {
		name      string
		requested string
		want      bool
	}{
		{"the exact string", "https://example.org/callback", true},
		{"percent encoded", "https%3A%2F%2Fexample.org%2Fcallback", false},
		{"an uppercased host", "https://EXAMPLE.ORG/callback", false},
		{"a trailing slash", "https://example.org/callback/", false},
		{"the default port spelled out", "https://example.org:443/callback", false},
		{"an added query", "https://example.org/callback?next=x", false},
		{"a different path", "https://example.org/other", false},
		{"a different host", "https://evil.org/callback", false},
		{"a userinfo prefix", "https://example.org@evil.org/callback", false},
		{"an added fragment", "https://example.org/callback#x", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			matched, ok := identity.MatchRedirectURI(registered, tc.requested)
			if ok != tc.want {
				t.Errorf("MatchRedirectURI(%q) ok = %v, want %v", tc.requested, ok, tc.want)
			}
			if ok && matched != registered[0] {
				t.Errorf("matched %q, want the registered string %q", matched, registered[0])
			}
		})
	}
}

// The one relaxation, and it relaxes nothing but the port (AC-10a).
func TestALoopbackRedirectURIMatchesOnAnyPortAndNothingElse(t *testing.T) {
	t.Parallel()
	registered := []string{"http://localhost/callback", "http://127.0.0.1/callback", "http://[::1]/callback"}
	cases := []struct {
		name      string
		requested string
		want      bool
	}{
		{"the registered form", "http://localhost/callback", true},
		{"an ephemeral port", "http://localhost:54321/callback", true},
		{"the v4 literal with a port", "http://127.0.0.1:1455/callback", true},
		{"the v6 literal with a port", "http://[::1]:1455/callback", true},
		{"a different path", "http://localhost:54321/other", false},
		{"a different loopback spelling than any registered", "http://0.0.0.0:1455/callback", false},
		{"https instead of http", "https://localhost/callback", false},
		{"an added query", "http://localhost:54321/callback?x=1", false},
		{"an added fragment", "http://localhost:54321/callback#x", false},
		{"a lookalike host", "http://localhost.evil.org:80/callback", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := identity.MatchRedirectURI(registered, tc.requested); ok != tc.want {
				t.Errorf("MatchRedirectURI(%q) = %v, want %v", tc.requested, ok, tc.want)
			}
		})
	}
}

// What the match returns is what the platform redirects to and what it stores on
// the code, so for a loopback client it has to be the address the request came
// with, port and all. Returning the registered form instead sends the code to
// port 80, where the client is not listening, and then refuses the exchange
// because the client presents its own port against a stored value without one
// (AC-10a, AC-18).
func TestALoopbackMatchReturnsTheRequestedAddressWithItsPort(t *testing.T) {
	t.Parallel()
	registered := []string{"http://localhost/callback", "http://127.0.0.1/callback"}
	cases := []struct {
		name      string
		requested string
	}{
		{"an ephemeral port", "http://localhost:54321/callback"},
		{"the v4 literal with a port", "http://127.0.0.1:1455/callback"},
		{"the registered form itself", "http://localhost/callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			matched, ok := identity.MatchRedirectURI(registered, tc.requested)
			if !ok {
				t.Fatalf("MatchRedirectURI(%q) did not match", tc.requested)
			}
			if matched != tc.requested {
				t.Errorf("matched %q, want the requested address %q", matched, tc.requested)
			}
		})
	}
}

// The port relaxation must not leak onto an https registration, or a registered
// https URI would match the same host on any port at all.
func TestTheLoopbackPortRelaxationDoesNotApplyToHTTPS(t *testing.T) {
	t.Parallel()
	if _, ok := identity.MatchRedirectURI([]string{"https://example.org/cb"}, "https://example.org:8443/cb"); ok {
		t.Error("an https registration matched a different port")
	}
}

func TestTheLoopbackWarningShowsOnlyWhenEveryRegisteredURIIsLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		uris []string
		want bool
	}{
		{"all three loopback literals", []string{"http://localhost/cb", "http://127.0.0.1/cb", "http://[::1]/cb"}, true},
		{"one loopback", []string{"http://localhost:1455/cb"}, true},
		{"mixed with an https one", []string{"http://localhost/cb", "https://example.org/cb"}, false},
		{"https only", []string{"https://example.org/cb"}, false},
		{"none registered", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := identity.AllLoopback(tc.uris); got != tc.want {
				t.Errorf("AllLoopback(%v) = %v, want %v", tc.uris, got, tc.want)
			}
		})
	}
}

func TestPKCEVerifiesOnlyTheVerifierThatProducedTheChallenge(t *testing.T) {
	t.Parallel()
	// The worked example from RFC 7636 appendix B, so this pins the platform
	// against the specification rather than against its own implementation.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	if got := identity.S256Challenge(verifier); got != challenge {
		t.Fatalf("S256Challenge = %q, want the RFC 7636 value %q", got, challenge)
	}
	if !identity.VerifyPKCE(challenge, verifier) {
		t.Error("the verifier that produced the challenge did not verify")
	}
	if identity.VerifyPKCE(challenge, verifier+"x") {
		t.Error("a wrong verifier verified")
	}
	if identity.VerifyPKCE(challenge, "") {
		t.Error("an empty verifier verified")
	}
	if identity.VerifyPKCE("", verifier) {
		t.Error("an empty challenge verified")
	}
	// A client that sent the plain verifier as its own challenge must not be
	// able to present that verifier back and pass.
	if identity.VerifyPKCE(verifier, verifier) {
		t.Error("a plain challenge verified against itself")
	}
}

func TestAClientNameIsBoundedAndStrippedBeforeItIsStored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an ordinary name", "Claude Desktop", "Claude Desktop"},
		{"markup is left for the renderer to escape", `<b>x</b>`, `<b>x</b>`},
		{"control characters go", "Cla\x00ude\x07", "Claude"},
		{"newlines collapse to one space", "Claude\nDesktop\r\nApp", "Claude Desktop App"},
		{"tabs collapse", "a\t\t\tb", "a b"},
		{"surrounding space goes", "   spaced   ", "spaced"},
		{"nothing usable", "\x00\x01", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := identity.CleanClientName(tc.in); got != tc.want {
				t.Errorf("CleanClientName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestALongClientNameIsTruncatedToTheBound(t *testing.T) {
	t.Parallel()
	got := identity.CleanClientName(strings.Repeat("a", identity.MaxClientNameLen+50))
	if len([]rune(got)) != identity.MaxClientNameLen {
		t.Errorf("a long name came out %d runes, want %d", len([]rune(got)), identity.MaxClientNameLen)
	}
	// Counted in runes rather than bytes, so a multi byte name is not cut in
	// half through a character.
	wide := identity.CleanClientName(strings.Repeat("é", identity.MaxClientNameLen+10))
	if len([]rune(wide)) != identity.MaxClientNameLen {
		t.Errorf("a multi byte name came out %d runes, want %d", len([]rune(wide)), identity.MaxClientNameLen)
	}
}

// The connector bucket must never be the sign in one, or adding a connector
// spends a person's sign in allowance (AC-6, AC-22).
func TestTheConnectorLimiterIsNotTheSignInOne(t *testing.T) {
	t.Parallel()
	connector := identity.ConnectorSettings()
	signIn := identity.SignInSettings()
	if connector == signIn {
		t.Fatal("the connector settings are the sign in settings")
	}
	// One connector being added spends the bucket at least three times in a row
	// from one address, so the capacity has to leave room for several.
	if connector.BucketCapacity < signIn.BucketCapacity {
		t.Errorf("the connector bucket holds %v, less than the sign in bucket's %v",
			connector.BucketCapacity, signIn.BucketCapacity)
	}
}
