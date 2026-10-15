package eval

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Independent opens model the daemon and CLI sharing a registry. A concurrent
// writer must delay migration, rather than make its read-to-write upgrade fail.
func TestStoreConcurrentOpenWithWriter(t *testing.T) {
	for _, schema := range []string{"fresh", "cache-columns", "reservation-lease", "park-pass"} {
		t.Run(schema, func(t *testing.T) {
			for iteration := 0; iteration < 20; iteration++ {
				root := t.TempDir()
				if schema != "fresh" {
					db := openLegacyEvaluationRegistry(t, root)
					if _, err := db.Exec(`DELETE FROM evaluations`); err != nil {
						t.Fatal(err)
					}
					switch schema {
					case "reservation-lease":
						if _, err := db.Exec(`CREATE TABLE replay_case_reservations (
    session_id TEXT NOT NULL,
    case_id TEXT NOT NULL REFERENCES cases(id) ON DELETE RESTRICT,
    PRIMARY KEY (session_id, case_id)
);
INSERT INTO replay_case_reservations VALUES ('session-old', 'case-old');`); err != nil {
							t.Fatal(err)
						}
					case "park-pass":
						if _, err := db.Exec(`ALTER TABLE cases ADD COLUMN verdict_should_park INTEGER NOT NULL DEFAULT 0`); err != nil {
							t.Fatal(err)
						}
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				writer, err := sql.Open("sqlite", filepath.Join(root, "registry.sqlite")+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
				if err != nil {
					t.Fatal(err)
				}
				writer.SetMaxOpenConns(1)
				if _, err := writer.Exec(`CREATE TABLE contention (value INTEGER); INSERT INTO contention VALUES (0)`); err != nil {
					_ = writer.Close()
					t.Fatal(err)
				}
				start := make(chan struct{})
				writerDone := make(chan error, 1)
				go func() {
					<-start
					// A finite burst leaves time for busy_timeout to admit waiters;
					// an unbounded writer could legitimately exhaust that timeout.
					for range 100 {
						if _, err := writer.Exec(`UPDATE contention SET value = value + 1`); err != nil {
							writerDone <- err
							return
						}
					}
					writerDone <- nil
				}()
				var wg sync.WaitGroup
				errors := make(chan error, 8)
				for range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						store, err := Open(root)
						if err == nil {
							var columns int
							err = store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('evaluations') WHERE name IN ('cache_read_tokens', 'cache_write_tokens')`).Scan(&columns)
							if err == nil && columns != 2 {
								err = fmt.Errorf("cache columns = %d, want 2", columns)
							}
							if err == nil {
								var leaseColumns, parkColumns, cases int
								err = store.db.QueryRow(`SELECT
    (SELECT count(*) FROM pragma_table_info('replay_case_reservations') WHERE name = 'reserved_until'),
    (SELECT count(*) FROM pragma_table_info('cases') WHERE name = 'verdict_should_park'),
    (SELECT count(*) FROM cases)`).Scan(&leaseColumns, &parkColumns, &cases)
								wantCases := 1
								if schema == "fresh" || schema == "park-pass" {
									wantCases = 0
								}
								if err == nil && (leaseColumns != 1 || parkColumns != 0 || cases != wantCases) {
									err = fmt.Errorf("migrated schema: lease columns=%d, park columns=%d, cases=%d (want %d)", leaseColumns, parkColumns, cases, wantCases)
								}
							}
							if err == nil && schema == "reservation-lease" {
								var lease int
								err = store.db.QueryRow(`SELECT reserved_until FROM replay_case_reservations WHERE session_id = 'session-old' AND case_id = 'case-old'`).Scan(&lease)
								if err == nil && lease != 0 {
									err = fmt.Errorf("legacy reservation lease = %d, want 0", lease)
								}
							}
							if closeErr := store.Close(); err == nil {
								err = closeErr
							}
						}
						errors <- err
					}()
				}
				close(start)
				wg.Wait()
				if err := <-writerDone; err != nil {
					t.Errorf("iteration %d writer: %v", iteration, err)
				}
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
				close(errors)
				for err := range errors {
					if err != nil {
						t.Errorf("iteration %d Open: %v", iteration, err)
					}
				}
				if t.Failed() {
					return
				}
			}
		})
	}
}
