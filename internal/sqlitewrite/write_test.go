package sqlitewrite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func TestDeadlineRollbackPreservesContention(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel=%v/wrapped=%v", cancelParent, wrapped), func(t *testing.T) {
				t.Parallel()
				database, release := exclusiveFixture(t, time.Millisecond)
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				attempts := 0
				var lockCause error
				err := retry(parent, "deadline rollback", 300*time.Millisecond, func(ctx context.Context) error {
					attempts++
					tx, err := database.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					_, err = tx.ExecContext(ctx, `INSERT INTO writes VALUES(1)`)
					if attempts == 1 {
						if !busy(err) {
							t.Fatalf("expected actual SQLite contention: %v", err)
						}
						lockCause = err
						release()
						return err
					}
					if err != nil {
						return err
					}
					if cancelParent {
						cancel()
					}
					<-ctx.Done()
					conn, err := database.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					err = tx.Commit()
					if !errors.Is(err, sql.ErrTxDone) {
						t.Fatalf("expected cancellation rollback before Commit, got %v", err)
					}
					if wrapped {
						return fmt.Errorf("commit interrupted: %w", err)
					}
					return err
				})
				var contention *ContentionError
				if !errors.As(err, &contention) || !errors.Is(err, lockCause) || contention.Attempts != 2 || contention.Operation != "deadline rollback" {
					t.Fatalf("lost prior SQLite cause after automatic rollback: attempts=%d error=%T %v", attempts, err, err)
				}
				var count int
				if err := database.QueryRow(`SELECT count(*) FROM writes`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("cancelled transaction committed: count=%d err=%v", count, err)
				}
			})
		}
	}
}

func TestRetryCancellationClassificationControls(t *testing.T) {
	for _, tc := range []struct {
		name           string
		priorBusy      bool
		cancel         bool
		result         error
		wantContention bool
	}{
		{name: "ordinary transaction done", result: sql.ErrTxDone},
		{name: "transaction done after busy without cancellation", priorBusy: true, result: fmt.Errorf("commit: %w", sql.ErrTxDone)},
		{name: "transaction done without prior busy", cancel: true, result: sql.ErrTxDone},
		{name: "source failure after busy and cancellation", priorBusy: true, cancel: true, result: errors.New("source failure")},
		{name: "wrapped cancellation without prior busy", cancel: true, result: fmt.Errorf("cancelled: %w", context.Canceled)},
		{name: "wrapped cancellation after busy", priorBusy: true, cancel: true, result: fmt.Errorf("cancelled: %w", context.Canceled), wantContention: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, release := exclusiveFixture(t, time.Millisecond)
			if !tc.priorBusy {
				release()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			var lockCause error
			err := Do(ctx, "classification control", func(ctx context.Context) error {
				attempts++
				if tc.priorBusy && attempts == 1 {
					_, err := database.ExecContext(ctx, `INSERT INTO writes VALUES(1)`)
					if !busy(err) {
						t.Fatalf("expected actual contention: %v", err)
					}
					lockCause = err
					release()
					return err
				}
				if tc.cancel {
					cancel()
				}
				return tc.result
			})
			wantAttempts := 1
			if tc.priorBusy {
				wantAttempts++
			}
			if attempts != wantAttempts || IsContention(err) != tc.wantContention {
				t.Fatalf("attempts=%d err=%T %v", attempts, err, err)
			}
			if tc.wantContention {
				if !errors.Is(err, lockCause) {
					t.Fatalf("lost lock cause: %v", err)
				}
			} else if !errors.Is(err, tc.result) {
				t.Fatalf("reclassified non-contention: %v", err)
			}
		})
	}
	t.Run("cancelled before first attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Do(ctx, "already cancelled", func(context.Context) error { t.Fatal("started cancelled operation"); return nil })
		if !errors.Is(err, context.Canceled) || IsContention(err) {
			t.Fatalf("misclassified cancellation: %v", err)
		}
	})
}

func TestFirstAttemptAutomaticRollbackClassification(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancel=%v/wrapped=%v", cancelParent, wrapped), func(t *testing.T) {
				t.Parallel()
				database, release := exclusiveFixture(t, time.Millisecond)
				release()
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				attempts := 0
				var commitErr error
				err := retry(parent, "first attempt rollback", 100*time.Millisecond, func(ctx context.Context) error {
					attempts++
					tx, err := database.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(ctx, `INSERT INTO writes VALUES(1)`); err != nil {
						t.Fatal(err)
					}
					if cancelParent {
						cancel()
					}
					<-ctx.Done()
					conn, err := database.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					commitErr = tx.Commit()
					if !errors.Is(commitErr, sql.ErrTxDone) {
						t.Fatalf("expected automatic rollback before Commit: %v", commitErr)
					}
					if wrapped {
						commitErr = fmt.Errorf("commit interrupted: %w", commitErr)
					}
					return commitErr
				})
				if attempts != 1 || !errors.Is(err, commitErr) {
					t.Fatalf("lost first-attempt commit error: attempts=%d err=%T %v", attempts, err, err)
				}
				var contention *ContentionError
				if cancelParent {
					if IsContention(err) {
						t.Fatalf("caller cancellation became contention: %v", err)
					}
				} else if !errors.As(err, &contention) || contention.Attempts != 1 || contention.Operation != "first attempt rollback" {
					t.Fatalf("deadline rollback became a code failure: %T %v", err, err)
				}
				var count int
				if err := database.QueryRow(`SELECT count(*) FROM writes`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("rolled-back write persisted: count=%d err=%v", count, err)
				}
			})
		}
	}
}
