package config

import "fmt"

// minCSRFKeyBytes is the shortest key the per session CSRF token may be derived
// under. Thirty two bytes is the output width of the SHA256 the HMAC runs over,
// so a shorter key adds nothing an attacker has to guess (spec 0013, AC-12).
const minCSRFKeyBytes = 32

// loadWeb reads the settings spec 0013 adds and returns a message for every
// value that was present but unusable, plus the names of the missing ones.
//
// The key is required rather than generated at boot, because a generated one
// would differ per pod and would invalidate every open form on a restart. It is
// validated here rather than at the first form render, like every other value.
func loadWeb(getenv func(string) string, c *Config) (missing []string, errs []string) {
	c.CSRFKey = getenv("DEPLOYER_CSRF_KEY")
	if c.CSRFKey == "" {
		return []string{"DEPLOYER_CSRF_KEY"}, nil
	}
	if len(c.CSRFKey) < minCSRFKeyBytes {
		errs = append(errs, fmt.Sprintf(
			"DEPLOYER_CSRF_KEY must be at least %d bytes of random data, got %d",
			minCSRFKeyBytes, len(c.CSRFKey)))
	}
	return nil, errs
}
