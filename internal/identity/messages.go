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
	inviteSubject            = "You have been invited to Deployer"
)

// inviteBody is the message an invited person gets. It is the platform's first
// message to somebody who has no account, and the only one that carries a live
// credential rather than a link to an account that already exists (spec 0025,
// AC-4, AC-12).
//
// The link works for this address alone, which is worth saying: a person who
// registers with a different address of theirs is refused in words that cannot
// explain why, so the message is the one place they can be told.
func inviteBody(inviter, link string, days int) string {
	return fmt.Sprintf(`%s has invited you to Deployer.

Create your account here:

%s

The link works once, expires in %d days, and only works for the address this
message was sent to. If you were not expecting this, you can ignore it: nothing
happens until the link is used.
`, inviter, link, days)
}

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
// ConsoleURL, never InternalURL: this text is read by a person, whose browser
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
