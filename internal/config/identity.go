package config

import (
	"fmt"
	"net/mail"
)

// loadIdentity reads the settings spec 0007 adds and returns a message for every
// value that was present but unusable.
//
// The From address is required whenever the API key is set, and checked here
// rather than at the first send, which is exactly the kind of deferred check the
// platform does not allow (AC-26, and the rule in AGENTS.md).
//
// The other way round is not an error. A From address with no key means no
// sender, which is a supported state: the address can sit in the ConfigMap
// waiting for the sealed key to arrive, rather than failing every boot until it
// does.
func loadIdentity(getenv func(string) string, c *Config) (errs []string) {
	c.ResendAPIKey = getenv("DEPLOYER_RESEND_API_KEY")
	c.MailFrom = getenv("DEPLOYER_MAIL_FROM")

	if c.ResendAPIKey != "" && c.MailFrom == "" {
		errs = append(errs, "DEPLOYER_RESEND_API_KEY is set but DEPLOYER_MAIL_FROM is not, so there is no address to send as")
	}
	if c.MailFrom != "" {
		if _, err := mail.ParseAddress(c.MailFrom); err != nil {
			errs = append(errs, fmt.Sprintf("DEPLOYER_MAIL_FROM must be an email address, got %q", c.MailFrom))
		}
	}
	return errs
}
