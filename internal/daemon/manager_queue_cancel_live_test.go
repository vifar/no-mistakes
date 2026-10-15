package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Terminal status promises that work has stopped. Drive the public cancellation
// RPC with a real agent subprocess that acknowledges SIGTERM but waits for the
// driver to release it. A premature terminal write would violate that promise
// even though ordinary fast-exiting agents could hide it.
func TestQueueCancellationWaitsForLiveAgentProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external agent fixture uses a POSIX shell")
	}
	for _, launch := range []string{"inline", "queued"} {
		t.Run(launch, func(t *testing.T) {
			root, err := os.MkdirTemp("", "qcancel")
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
			_, head := setupTestGitRepo(t, p, database, "cancel-live")
			claude := filepath.Join(root, "claude")
			script := fmt.Sprintf(`#!/bin/sh
run=${PWD##*/}
trap 'touch "%s/$run.cancelled"; while [ ! -f "%s/$run.release" ]; do sleep 0.02; done; exit 1' TERM
echo $$ > "%s/$run.started"
while [ ! -f "%s/$run.release" ]; do sleep 0.02; done
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"summary":"clean"}}'
`, root, root, root, root)
			if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %s\ndaemon:\n  max_concurrent_runs: 1\n", claude)
			if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			runTestDaemon(t, p, database, func() []pipeline.Step {
				return []pipeline.Step{&liveAgentWorkload{}}
			}, 20*time.Second)
			f := &schedFixture{t: t, p: p, d: database, repoID: "cancel-live", head: head}
			f.dial()
			waitFile := func(id, suffix string) {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(root, id+suffix)); err == nil {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("agent did not write %s%s", id, suffix)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			release := func(id string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, id+".release"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var holder string
			if launch == "queued" {
				holder, _ = f.push("holder")
				waitFile(holder, ".started")
			}
			id, _ := f.push("cancel-subject")
			if holder != "" {
				f.assertQueued(id, 1, 1)
				release(holder)
			}
			waitFile(id, ".started")
			if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: id}, nil); err != nil {
				t.Fatal(err)
			}
			waitFile(id, ".cancelled")
			// Give a premature terminal write time to become observable while
			// the agent is demonstrably blocked in its termination handler.
			time.Sleep(100 * time.Millisecond)
			info := f.info(id)
			if info.Status.Terminal() {
				t.Fatalf("run became %s before the agent process exited", info.Status)
			}
			t.Logf("cancel_run acknowledged; agent acknowledged SIGTERM and is still waiting; get_run status=%s", info.Status)
			release(id)
			f.waitStatus(id, types.RunCancelled)
			if holder != "" {
				f.waitStatus(holder, types.RunCompleted)
			}
			t.Logf("after releasing the agent process: get_run status=%s; slots=%+v", f.info(id).Status, f.slots())
		})
	}
}
