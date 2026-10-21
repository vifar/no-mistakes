package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiFailedPushStatusOffersSupportedExit(t *testing.T) {
	f := newCLIRecoverFixture(t)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.UpdateRunErrorStatusWithVerifiedHead(f.runID, "check PR context after push: PR comparison changed after review", types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(f.runID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	// Model a database written by an older executable, after terminalization.
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	t.Logf("status before recovery:\n%s", out)
	if strings.Contains(out, "code: continue_active_run") || !strings.Contains(out, "code: recover_custody") {
		t.Errorf("status must offer recovery instead of pointing at itself:\n%s", out)
	}
	out, err = executeCmd("axi", "sync", "--recover")
	t.Logf("supported recovery command:\n%s", out)
	if err != nil || !strings.Contains(out, "recovered: true") {
		t.Fatalf("axi sync --recover: %v\n%s", err, out)
	}
	out, err = executeCmd("axi", "sync", "--check")
	if err != nil || !strings.Contains(out, "state: custody_returned") {
		t.Fatalf("branch still blocked: %v\n%s", err, out)
	}
	repo, err := d.GetRepo(f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	env := &axiEnv{d: d, repo: repo, p: p, cfg: config.DefaultGlobalConfig()}
	if state := freshRunBranchOwnershipState(context.Background(), env); state != nil {
		t.Fatalf("fresh axi run still refused: %+v", state)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD=%s, want %s", got, f.preserved)
	}
}
