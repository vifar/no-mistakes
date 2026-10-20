package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
