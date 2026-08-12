// Package store is the platform's only writer of the SQLite database. It owns
// the connection, the migrations, and every transaction that has to hold more
// than one write together. Domain and use case packages never import it; they
// declare the narrow interfaces they need and take one of these types.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure Go driver, registered as "sqlite"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store holds the open database and the clock every timestamp comes from.
type Store struct {
	db     *sql.DB
	q      *sqlcgen.Queries
	clock  ids.Clock
	suffix func() string
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
	// SuffixSource produces the random tail of a new app slug. Nil means real
	// randomness; a test pins it to make a collision certain rather than rare.
	SuffixSource func() string
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
	if opts.SuffixSource == nil {
		opts.SuffixSource = domain.RandomSuffix
	}

	// Deliberately not _txlock=immediate. Taking the write lock at every BEGIN
	// would fix the SQLITE_BUSY a read then write transaction hits when it
	// upgrades its lock mid flight, but it would make every read only
	// transaction queue behind the writer too, and a deployment drive working to
	// a deadline can spend its whole budget waiting on that BEGIN and fail as an
	// internal fault instead of as a timeout. The rule this platform holds
	// instead: a transaction never reads and then writes. Anything that has to
	// decide from existing rows does it in one statement.
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
	return &Store{db: db, q: sqlcgen.New(db), clock: opts.Clock, suffix: opts.SuffixSource}, nil
}

// migrations builds a goose provider for this store. The provider API is used
// rather than goose's package level helpers because those write to process wide
// globals, which is a data race the moment two stores migrate at once.
func (s *Store) migrations() (*goose.Provider, error) {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: reading the embedded migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, sub)
	if err != nil {
		return nil, fmt.Errorf("store: preparing migrations: %w", err)
	}
	return p, nil
}

// Migrate brings the database up to the latest embedded migration. It is safe to
// call on every boot: an already migrated file is left alone.
func (s *Store) Migrate(ctx context.Context) error {
	p, err := s.migrations()
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: running migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls the database back one migration. It exists for tests and for
// a deliberate operator rollback, never for the boot path.
func (s *Store) MigrateDown(ctx context.Context) error {
	p, err := s.migrations()
	if err != nil {
		return err
	}
	if _, err := p.Down(ctx); err != nil {
		return fmt.Errorf("store: rolling back migrations: %w", err)
	}
	return nil
}

// Ready reports whether the database is open and every migration has applied.
// It is what the readiness probe asks (spec 0003, AC-4): a non nil error means
// the pod must be taken out of its Service, not restarted.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: the database is unreachable: %w", err)
	}
	p, err := s.migrations()
	if err != nil {
		return err
	}
	pending, err := p.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("store: reading the migration state: %w", err)
	}
	if pending {
		return errors.New("store: migrations are still pending")
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
