package store

import (
	"errors"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DefaultPageSize is how many rows a list returns when the caller does not say.
const DefaultPageSize = 50

// MaxPageSize caps a page so no caller can ask for the whole table.
const MaxPageSize = 200

// Page is the paging window every list method takes. There is no unpaginated
// list in the store. Cursor is the last id of the previous page; empty starts at
// the newest row.
type Page struct {
	Cursor string
	Limit  int
}

// limit clamps the requested size into the allowed range.
func (p Page) limit() int64 {
	switch {
	case p.Limit <= 0:
		return DefaultPageSize
	case p.Limit > MaxPageSize:
		return MaxPageSize
	default:
		return int64(p.Limit)
	}
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate, which is
// how the database, not Go, enforces the slug, release numbering, and one
// release per deployment rules.
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	code := serr.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// isConstraintViolation reports whether err is any constraint the schema
// enforces: a CHECK, a foreign key, a NOT NULL, or a uniqueness rule.
func isConstraintViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	// SQLite packs the specific constraint into the high bits of the code, so
	// the low byte is what identifies the whole family.
	return serr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

// isBusy reports whether err is SQLite refusing to hand over the write lock in
// the time the busy timeout allows. It is worth one more attempt rather than a
// lost write: inTx already asks for the lock at BEGIN, so a transaction that
// still cannot get it lost a genuine race for a contended file rather than hit
// the un waitable lock upgrade that BEGIN IMMEDIATE exists to avoid.
func isBusy(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	code := serr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}
