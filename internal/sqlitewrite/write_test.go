package sqlitewrite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func exclusiveFixture(t *testing.T, timeout time.Duration) (*sql.DB, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exclusive.sqlite")
	database, err := sql.Open("sqlite", path+"?_pragma=busy_timeout("+strconv.FormatInt(timeout.Milliseconds(), 10)+")")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(`CREATE TABLE writes (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { holder.Close() })
	if _, err := holder.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	release := func() {
		if _, err := holder.Exec("ROLLBACK"); err != nil {
			t.Error(err)
		}
	}
	return database, release
}

func TestExclusiveLockOutlivesBusyTimeoutThenWriteSucceeds(t *testing.T) {
	t.Parallel()
	database, release := exclusiveFixture(t, BusyTimeout)
	released := make(chan struct{})
	go func() { time.Sleep(BusyTimeout + time.Second); release(); close(released) }()
	start := time.Now()
	attempts := 0
	err := Do(context.Background(), "real exclusive lock", func(ctx context.Context) error {
		attempts++
		_, err := database.ExecContext(ctx, `INSERT INTO writes VALUES (1)`)
		return err
	})
	<-released
	if err != nil {
		t.Fatal(err)
	}
	if attempts < 2 || time.Since(start) < BusyTimeout {
		t.Fatalf("attempts=%d elapsed=%v", attempts, time.Since(start))
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM writes`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("write count=%d err=%v", count, err)
	}
	t.Logf("exclusive lock held 6s, attempts=%d, success after %v", attempts, time.Since(start))
}

func TestExclusiveLockPastBudgetNamesInfrastructureFailure(t *testing.T) {
	t.Parallel()
	database, release := exclusiveFixture(t, BusyTimeout)
	defer release()
	start := time.Now()
	_, err := Exec(context.Background(), database, "real held lock", `INSERT INTO writes VALUES (1)`)
	var named *ContentionError
	var cause *sqlite.Error
	if !errors.As(err, &named) || !errors.As(err, &cause) || cause.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("error=%T %v", err, err)
	}
	if named.Attempts < 2 || time.Since(start) < Budget || time.Since(start) > Budget+BusyTimeout+2*time.Second {
		t.Fatalf("attempts=%d elapsed=%v", named.Attempts, time.Since(start))
	}
	t.Logf("held exclusive lock past budget: %v; elapsed=%v", err, time.Since(start))
}

func TestNonContentionIsNotRetried(t *testing.T) {
	database, release := exclusiveFixture(t, time.Millisecond)
	release()
	attempts := 0
	err := Do(context.Background(), "invalid SQL", func(ctx context.Context) error {
		attempts++
		_, err := database.ExecContext(ctx, `INSERT INTO missing VALUES (1)`)
		return err
	})
	if err == nil || IsContention(err) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestCancellationStopsRetryAndPreservesLockCause(t *testing.T) {
	database, release := exclusiveFixture(t, 20*time.Millisecond)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Exec(ctx, database, "cancelled lock wait", `INSERT INTO writes VALUES (1)`)
	if !IsContention(err) {
		t.Fatalf("expected named lock cause: %T %v", err, err)
	}
}

func TestLockedTableIsRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked-table.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	conn, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `CREATE TABLE writes (value INTEGER); INSERT INTO writes VALUES(1),(2)`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(context.Background(), `SELECT value FROM writes`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected active reader")
	}
	attempts := 0
	err = Do(context.Background(), "locked table", func(ctx context.Context) error {
		attempts++
		_, err := conn.ExecContext(ctx, `DROP TABLE writes`)
		if attempts == 1 {
			var locked *sqlite.Error
			if !errors.As(err, &locked) || locked.Code()&0xff != sqlite3.SQLITE_LOCKED {
				t.Fatalf("expected real SQLITE_LOCKED, got %v", err)
			}
			if closeErr := rows.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestConnectionWaitDeadlineIsInfrastructureFailure(t *testing.T) {
	database, release := exclusiveFixture(t, time.Millisecond)
	release()
	conn, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = Exec(ctx, database, "wait for pooled writer", `INSERT INTO writes VALUES(1)`)
	if !IsContention(err) {
		t.Fatalf("pool timeout must not become code failure: %T %v", err, err)
	}
}
