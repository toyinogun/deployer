package auth

import (
	"net/http"
	"strings"
)

// cfConnectingIP is the header Cloudflare writes the visitor's address into. It
// is trusted on the platform's public hostnames only, for the reason
// ClientAddress explains.
const cfConnectingIP = "CF-Connecting-IP"

// ClientAddress is who a rate limit bucket belongs to and whose address an audit
// row records.
//
// It lives here rather than beside either surface because the page surface and
// the JSON surface must derive it identically: a limit one of them spends from a
// different bucket is not a limit (spec 0021, AC-16). Two copies of this
// function is how that property is lost, so there is one.
//
// trustedHosts are the platform's public hostnames, the console and the deploy
// host, or none on a platform with no public edge. On one of those hosts, and
// only there, CF-Connecting-IP is read, because those are the hostnames the
// tunnel serves and network policy is what makes that safe: each one's origin is
// the control plane Service directly, and only the tunnel namespace, the
// tailscale proxy and the two build namespaces may open a connection to it
// (spec 0021, AC-15a; spec 0022, AC-13). Behind the shared ingress controller the
// same header would be writable by most of the cluster, which is why neither
// public name goes through it.
//
// Every surface passes the same set, so the upload route, the MCP endpoint and
// the pages derive one address per visitor and spend from one bucket rather than
// three (spec 0022, AC-14).
//
// Everywhere else the derivation is unchanged: the last hop of X-Forwarded-For
// when the request came through a proxy, and the connection address on a direct
// call.
func ClientAddress(r *http.Request, trustedHosts ...string) string {
	if trusted(requestHost(r), trustedHosts) {
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

// trusted reports whether host is one of the platform's public hostnames. An
// empty entry never matches, so a platform with no public edge configured trusts
// the header nowhere rather than trusting it on a request with no Host.
func trusted(host string, hosts []string) bool {
	if host == "" {
		return false
	}
	for _, h := range hosts {
		if h != "" && h == host {
			return true
		}
	}
	return false
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
