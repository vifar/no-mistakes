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
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			for iteration := 0; iteration < 20; iteration++ {
				root := t.TempDir()
				if legacy {
					db := openLegacyEvaluationRegistry(t, root)
					if _, err := db.Exec(`DELETE FROM evaluations`); err != nil {
						t.Fatal(err)
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
