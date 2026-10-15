package eval

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent CLI capture and daemon auto-capture must both be able to open
// the registry. The persisted schema contract requires every migration to
// run once: a legacy row's disjoint input is 10 + 200 + 300 = 510, not 1010,
// and adding a reservation lease must preserve the existing reservation.
func TestOpenConcurrentMigrations(t *testing.T) {
	for _, schema := range []string{"fresh", "legacy", "legacy-wal", "current-wal", "park-pass"} {
		t.Run(schema, func(t *testing.T) {
			root := t.TempDir()
			switch schema {
			case "legacy", "legacy-wal":
				database := openLegacyEvaluationRegistry(t, root)
				if schema == "legacy-wal" {
					if _, err := database.Exec("PRAGMA journal_mode=WAL"); err != nil {
						t.Fatal(err)
					}
				}
				payload := filepath.Join(root, "evaluation.json")
				if err := writeJSON(payload, Evaluation{CacheReadTokens: 200, CacheWriteTokens: 300}); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`UPDATE evaluations SET path = ?`, payload); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`CREATE TABLE replay_case_reservations (
session_id TEXT NOT NULL, case_id TEXT NOT NULL REFERENCES cases(id),
PRIMARY KEY (session_id, case_id));
INSERT INTO replay_case_reservations VALUES ('session', 'case-old');`); err != nil {
					t.Fatal(err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			case "current-wal":
				store, err := Open(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
					_ = store.Close()
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			case "park-pass":
				database, err := sql.Open("sqlite", filepath.Join(root, "registry.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`CREATE TABLE cases (verdict_should_park INTEGER)`); err != nil {
					_ = database.Close()
					t.Fatal(err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}

			const openers = 8
			stores := make([]*Store, openers)
			errs := make([]error, openers)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for i := range openers {
				workers.Go(func() {
					<-start
					stores[i], errs[i] = Open(root)
				})
			}
			close(start)
			workers.Wait()
			for i, store := range stores {
				if store != nil {
					t.Cleanup(func() { _ = store.Close() })
				}
				if errs[i] != nil {
					t.Errorf("concurrent Open %d: %v", i, errs[i])
				}
			}
			if t.Failed() {
				t.FailNow()
			}
			for _, store := range stores {
				if schema != "legacy" && schema != "legacy-wal" {
					cases, err := store.ListCases("all")
					if err != nil || len(cases) != 0 {
						t.Fatalf("new registry cases = %v, err = %v", cases, err)
					}
					continue
				}
				var input, fresh, read, write int64
				if err := store.db.QueryRow(`SELECT input_tokens, fresh_input_tokens, cache_read_tokens, cache_write_tokens
FROM evaluations WHERE id = 'old-eval'`).Scan(&input, &fresh, &read, &write); err != nil {
					t.Fatal(err)
				}
				if input != 510 || fresh != 10 || read != 200 || write != 300 {
					t.Fatalf("legacy tokens = %d/%d/%d/%d, want 510/10/200/300", input, fresh, read, write)
				}
				var lease int64
				if err := store.db.QueryRow(`SELECT reserved_until FROM replay_case_reservations
WHERE session_id = 'session' AND case_id = 'case-old'`).Scan(&lease); err != nil || lease != 0 {
					t.Fatalf("legacy reservation lease = %d, err = %v", lease, err)
				}
			}
		})
	}
}
