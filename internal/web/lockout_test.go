package web

import (
	"net/http"
	"net/url"
	"testing"
)

// TestTheBrowserSignInLocksOutLikeTheJSONSurface is spec 0021 AC-5: the two sign
// in surfaces refuse the same way, so the browser cannot be a softer way in.
//
// It was not true. The lockout lived in the JSON handler in internal/httpapi,
// which called LockedOut before the work and Failed after a wrong password,
// while the browser handler only spent a rate limit token. Service.Login touched
// the limiter nowhere, so the comment on loginSubmit claiming the lockout came
// from the shared call described an intent nothing implemented.
//
// Spec 0021 is what made it matter. The JSON surface is 404 on the console
// hostname and reachable only on the tailnet, so after the flip the surface with
// no lockout is the one on the open internet, and the one with the lockout is
// the one nobody outside can reach. Measured against the live cluster on
// 2026-08-16: the JSON surface answered 401 five times then 429, and the browser
// answered 401 eight times for the same address, including while that address
// was already locked out.
//
// The attempts stay under the ten token bucket so a 429 here can only be the
// lockout, never the bucket.
// covers: AC-5
func TestTheBrowserSignInLocksOutLikeTheJSONSurface(t *testing.T) {
	h := newHarness(t, nil)
	const email = "lockout@example.test"
	h.signIn(t, email)

	wrong := url.Values{"email": {email}, "password": {"not the right password"}}

	// failuresBeforeLockout is 5, so five attempts are refused as bad
	// credentials and the sixth is refused as a lockout.
	for i := 1; i <= 5; i++ {
		if rec := h.post(t, "/login", wrong, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: got %d, want 401", i, rec.Code)
		}
	}

	rec := h.post(t, "/login", wrong, nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the sixth wrong password: got %d, want 429, so the browser is a softer way in "+
			"than the JSON surface and an online guess is bounded only by the per address bucket", rec.Code)
	}

	// The lockout is on the account, not the caller, so the right password is
	// refused too while the penalty window is open. This is what stops the
	// browser being used to check a guess the JSON surface would have refused.
	right := url.Values{"email": {email}, "password": {testPassword}}
	if rec := h.post(t, "/login", right, nil, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the right password inside the penalty window: got %d, want 429", rec.Code)
	}
}
