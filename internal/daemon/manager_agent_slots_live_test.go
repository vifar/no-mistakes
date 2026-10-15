package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This workload invokes the production agent adapter. Only the external
// Claude executable is disposable; admission, config reload, IPC and child
// process management run in the real daemon.
type liveAgentWorkload struct{}

func (*liveAgentWorkload) Name() types.StepName { return types.StepReview }

func (*liveAgentWorkload) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "disposable concurrency workload", CWD: sctx.WorkDir}); err != nil {
		return nil, err
	}
	if err := bindMockRunComparison(sctx); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func TestDaemonAgentCapAndHotReloadWithLiveSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external agent fixture uses a POSIX shell")
	}
	root, err := os.MkdirTemp("", "agentq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, _ := setupTestGitRepo(t, p, database, "agent-cap")
	// A repository cannot override machine-wide admission or agent limits.
	repoConfig := "daemon:\n  max_concurrent_runs: 99\n  hold_slot_while_parked: true\n  max_concurrent_agents: 99\n"
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(repoConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "try repository daemon overrides")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	marker := filepath.Join(root, "started")
	release := filepath.Join(root, "release")
	claude := filepath.Join(root, "claude")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s:%%s\\n' \"$$\" \"$PWD\" >> '%s'\nwhile [ ! -f '%s' ]; do sleep 0.05; done\nprintf '%%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"findings\":[],\"summary\":\"clean\"}}'\n", marker, release)
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCap := func(n int) {
		t.Helper()
		body := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %s\ndaemon:\n  max_concurrent_runs: 3\n  max_concurrent_agents: %d\n", claude, n)
		if err := os.WriteFile(p.ConfigFile(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCap(1)
	runTestDaemon(t, p, database, func() []pipeline.Step { return []pipeline.Step{&liveAgentWorkload{}} }, 30*time.Second)
	f := &schedFixture{t: t, p: p, d: database, repoID: "agent-cap", head: head}
	f.dial()
	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := f.push(fmt.Sprintf("agent-%d", i))
		ids = append(ids, id)
	}
	started := func() []string {
		data, _ := os.ReadFile(marker)
		return strings.Fields(string(data))
	}
	waitCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for len(started()) != n {
			if time.Now().After(deadline) {
				t.Fatalf("agent subprocesses = %v, want %d", started(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitCount(1)
	time.Sleep(250 * time.Millisecond)
	if len(started()) != 1 {
		t.Fatalf("agent cap one admitted extra subprocesses: %v", started())
	}
	if usage := f.slots(); usage.InUse != 3 || usage.MaxConcurrentRuns != 3 || usage.HoldSlotWhileParked {
		t.Fatalf("expected three admitted runs, got %+v", usage)
	}
	t.Logf("agent cap 1: actual child processes %v; daemon slots %+v", started(), f.slots())
	writeCap(2)
	f.reloadSlots()
	waitCount(2)
	time.Sleep(250 * time.Millisecond)
	if len(started()) != 2 {
		t.Fatalf("agent cap two admitted extra subprocesses: %v", started())
	}
	t.Logf("agent cap 2 after live reload: actual child processes %v", started())
	writeCap(0)
	f.reloadSlots()
	waitCount(3)
	t.Logf("agent cap 0 after live reload: actual child processes %v", started())
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		f.waitStatus(id, types.RunCompleted)
	}
}
