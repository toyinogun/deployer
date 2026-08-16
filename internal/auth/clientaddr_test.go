package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

const consoleHost = "console.apps.example.org"

// request builds one request on a host, with whatever headers a case needs.
func request(host string, headers map[string][]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/login", nil)
	r.Host = host
	r.RemoteAddr = "10.42.0.9:44321"
	for name, values := range headers {
		for _, v := range values {
			r.Header.Add(name, v)
		}
	}
	return r
}

// TestCFConnectingIPIsReadOnlyOnTheConsoleHost is AC-15. The header is trusted on
// exactly one hostname, because that is the only one the tunnel serves and
// network policy is what makes that true.
func TestCFConnectingIPIsReadOnlyOnTheConsoleHost(t *testing.T) {
	// covers: AC-15
	t.Parallel()
	for _, tc := range []struct {
		name string
		host string
		want string
	}{
		{"on the console host it is used", consoleHost, "203.0.113.7"},
		{"on the console host with a port it is still used", consoleHost + ":8080", "203.0.113.7"},
		{"on the tailnet host it is ignored", "deployer.example.ts.net", "10.42.0.9"},
		{"on any other host it is ignored", "10.42.0.3:8080", "10.42.0.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := request(tc.host, map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}})
			if got := auth.ClientAddress(r, consoleHost); got != tc.want {
				t.Errorf("ClientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMoreThanOneCFConnectingIPIsTreatedAsAbsent is AC-15b. Cloudflare sends
// exactly one, so more than one means something else wrote the header and the
// ordinary derivation is used instead.
func TestMoreThanOneCFConnectingIPIsTreatedAsAbsent(t *testing.T) {
	// covers: AC-15b
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{"two separate headers fall back", []string{"203.0.113.7", "198.51.100.1"}, "10.42.0.9"},
		{"one header carrying two values falls back", []string{"203.0.113.7, 198.51.100.1"}, "10.42.0.9"},
		{"an empty value falls back", []string{""}, "10.42.0.9"},
		{"exactly one is used", []string{"203.0.113.7"}, "203.0.113.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := request(consoleHost, map[string][]string{"CF-Connecting-IP": tc.values})
			if got := auth.ClientAddress(r, consoleHost); got != tc.want {
				t.Errorf("ClientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheHeaderWinsOverTheForwardedChainOnTheConsole is AC-15. Behind the tunnel
// the forwarded chain is the proxy's, so one abuser has to be one bucket rather
// than everybody sharing the tunnel's.
func TestTheHeaderWinsOverTheForwardedChainOnTheConsole(t *testing.T) {
	// covers: AC-15, AC-16
	t.Parallel()
	r := request(consoleHost, map[string][]string{
		"CF-Connecting-IP": {"203.0.113.7"},
		"X-Forwarded-For":  {"203.0.113.7, 172.16.70.40"},
	})
	if got := auth.ClientAddress(r, consoleHost); got != "203.0.113.7" {
		t.Errorf("ClientAddress = %q, want the visitor rather than the proxy", got)
	}
}

// TestAnEmptyConsoleHostTrustsNothing is the platform with no public edge, and
// the /v1 surface, which passes an empty host on purpose. The header must not be
// readable there under any hostname.
func TestAnEmptyConsoleHostTrustsNothing(t *testing.T) {
	// covers: AC-15
	t.Parallel()
	for _, host := range []string{consoleHost, "deployer.example.ts.net", ""} {
		r := request(host, map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}})
		if got := auth.ClientAddress(r, ""); got != "10.42.0.9" {
			t.Errorf("on %q with no console host configured: ClientAddress = %q, want the connection address", host, got)
		}
	}
}
