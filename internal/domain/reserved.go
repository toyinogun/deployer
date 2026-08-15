package domain

import "slices"

// reservedLabels are the hostname labels an app may never occupy under the
// platform's wildcard domain.
//
// A constant, not configuration. A scratch instance and the live one refuse the
// same names, so a slug that worked on one can never be the slug that collides
// with the console on the other (spec 0021, Key invariants).
//
// `console` is the load bearing one: it is where people sign in. The rest are
// names a reader would take for the platform itself rather than for somebody's
// app, which is worth refusing for the same reason.
var reservedLabels = []string{
	"admin",
	"api",
	"app",
	"console",
	"deployer",
	"mcp",
	"registry",
	"www",
}

// ReservedLabel reports whether name derives to a hostname label the platform
// keeps for itself.
//
// It asks about the readable base rather than the whole slug, because every slug
// carries a random suffix and so no derived slug is ever literally `console`.
// The base is what a person reads off the hostname, so the base is what this
// refuses (spec 0021, AC-6, AC-7).
func ReservedLabel(name string) bool {
	return slices.Contains(reservedLabels, DeriveBase(name))
}

// ReservedLabels returns the reserved set, for the surfaces that document it.
func ReservedLabels() []string {
	return slices.Clone(reservedLabels)
}
