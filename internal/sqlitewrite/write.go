// Package sqlitewrite owns the SQLite write contention policy for pipeline,
// daemon and local eval storage. Retry callbacks must contain only database
// work (or explicitly idempotent local work), never pipeline or forge actions.
package sqlitewrite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	BusyTimeout = 5 * time.Second
	Budget      = 30 * time.Second
	// ErrorName is also the durable recovery discriminator. Do not change it
	// without teaching older run records' status recovery about the old name.
	ErrorName = "SQLITE_WRITE_CONTENTION"
)

// ContentionError is an infrastructure failure, never a verdict on source code.
// Operation is a static label: SQL arguments (which can contain secrets) are
// deliberately excluded from this diagnostic.
type ContentionError struct {
	Operation string
	Attempts  int
	Cause     error
}

func (e *ContentionError) Error() string {
	return fmt.Sprintf("%s: infrastructure failure writing %s after %d attempts; retry the operation when the database lock is released: %v", ErrorName, e.Operation, e.Attempts, e.Cause)
}
func (e *ContentionError) Unwrap() error { return e.Cause }
func IsContention(err error) bool {
	var target *ContentionError
	return errors.As(err, &target)
}

// RecordedContention recognizes the named outcome after persistence/restart.
func RecordedContention(message string) bool { return strings.HasPrefix(message, ErrorName+":") }

func busy(err error) bool {
	var target *sqlite.Error
	if !errors.As(err, &target) {
		return false
	}
	code := target.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// Do retains SQLite's connection-level busy timeout and adds a bounded,
// cancelable retry with jitter. An entire transaction must be in the callback:
// BUSY_SNAPSHOT cannot be repaired by retrying only its failed statement.
// The deadline stops new attempts. The driver may finish one in-flight busy
// handler after that deadline (at most BusyTimeout for our connections).
func Do(ctx context.Context, operation string, write func(context.Context) error) error {
	return retry(ctx, operation, Budget, write)
}

func retry(ctx context.Context, operation string, budget time.Duration, write func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if last == nil {
				if !errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				last = err
			}
			failure := &ContentionError{operation, attempt - 1, last}
			slog.Error(failure.Error())
			return failure
		}
		err := write(ctx)
		if err == nil {
			return nil
		}
		if !busy(err) {
			// A deadline can interrupt a retry inside the driver, replacing BUSY
			// with context.DeadlineExceeded. Retain the actual lock cause.
			cancelledTx := errors.Is(err, sql.ErrTxDone)
			if ctx.Err() == nil || (!errors.Is(err, ctx.Err()) && !cancelledTx) {
				return err
			}
			if last == nil {
				if !errors.Is(err, context.DeadlineExceeded) && !(cancelledTx && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
					return err
				}
				// Waiting for the single pooled connection is also storage work,
				// not a code verdict, even when SQLite has not seen this statement.
				last = err
			}
		} else {
			last = err
		}
		if ctx.Err() != nil {
			failure := &ContentionError{operation, attempt, last}
			slog.Error(failure.Error())
			return failure
		}
		capDelay := 25 * time.Millisecond << min(attempt-1, 4)
		delay := capDelay/2 + time.Duration(rand.Int64N(int64(capDelay/2)+1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Value is the result-bearing form of Do. Results from a rolled-back attempt
// are never returned to the caller.
func Value[T any](ctx context.Context, operation string, write func(context.Context) (T, error)) (T, error) {
	var result T
	err := Do(ctx, operation, func(ctx context.Context) error {
		value, err := write(ctx)
		if err == nil {
			result = value
		}
		return err
	})
	return result, err
}

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func Exec(ctx context.Context, database executor, operation, query string, args ...any) (sql.Result, error) {
	return Value(ctx, operation, func(ctx context.Context) (sql.Result, error) {
		return database.ExecContext(ctx, query, args...)
	})
}

// Transaction rolls back before every retry, including COMMIT failures.
func Transaction(ctx context.Context, database *sql.DB, operation string, write func(context.Context, *sql.Tx) error) error {
	return Do(ctx, operation, func(ctx context.Context) error {
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := write(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}
