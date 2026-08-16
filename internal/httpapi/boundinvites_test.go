package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mintJSON posts one mint to the JSON admin route and decodes whatever came
// back, refusal or invite alike.
func mintJSON(t *testing.T, h *idHarness, admin *http.Cookie, note, email string) (int, map[string]any) {
	t.Helper()
	rec := h.do(t, "POST", "/v1/admin/invites",
		map[string]string{"note": note, "email": email}, admin)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding the mint response: %v: %s", err, rec.Body)
		}
	}
	return rec.Code, body
}

// TestTheJSONMintTakesAnAddressToo is AC-15: neither surface can mint an invite
// the other cannot, so the JSON route takes the same optional address, applies
// the same validation and the same refusals, and runs the same inline send.
// covers: AC-15
func TestTheJSONMintTakesAnAddressToo(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	before := h.mail.count()

	status, body := mintJSON(t, h, admin, "for Sam", "Sam@Example.com")
	if status != http.StatusCreated {
		t.Fatalf("a bound JSON mint: got %d, want 201: %v", status, body)
	}
	if body["email"] != "sam@example.com" {
		t.Errorf("the response bound %v, want the normalized address", body["email"])
	}
	if body["sent"] != true {
		t.Error("the response does not report the send")
	}
	if h.mail.count() != before+1 {
		t.Fatalf("a bound JSON mint sent %d messages, want one", h.mail.count()-before)
	}
	sent := h.mail.last(t)
	if sent.To != "sam@example.com" {
		t.Errorf("the message went to %q", sent.To)
	}
	link, _ := body["link"].(string)
	if link == "" || !strings.Contains(sent.Body, link) {
		t.Error("the mailed message does not carry the link the response returned")
	}

	// An empty address is unchanged: an invite, no binding, and no message.
	before = h.mail.count()
	status, body = mintJSON(t, h, admin, "unbound", "")
	if status != http.StatusCreated {
		t.Fatalf("an unbound JSON mint: got %d, want 201", status)
	}
	if body["email"] != "" || body["sent"] != false {
		t.Errorf("an unbound mint reported email=%v sent=%v", body["email"], body["sent"])
	}
	if h.mail.count() != before {
		t.Error("an unbound JSON mint sent a message")
	}

	// The same refusals the page answers.
	if status, _ := mintJSON(t, h, admin, "", "not an address"); status != http.StatusUnprocessableEntity {
		t.Errorf("a malformed address: got %d, want 422", status)
	}
	if status, _ := mintJSON(t, h, admin, "", "admin@example.com"); status != http.StatusConflict {
		t.Errorf("an address that already has an account: got %d, want 409", status)
	}
}

// TestAFailedSendStillReturnsTheInvite is AC-6 on the JSON surface: the invite
// is committed before the send is attempted, so the caller gets its link and is
// told the send did not go, rather than getting an error and no invite.
// covers: AC-6
func TestAFailedSendStillReturnsTheInvite(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	h.mail.fail = true

	status, body := mintJSON(t, h, admin, "for Sam", "sam@example.com")
	if status != http.StatusCreated {
		t.Fatalf("a failed send refused the mint: got %d, want 201: %v", status, body)
	}
	if body["sent"] != false {
		t.Error("the response claims the message went")
	}
	if link, _ := body["link"].(string); link == "" {
		t.Error("the response carries no link, so the invite is unreachable")
	}
}

// TestTheJSONListCarriesTheBoundAddress is AC-14 on the JSON surface: the
// address is its own key, empty on an unbound invite, and the note is unchanged.
// covers: AC-14
func TestTheJSONListCarriesTheBoundAddress(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")

	if status, _ := mintJSON(t, h, admin, "bound one", "sam@example.com"); status != http.StatusCreated {
		t.Fatalf("minting a bound invite: got %d", status)
	}
	if status, _ := mintJSON(t, h, admin, "unbound one", ""); status != http.StatusCreated {
		t.Fatalf("minting an unbound invite: got %d", status)
	}

	rec := h.do(t, "GET", "/v1/admin/invites", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("listing invites: got %d", rec.Code)
	}
	var listed struct {
		Invites []struct{ Note, Email string } `json:"invites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decoding the list: %v", err)
	}
	seen := map[string]string{}
	for _, inv := range listed.Invites {
		seen[inv.Note] = inv.Email
	}
	if seen["bound one"] != "sam@example.com" {
		t.Errorf("the bound invite listed %q", seen["bound one"])
	}
	if seen["unbound one"] != "" {
		t.Errorf("the unbound invite listed %q, want empty", seen["unbound one"])
	}
}
