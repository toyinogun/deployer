package domain

// ValidConfigKey reports whether key matches the platform's naming rule for an
// environment variable: an upper case letter or underscore, then any number of
// upper case letters, digits, and underscores.
//
// Spec 0002 fixes the database CHECK as GLOB '[A-Z_][A-Z0-9_]*'. GLOB is shell
// style globbing rather than a regular expression, so its trailing * matches any
// characters at all: the constraint only really polices the first character.
// This function enforces what that pattern plainly means, so ErrInvalidKey is a
// promise rather than a hope. The CHECK stays as the spec fixed it and remains
// the backstop against a write that does not come through here.
func ValidConfigKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
