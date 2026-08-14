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

// How a write transaction that loses the race for the lock is retried. These are
// small on purpose: the busy timeout has already done the waiting by the time one
// of these attempts fails, so this is the last word on a genuinely contended
// file, not the main way the platform waits.
const (
	busyRetries = 3
	busyBackoff = 20 * time.Millisecond
)

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

	// Deliberately not _txlock=immediate, which is a connection wide setting and
	// so would make every read only transaction take the write lock at BEGIN too,
	// queueing reads behind the writer: a deployment drive working to a deadline
	// can spend its whole budget waiting there and fail as an internal fault
	// instead of as a timeout. Write transactions do need the lock up front,
	// because several of them read the row they are about to change and a
	// deferred transaction cannot upgrade that read into a write once another
	// connection has written. inTx takes it per transaction instead, so only the
	// writers wait. See the comment there for why the busy timeout cannot cover
	// this on its own.
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

// inTx runs fn inside a single write transaction and rolls back on any error, so
// a state change and its event row are always applied together or not at all.
//
// It begins with BEGIN IMMEDIATE, which takes the write lock here rather than at
// the first write inside fn. Every caller of this helper writes, and several of
// them have to decide from the row they are changing, so they read first. A
// deferred transaction that has already read cannot take the write lock once
// another connection has written: SQLite answers SQLITE_BUSY straight away and
// never calls the busy handler, because waiting would deadlock the writer that
// needs this reader's snapshot released. The busy timeout cannot help there, and
// the caller loses the write. Waiting at BEGIN is a wait the busy timeout does
// serve. Read only paths never come through here, they use the pool directly and
// stay deferred, so they still never queue behind a writer.
// A write that still cannot get the lock inside the busy timeout is retried
// rather than lost. The transaction rolled back whole, so running fn again is
// running it against the state it would have read on a first attempt, and every
// caller decides from the rows it reads inside the transaction rather than from
// anything it captured before it.
func (s *Store) inTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	var err error
	for attempt := range busyRetries {
		err = s.writeTx(ctx, fn)
		if !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(busyBackoff * time.Duration(attempt+1)):
		}
	}
	return err
}

// writeTx is one attempt at the transaction inTx retries.
func (s *Store) writeTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: taking a connection: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// The rollback runs even when ctx is already done, otherwise the
			// connection goes back to the pool with a transaction still open.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
		_ = conn.Close()
	}()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	if err := fn(sqlcgen.New(conn)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	committed = true
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
