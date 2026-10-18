package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublicWritersRetryPastBusyTimeout(t *testing.T) {
	for _, surface := range []string{"run_insert", "step_result", "launch_receipt", "migration"} {
		t.Run(surface, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, err := d.InsertRepo("/test", "git@github.com:test/test.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			holder, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			holder.SetMaxOpenConns(1)
			defer holder.Close()
			if _, err := holder.Exec("BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			released := make(chan error, 1)
			go func() {
				time.Sleep(sqlitewrite.BusyTimeout + time.Second)
				_, err := holder.Exec("ROLLBACK")
				released <- err
			}()
			start := time.Now()
			switch surface {
			case "run_insert":
				inserted, writeErr := d.InsertRun(repo.ID, "other", "head", "base")
				err = writeErr
				if err == nil && (inserted == nil || inserted.Status != types.RunPending) {
					t.Error("run not admitted")
				}
			case "step_result":
				step, writeErr := d.InsertStepResult(run.ID, types.StepReview)
				err = writeErr
				if err == nil && step.ID == "" {
					t.Error("step not recorded")
				}
			case "launch_receipt":
				receipt, claimed, writeErr := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, nil)
				err = writeErr
				if err == nil && (receipt == nil || !claimed) {
					t.Error("receipt not claimed")
				}
				if err == nil {
					_, second, writeErr := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, nil)
					if writeErr != nil || second {
						t.Errorf("receipt claimed twice: %v %v", second, writeErr)
					}
				}
			case "migration":
				opened, writeErr := Open(path)
				err = writeErr
				if opened != nil {
					opened.Close()
				}
			}
			if releaseErr := <-released; releaseErr != nil {
				t.Fatal(releaseErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) < sqlitewrite.BusyTimeout {
				t.Fatalf("did not wait through timeout: %v", time.Since(start))
			}
			t.Logf("%s succeeded after held exclusive writer: %v", surface, time.Since(start))
		})
	}
}
