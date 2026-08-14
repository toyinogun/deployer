package identity

import (
	"fmt"
	"net/url"
)

// The messages the platform sends. They are plain text on purpose: this is a
// homelab control plane, and a link a person can read before clicking is worth
// more here than a rendered layout.
const (
	verifySubject            = "Confirm your Deployer address"
	alreadyRegisteredSubject = "Someone tried to register your Deployer address"
	resetSubject             = "Reset your Deployer password"
)

// verifyBody is the message a new registration gets.
func verifyBody(link string) string {
	return fmt.Sprintf(`Welcome to Deployer.

Confirm this address to finish setting up your account:

%s

The link works once and expires in 24 hours. If you did not register, you can
ignore this message: nothing happens until the link is used.
`, link)
}

// alreadyRegisteredBody is what the real owner of an address gets when somebody
// tries to register it again. It carries no link, because there is nothing to
// confirm: the account already exists and the person who has it can sign in.
func alreadyRegisteredBody(baseURL string) string {
	return fmt.Sprintf(`Someone tried to register a Deployer account with this address.

You already have one, so nothing was created and nothing changed. Sign in at
%s instead, or reset your password if you have forgotten it.

If that was not you, you can ignore this message.
`, baseURL)
}

// resetBody is the password reset message.
func resetBody(link string) string {
	return fmt.Sprintf(`Someone asked to reset the password on your Deployer account.

Set a new one here:

%s

The link works once and expires in 24 hours. If that was not you, ignore this
message and your password stays as it is.
`, link)
}

// linkURL builds the address a mailed link points at. It is built from
// PublicURL, never InternalURL: this text is read by a person, whose browser
// resolves names on the tailnet rather than on cluster DNS.
// The paths are the page ones, not the /v1 ones: a person clicking a link in
// their mail should land on a page, not on a JSON body. The /v1 endpoints keep
// answering exactly as they did and stay drivable with curl, they are simply no
// longer what a mailed link points at (spec 0013, AC-10).
func linkURL(baseURL, purpose, rawToken string) string {
	path := "/verify"
	if purpose == PurposeReset {
		path = "/reset"
	}
	return baseURL + path + "?token=" + url.QueryEscape(rawToken)
}
