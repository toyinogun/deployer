package identity

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// The bounds this flow holds. They are constants here rather than DEPLOYER_*
// configuration for the same reason the session and link lifetimes are: they are
// product decisions about how a connection behaves, not something an operator
// tunes per cluster.
const (
	// CodeLifetime is how long an authorization code is worth presenting. The
	// exchange happens within a second of the redirect in every real client, so
	// a minute is generous already (AC-16a).
	CodeLifetime = 60 * time.Second

	// ClientRetention is how long a client nobody approved stays resumable
	// before the daily sweep deletes it (AC-8). Seven days rather than one
	// removes the case where a person leaves the approval page open overnight,
	// presses the button, and is told their client is unknown, which reads
	// exactly like a spoofed request.
	ClientRetention = 7 * 24 * time.Hour

	// MaxRedirectURIs is how many a registration may carry. Claude Code
	// declares two; ten leaves room without letting a stranger store a list.
	MaxRedirectURIs = 10

	// MaxClientNameLen bounds the name a client claims for itself, in runes.
	// The name is attacker supplied text that ends up on the approval page and
	// in the token list, so it is bounded before it is stored (AC-4a, AC-20a).
	MaxClientNameLen = 64

	// ConnectorScope is the single scope this authorization server knows.
	ConnectorScope = "deploy"

	// ProtectedResourcePath is where the deploy host serves its RFC 9728
	// document. It is a constant here rather than a literal in two packages
	// because one package serves it and another points at it in a
	// WWW-Authenticate header, and the two disagreeing is a client that
	// discovers nothing (AC-1, AC-2).
	ProtectedResourcePath = "/.well-known/oauth-protected-resource"

	// AuthorizationServerPath is where the console host serves its RFC 8414
	// document (AC-3).
	AuthorizationServerPath = "/.well-known/oauth-authorization-server"

	// The three machine endpoints, on the console host. They are namespaced
	// under /oauth/ because POST /register is already the account signup form
	// there, and all three are advertised in the AC-3 document, so no client
	// hardcodes one (AC-25).
	RegisterPath  = "/oauth/register"
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
)

// ConnectorSettings is the rate limit the OAuth endpoints spend from. It is
// deliberately not SignInSettings: adding one connector spends the bucket three
// times in a row from one address, so sharing the sign in bucket would let a
// person adding a connector lock themselves out of the console they are signing
// in to (AC-6, AC-22).
//
// The lockout half is unused here, because none of these endpoints judges a
// credential, so the numbers that drive it are the sign in ones and never fire.
func ConnectorSettings() Settings {
	return Settings{
		BucketCapacity:        20,
		BucketRefill:          3 * time.Second,
		FailuresBeforeLockout: 5,
		LockoutBase:           30 * time.Second,
		LockoutCeiling:        15 * time.Minute,
	}
}

// CheckRedirectURI reports whether a client may register this redirect URI.
//
// An absolute URI with no fragment that is either https, or http on a loopback
// host. RFC 8252 names all three loopback literals, so refusing the IPv6 one
// would turn away a conforming client for no reason (AC-5).
func CheckRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	default:
		return false
	}
}

// isLoopbackHost is the closed set of hosts http is allowed on.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// MatchRedirectURI reports whether a requested redirect URI is one this client
// registered. This comparison is the whole open redirect defence, so what it
// does is pinned rather than left to a reader's judgement (AC-10b).
//
// The two sides arrive differently: requested is the already percent decoded
// query parameter as net/url yields it, and registered is the string the
// registration supplied, kept verbatim. They are compared with plain string
// equality and no normalization at all: no case folding, no trailing slash
// handling, no default port elision, no re encoding.
//
// The single exception is the loopback port. RFC 8252 has a native client take
// whatever ephemeral port it can get, so a registered loopback URI matches with
// the port ignored while its scheme, host and path must still be equal (AC-10a).
func MatchRedirectURI(registered []string, requested string) (string, bool) {
	for _, candidate := range registered {
		if candidate == requested {
			return candidate, true
		}
		if loopbackMatch(candidate, requested) {
			return candidate, true
		}
	}
	return "", false
}

// loopbackMatch is the one relaxation MatchRedirectURI allows, and it relaxes
// nothing but the port.
func loopbackMatch(registered, requested string) bool {
	reg, err := url.Parse(registered)
	if err != nil || reg.Scheme != "http" || !isLoopbackHost(reg.Hostname()) {
		return false
	}
	req, err := url.Parse(requested)
	if err != nil {
		return false
	}
	return req.Scheme == reg.Scheme &&
		req.Hostname() == reg.Hostname() &&
		req.Path == reg.Path &&
		req.RawQuery == reg.RawQuery &&
		req.Fragment == "" && reg.Fragment == ""
}

// AllLoopback reports whether every URI this client registered is a loopback
// address, which is what puts the extra warning on the approval page. A request
// from a program on the person's own machine is one the platform cannot
// attribute to any particular program (AC-13a).
func AllLoopback(registered []string) bool {
	if len(registered) == 0 {
		return false
	}
	for _, raw := range registered {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
			return false
		}
	}
	return true
}

// S256Challenge is the PKCE transformation: base64url of the SHA-256 of the
// verifier, unpadded, exactly as RFC 7636 defines it.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE reports whether this verifier produces this challenge. The compare
// is constant time because the challenge is stored and the verifier is
// presented, which is the shape a timing oracle likes.
func VerifyPKCE(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	got := S256Challenge(verifier)
	return subtle.ConstantTimeCompare([]byte(challenge), []byte(got)) == 1
}

// CleanClientName bounds and cleans the name a client claimed for itself, so
// what reaches a token row is short, printable, and single line. It does not
// escape anything: escaping belongs to whatever renders it, and doing both is
// how text ends up double escaped in one of the two places (AC-20a).
//
// An empty result means the client offered nothing usable, and the caller
// supplies its own label rather than storing a blank.
func CleanClientName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Dropped outright: there is no sensible printable stand in.
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(cleaned)
	if len(runes) > MaxClientNameLen {
		cleaned = strings.TrimSpace(string(runes[:MaxClientNameLen]))
	}
	return cleaned
}
