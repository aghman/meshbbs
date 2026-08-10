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
//
// # Why this pins a connection and turns foreign keys off
//
// SQLite cannot alter a CHECK constraint. Widening one means the documented
// twelve-step rebuild — create the new table, copy, drop the old, rename — and
// the DROP is the problem: `files` references `areas` ON DELETE CASCADE, so
// dropping the parent with foreign keys enforced deletes every file row on the
// way past. Migration 0007 does exactly that rebuild.
//
// PRAGMA foreign_keys is a no-op inside a transaction and each migration runs
// in one, so it cannot live in the .sql file. It is also per-CONNECTION state,
// and database/sql hands out whichever connection it likes — SetMaxOpenConns(1)
// makes that a single connection in practice, but "in practice" is not a
// property to rest a data-loss guard on. So migrations run on a connection
// pinned for their duration, and the guard is scoped to it by construction.
//
// Turning enforcement off does not mean giving it up: foreign_key_check runs
// afterward and refuses to open a database whose relationships a migration
// broke, which is a stronger check than the incremental one — it examines every
// row rather than the ones a statement happened to touch.
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin a connection for migrations: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("suspend foreign keys for migrations: %w", err)
	}
	// Restored on every path out, including the failures, because this
	// connection returns to the pool and the next caller must not inherit a
	// database with its integrity checks quietly switched off.
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`) //nolint:errcheck

	if _, err := conn.ExecContext(ctx, `
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
		err := conn.QueryRowContext(ctx,
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

		tx, err := conn.BeginTx(ctx, nil)
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

	// Every relationship in the database, not just the ones a migration
	// touched. If a rebuild orphaned a row this is where the database refuses
	// to open, rather than three weeks later when something reads it.
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check foreign keys after migrating: %w", err)
	}
	defer rows.Close()
	var broken []string
	for rows.Next() {
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("read foreign key violations: %w", err)
		}
		broken = append(broken, fmt.Sprintf("%s row %d -> %s",
			table.String, rowid.Int64, parent.String))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(broken) > 0 {
		return fmt.Errorf("migrations left %d broken foreign key reference(s): %s",
			len(broken), strings.Join(broken, "; "))
	}
	return nil
}

// now returns the current unix time from the injected clock (§12.1).
func (s *Store) now() int64 { return s.clock.Now().Unix() }

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("not found")

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
