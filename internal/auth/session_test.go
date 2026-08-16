package auth_test

import (
	"net/http"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// TestASessionIsReadUnderOneNameOnly is AC-19 and AC-20, and it is the pair of
// them rather than either alone.
//
// SessionID used to try both names, secure first, on the reasoning that the
// reader did not hold the platform's scheme. Two things followed. Renaming the
// cookie ended nobody's session, because the old name still authenticated, which
// is AC-20 not happening. And the protection the new name carries was defeated
// on the read side, because `deployer_session` has no `__Host-` prefix and can
// therefore be set with a Domain: an app on a sibling hostname under the
// platform's wildcard could scope one to the parent domain and the console would
// read it, which is the session fixation the prefix exists to close. Found live
// on 2026-08-16, by a browser holding only a parent scoped plain cookie reaching
// the signed in console.
//
// So the read takes the scheme, exactly as the write already does, and the other
// name resolves to nothing.
func TestASessionIsReadUnderOneNameOnly(t *testing.T) {
	// covers: AC-19, AC-20
	t.Parallel()

	cases := []struct {
		name   string
		secure bool
		cookie string
		want   string
	}{
		{"the prefixed name on a secure platform", true, auth.SessionCookieSecure, "the-session"},
		{"the plain name on a secure platform", true, auth.SessionCookiePlain, ""},
		{"the plain name on a plain platform", false, auth.SessionCookiePlain, "the-session"},
		{"the prefixed name on a plain platform", false, auth.SessionCookieSecure, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
			if err != nil {
				t.Fatalf("building a request: %v", err)
			}
			req.AddCookie(&http.Cookie{Name: c.cookie, Value: "the-session"})
			if got := auth.SessionID(req, c.secure); got != c.want {
				t.Errorf("a %s cookie resolved to %q, want %q", c.cookie, got, c.want)
			}
		})
	}
}

// TestAParentScopedPlainCookieIsNotASession is the attack the case above
// describes, written as its own test because it is the reason this change is not
// a tidy up. A sibling app cannot set the prefixed name at all, so refusing the
// plain one is the whole of what the console needs.
func TestAParentScopedPlainCookieIsNotASession(t *testing.T) {
	// covers: AC-19, AC-20
	t.Parallel()

	req, err := http.NewRequest(http.MethodGet, "https://console.apps.example.org/apps", nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	// What a stranger's app on a sibling hostname is able to set, and nothing
	// else: the platform's own cookie is unreachable to it.
	req.AddCookie(&http.Cookie{Name: auth.SessionCookiePlain, Value: "the-attackers-session"})

	if got := auth.SessionID(req, true); got != "" {
		t.Errorf("a parent scoped %s cookie resolved to %q, want no session at all",
			auth.SessionCookiePlain, got)
	}
}
