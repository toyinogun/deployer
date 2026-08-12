package domain

import "strings"

// RunsAsRoot reports whether an image's declared user means it would run as
// root. An image that names no user at all counts as root, because that is what
// a container runtime does with an empty USER (spec 0004, AC-10).
//
// The value is whatever the image config's User field held, which may be a name,
// a numeric id, or either of those with a group after a colon.
func RunsAsRoot(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return true
	}
	// A "user:group" pair only tells us about the user half.
	if name, _, ok := strings.Cut(user, ":"); ok {
		user = strings.TrimSpace(name)
	}
	return user == "" || user == "0" || user == "root"
}
