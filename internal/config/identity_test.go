package config

import (
	"strings"
	"testing"
)

// TestMailConfiguration is spec 0007's configuration rule: a key with no From
// address fails the boot, and a From address with no key is the supported "no
// sender" state rather than a failure, so the address can sit in the ConfigMap
// waiting for the sealed key to arrive (AC-26).
func TestMailConfiguration(t *testing.T) {
	tests := []struct {
		name, key, from string
		wantErr         string
	}{
		{"neither is set", "", "", ""},
		{"both are set", "re_abc", "noreply@example.com", ""},
		{"a From address alone is fine", "", "noreply@example.com", ""},
		{"a key with no From address", "re_abc", "", "DEPLOYER_MAIL_FROM"},
		{"an unusable From address", "re_abc", "not an address", "must be an email address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withValid(map[string]string{
				"DEPLOYER_RESEND_API_KEY": tc.key,
				"DEPLOYER_MAIL_FROM":      tc.from,
			})))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want an error naming %q", err, tc.wantErr)
				}
				return
			}
			if c.ResendAPIKey != tc.key || c.MailFrom != tc.from {
				t.Errorf("got key %q from %q, want %q and %q", c.ResendAPIKey, c.MailFrom, tc.key, tc.from)
			}
		})
	}
}
