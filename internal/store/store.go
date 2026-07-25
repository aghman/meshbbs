// Package store is the SQLite persistence layer (design §11.1).
//
// It uses modernc.org/sqlite — the pure-Go driver — because the alternative
// (mattn/go-sqlite3) needs a C toolchain, which would destroy clean
// cross-compilation and force per-platform build tags. Keeping every artifact
// CGO_ENABLED=0 is a stated non-negotiable (§4, §10).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aghman/meshbbs/internal/clock"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store owns the database handle.
type Store struct {
	db    *sql.DB
	clock clock.Clock
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string, clk clock.Clock) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}

	// _pragma parameters are applied per-connection by the driver.
	//   busy_timeout — wait rather than failing instantly under contention.
	//   journal_mode=WAL — readers do not block the writer.
	//   synchronous=FULL — durability matters here: §6.2.1 rule 3 requires the
	//     sequence high-water mark to survive a crash, and a lost fsync is
	//     exactly the "restore from backup" divergence we are guarding against.
	//   foreign_keys — enforce the ON DELETE CASCADE relationships.
	dsn := path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// SQLite tolerates one writer. Serializing here avoids SQLITE_BUSY storms
	// and keeps write ordering deterministic, which the simulator depends on.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to database %s: %w", path, err)
	}

	s := &Store{db: db, clock: clk}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenMemory opens an in-memory database, for tests.
func OpenMemory(ctx context.Context, clk clock.Clock) (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, clock: clk}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for packages that need direct access.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies any migrations not yet recorded, in filename order.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	// Filename order is the migration order, and it must be deterministic:
	// ReadDir already sorts, but sorting explicitly documents the dependency.
	sort.Strings(names)

	for _, name := range names {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, s.now()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// now returns the current unix time from the injected clock (§12.1).
func (s *Store) now() int64 { return s.clock.Now().Unix() }

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("not found")

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
