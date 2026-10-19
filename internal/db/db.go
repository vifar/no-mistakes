package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// Bump whenever schemaSQL or migrationStatements changes. Version zero is the
// historical, unversioned schema; its complete upgrade runs once before the
// stamp is committed. Current opens execute no DDL or backfill DML.
const schemaVersion = 4

// DB wraps a SQLite database connection.
type DB struct {
	sql    *sql.DB
	closed atomic.Bool
	// queueClock stamps queued_at; nil means the wall clock. A test seam for
	// proving the stored queue order does not depend on the clock.
	queueClock func() int64
}

// Open opens (or creates) the SQLite database at path, running migrations only
// when its schema version is older than this binary's supported version.
// This check cannot prevent pre-versioning executables from writing the same
// database: those binaries do not inspect user_version. Downgrade safety is not
// guaranteed; use a separate root or a compatible backup when changing versions.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(%d)", path, sqlitewrite.BusyTimeout.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{sql: sqlDB}, nil
}

func migrate(sqlDB *sql.DB) error {
	// The startup migration has no caller deadline. Bound contention retries,
	// but retain main's uninterrupted schema upgrade: its many DDL statements
	// can legitimately take longer than the retry budget without any lock.
	return sqlitewrite.Do(context.Background(), "database migration", func(context.Context) error {
		return migrateWriteAttempt(sqlDB)
	})
}

func migrateWriteAttempt(sqlDB *sql.DB) error {
	var version int
	if err := sqlDB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read db schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d; update this binary", version, schemaVersion)
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("begin db migration: %w", err)
	}
	defer tx.Rollback()
	if version >= 1 {
		if version == 1 {
			if _, err := tx.Exec(ownerAdmissionSchema); err != nil {
				return fmt.Errorf("migrate db (owner_admission_events): %w", err)
			}
		}
		if _, err := tx.Exec(pushNotificationSchema); err != nil {
			return fmt.Errorf("migrate db (push_notifications): %w", err)
		}
		if _, err := tx.Exec(statusValidationSchema); err != nil {
			return fmt.Errorf("migrate db (status validation): %w", err)
		}
		for _, stmt := range statusValidationMigrationStatements {
			if _, err := tx.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
				return fmt.Errorf("migrate db (%s): %w", stmt, err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(schemaSQL + ownerAdmissionSchema + pushNotificationSchema); err != nil {
		return fmt.Errorf("migrate db (schemaSQL): %w", err)
	}
	for _, stmt := range migrationStatements {
		if _, err := tx.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			return fmt.Errorf("migrate db (%s): %w", stmt, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
		return fmt.Errorf("migrate db (PRAGMA user_version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit db migration: %w", err)
	}
	return nil
}

// OpenReader opens only an existing, current schema. CLI observations must not
// migrate the daemon's database or silently change an older offline database.
// OpenReadOnly remains available for pre-mutation authorization that reads only
// historical columns and must also work before an upgrade.
func OpenReader(path string) (*DB, error) {
	d, err := OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	var version int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		d.Close()
		return nil, fmt.Errorf("read db schema version: %w", err)
	}
	if version != schemaVersion {
		d.Close()
		if version < schemaVersion {
			return nil, fmt.Errorf("database schema too old (%d, need %d); the daemon must be updated and started to migrate it", version, schemaVersion)
		}
		return nil, fmt.Errorf("database schema %d is newer than supported schema %d; update this binary", version, schemaVersion)
	}
	return d, nil
}

// OpenReadOnly opens an existing database without creating or migrating it.
// It is used by pre-mutation authorization, where even schema repair would be
// an unacceptable side effect before the caller is classified.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// isDuplicateColumnErr reports whether err is SQLite's "duplicate column name"
// error, which ALTER TABLE ADD COLUMN emits when the column already exists.
// Historical unversioned databases may already contain some additive columns;
// treating this as a no-op lets their complete upgrade finish before stamping.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database connection.
func (d *DB) Close() error {
	d.closed.Store(true)
	return d.sql.Close()
}

// newID generates a new ULID with monotonic ordering.
func newID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// now returns the current unix timestamp in seconds.
func now() int64 {
	return time.Now().Unix()
}

func (d *DB) Closed() bool { return d.closed.Load() }
