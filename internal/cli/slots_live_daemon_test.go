package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Read each public status surface against a real daemon, without replacing
// fetchSlots. The documented default and an applied cap are the oracle.
func TestSlotStatusSurfacesWithLiveDaemon(t *testing.T) {
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repoDir := t.TempDir()
	run(t, repoDir, "git", "init", "-b", "main")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	repoDir, err = filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepoWithID("slots-live", repoDir, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	chdir(t, repoDir)
	// Keep doctor from probing any operator agent or forge configuration.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startTestDaemon(t, p, database)
	for _, cap := range []int{3, 1} {
		if cap == 1 {
			if _, err := executeCmd("daemon", "set", "max-concurrent-runs", "1"); err != nil {
				t.Fatal(err)
			}
		}
		want := "0/" + strconv.Itoa(cap) + " in use, 0 queued"
		for _, args := range [][]string{{"axi", "status"}, {"axi"}, {"doctor"}} {
			out, err := executeCmd(args...)
			t.Logf("$ no-mistakes %s\n%s", strings.Join(args, " "), out)
			if !strings.Contains(out, want) {
				t.Fatalf("public surface omitted live slot usage %q (error %v)", want, err)
			}
			if args[0] != "doctor" && err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The CLI must persist the requested cap and apply it to a running daemon.
// Unlike the protocol fixture test, this executes the real CLI guard, config
// writer, daemon handlers and scheduler. Keeping one IPC connection open also
// rejects an implementation that applies settings by restarting the daemon.
func TestDaemonSetMaxConcurrentRunsWithLiveDaemon(t *testing.T) {
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	startTestDaemon(t, p, database)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, cap := range []int{5, 1, 0} {
		out, err := executeCmd("daemon", "set", "max-concurrent-runs", strconv.Itoa(cap))
		t.Logf("$ no-mistakes daemon set max-concurrent-runs %d\n%s", cap, out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "applied: slots") {
			t.Fatal("CLI did not confirm the running daemon applied the setting")
		}
		var usage ipc.SlotsResult
		if err := client.Call(ipc.MethodGetSlots, nil, &usage); err != nil {
			t.Fatalf("original daemon connection stopped answering: %v", err)
		}
		if usage.MaxConcurrentRuns != cap {
			t.Fatalf("live daemon cap = %d, want %d", usage.MaxConcurrentRuns, cap)
		}
		cfg, err := config.LoadGlobal(p.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Daemon.MaxConcurrentRuns != cap {
			t.Fatalf("persisted cap = %d, want %d", cfg.Daemon.MaxConcurrentRuns, cap)
		}
		t.Logf("get_slots over original connection: %+v", usage)
	}
	for _, invalid := range []string{"-1", "many", "2.5"} {
		out, err := executeCmd("daemon", "set", "max-concurrent-runs", "--", invalid)
		t.Logf("$ no-mistakes daemon set max-concurrent-runs -- %s\n%s\nerror: %v", invalid, out, err)
		if err == nil {
			t.Fatal("invalid setting was accepted")
		}
		var usage ipc.SlotsResult
		if err := client.Call(ipc.MethodGetSlots, nil, &usage); err != nil {
			t.Fatal(err)
		}
		cfg, loadErr := config.LoadGlobal(p.ConfigFile())
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if usage.MaxConcurrentRuns != 0 || cfg.Daemon.MaxConcurrentRuns != 0 {
			t.Fatal("rejected input changed the last valid cap")
		}
	}
}
