// Package store is the platform's only writer of the SQLite database. It owns
// the connection, the migrations, and every transaction that has to hold more
// than one write together. Domain and use case packages never import it; they
// declare the narrow interfaces they need and take one of these types.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store holds the open database and the clock every timestamp comes from.
type Store struct {
	db    *sql.DB
	q     *sqlcgen.Queries
	clock ids.Clock
}

// Options configures Open.
type Options struct {
	// Path is the SQLite file. Use ":memory:" only in throwaway tests; the
	// platform always runs against a file on the mounted volume.
	Path string
	// BusyTimeout is how long a blocked writer waits before giving up.
	BusyTimeout time.Duration
	// Clock supplies every timestamp written. Nil means the system clock.
	Clock ids.Clock
}

// Open opens the database with WAL journaling, foreign keys enforced, and the
// busy timeout applied, all set once per connection so every connection the pool
// hands out carries them. It does not run migrations; call Migrate for that.
func Open(opts Options) (*Store, error) {
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	if opts.Clock == nil {
		opts.Clock = ids.SystemClock{}
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)",
		opts.Path, opts.BusyTimeout.Milliseconds(),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", opts.Path, err)
	}
	if err := db.Ping(); err != nil {
		// The open already failed, so a close error adds nothing to report.
		_ = db.Close()
		return nil, fmt.Errorf("store: connecting to %s: %w", opts.Path, err)
	}
	return &Store{db: db, q: sqlcgen.New(db), clock: opts.Clock}, nil
}

// Migrate brings the database up to the latest embedded migration. It is safe to
// call on every boot: an already migrated file is left alone.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("store: selecting migration dialect: %w", err)
	}
	if err := goose.UpContext(ctx, s.db, "migrations"); err != nil {
		return fmt.Errorf("store: running migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls the database back one migration. It exists for tests and for
// a deliberate operator rollback, never for the boot path.
func (s *Store) MigrateDown(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("store: selecting migration dialect: %w", err)
	}
	if err := goose.DownContext(ctx, s.db, "migrations"); err != nil {
		return fmt.Errorf("store: rolling back migrations: %w", err)
	}
	return nil
}

// DB exposes the raw handle for tests that assert on the schema itself.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// now returns the current time as the database stores it.
func (s *Store) now() string { return ids.Stamp(s.clock.Now()) }

// inTx runs fn inside a single transaction and rolls back on any error, so a
// state change and its event row are always applied together or not at all.
func (s *Store) inTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe as the
		// single unconditional cleanup path.
		_ = tx.Rollback()
	}()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	return nil
}

// ptr is the bridge to the generated code's nullable parameters, which are
// pointers because the columns they land in are nullable.
func ptr[T any](v T) *T { return &v }

// deref reads a nullable column into its zero value when it is null.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
