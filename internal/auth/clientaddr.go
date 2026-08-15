package auth

import (
	"net/http"
	"strings"
)

// cfConnectingIP is the header Cloudflare writes the visitor's address into. It
// is trusted on exactly one hostname, for the reason ClientAddress explains.
const cfConnectingIP = "CF-Connecting-IP"

// ClientAddress is who a rate limit bucket belongs to and whose address an audit
// row records.
//
// It lives here rather than beside either surface because the page surface and
// the JSON surface must derive it identically: a limit one of them spends from a
// different bucket is not a limit (spec 0021, AC-16). Two copies of this
// function is how that property is lost, so there is one.
//
// consoleHost is the public console hostname, or empty on a platform with no
// public edge. On that host, and only on that host, CF-Connecting-IP is read,
// because that is the only hostname the tunnel serves and network policy is what
// makes that true: the console origin is the control plane Service directly, and
// only the tunnel namespace, the tailscale proxy and the two build namespaces may
// open a connection to it (AC-15a). Behind the shared ingress controller the same
// header would be writable by most of the cluster, which is why the console does
// not go through it.
//
// Everywhere else the derivation is unchanged: the last hop of X-Forwarded-For
// when the request came through a proxy, and the connection address on a direct
// call.
func ClientAddress(r *http.Request, consoleHost string) string {
	if consoleHost != "" && requestHost(r) == consoleHost {
		// Exactly one value, or none. Cloudflare sends one, so more than one
		// means something else wrote the header and the ordinary derivation is
		// used instead (AC-15b).
		//
		// Both shapes of "more than one" are refused, because they are different
		// mechanisms: two repeated headers arrive as two entries from Values, and
		// one header carrying "a, b" arrives as a single entry with a comma in
		// it. Values does not split on commas, so the comma check is not
		// redundant with the length check.
		if values := r.Header.Values(cfConnectingIP); len(values) == 1 {
			if addr := strings.TrimSpace(values[0]); addr != "" && !strings.Contains(addr, ",") {
				return addr
			}
		}
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		hops := strings.Split(fwd, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

// requestHost is the Host header without its port. A bare IPv6 literal keeps its
// brackets and its own colons, which is why this cuts at the closing bracket
// rather than at the first colon.
func requestHost(r *http.Request) string {
	host := r.Host
	if i := strings.LastIndex(host, "]"); i >= 0 {
		return host[:i+1]
	}
	if h, _, found := strings.Cut(host, ":"); found {
		return h
	}
	return host
}
