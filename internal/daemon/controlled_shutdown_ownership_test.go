package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPlannedShutdownRefusesUnsettledUnadmittedRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER fail_unadmitted_terminal BEFORE UPDATE OF status ON runs BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(d, p, nil)
	mgr.runtimeStartedAt = time.Now().UTC()
	done := make(chan error, 1)
	go func() {
		done <- mgr.failUnadmittedRunSetup(context.Background(), run, errors.New("queue admission failed"))
	}()
	defer func() {
		if _, err := control.Exec("DROP TRIGGER IF EXISTS fail_unadmitted_terminal"); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("setup settlement did not finish")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mgr.mu.Lock()
		owned := mgr.runs[run.ID]
		mgr.mu.Unlock()
		if owned != nil {
			if owned.handle != nil {
				t.Fatal("unadmitted run acquired a cancel handle")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unadmitted setup did not retain ownership")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request := ipc.ShutdownParams{
		ExpectedPID:       os.Getpid(),
		ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano),
		ExpectedInstance:  mgr.runtimeInstance,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := mgr.prepareControlledShutdown(ctx, request)
	if err == nil || ack != nil || !strings.Contains(err.Error(), "unsettled run "+run.ID) {
		t.Fatalf("shutdown did not refuse the unsettled owned run: %+v, %v", ack, err)
	}
}

// A planned shutdown drains the runs it owns and then stops. A run row that is
// merely present in the database - seeded by a caller, left by a previous
// instance, or written by another tool - was never cancelled or awaited by
// this shutdown, so it must not make the daemon refuse to stop.
func TestPlannedShutdownIgnoresRunsItDoesNotOwn(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	// A synthetic run row with no executor behind it, exactly as the CLI
	// reader and wizard tests leave behind.
	synthetic, err := d.InsertRun(repo.ID, "main", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(synthetic.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	mgr := NewRunManager(d, p, nil)
	mgr.runtimeStartedAt = time.Now().UTC()
	request := ipc.ShutdownParams{
		ExpectedPID:       os.Getpid(),
		ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano),
		ExpectedInstance:  mgr.runtimeInstance,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack, err := mgr.prepareControlledShutdown(ctx, request)
	if err != nil {
		t.Fatalf("a run this daemon does not own blocked the shutdown: %v", err)
	}
	if ack == nil || !ack.OK {
		t.Fatalf("planned shutdown was not acknowledged: %+v", ack)
	}
	// The synthetic row is left exactly as it was: the shutdown neither
	// terminalized it nor claimed it.
	stored, err := d.GetRun(synthetic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunRunning {
		t.Fatalf("a run this daemon does not own was settled by the shutdown: %s", stored.Status)
	}
}
