package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDaemonDefaultsWithoutSetup(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := Daemon{MaxConcurrentRuns: DefaultMaxConcurrentRuns}
	if cfg.Daemon != want || DefaultMaxConcurrentRuns != 3 {
		t.Fatalf("daemon defaults = %#v (default cap %d), want %#v with cap 3", cfg.Daemon, DefaultMaxConcurrentRuns, want)
	}
}

func TestDaemonSettingsAreConfigurable(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("daemon:\n  max_concurrent_runs: 5\n  hold_slot_while_parked: true\n  max_concurrent_agents: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Daemon{MaxConcurrentRuns: 5, HoldSlotWhileParked: true, MaxConcurrentAgents: 2}
	if cfg.Daemon != want {
		t.Fatalf("daemon config = %#v, want %#v", cfg.Daemon, want)
	}
}

// An explicit zero means unlimited and must survive the "not set" check
// instead of falling back to the default cap.
func TestDaemonMaxConcurrentRunsZeroIsUnlimited(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("daemon:\n  max_concurrent_runs: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.MaxConcurrentRuns != 0 {
		t.Fatalf("max_concurrent_runs = %d, want 0 (unlimited)", cfg.Daemon.MaxConcurrentRuns)
	}
}

func TestDaemonNegativeLimitsAreRejected(t *testing.T) {
	for _, body := range []string{
		"daemon:\n  max_concurrent_runs: -1\n",
		"daemon:\n  max_concurrent_agents: -2\n",
	} {
		_, err := LoadGlobalFromBytes([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "daemon.max_concurrent_") {
			t.Fatalf("config %q error = %v, want a daemon.max_concurrent_* rejection", body, err)
		}
	}
}

func TestDaemonUnknownKeyIsRejected(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("daemon:\n  max_concurrent_run: 2\n")); err == nil {
		t.Fatal("a misspelled daemon key must fail the strict global config parse")
	}
}

// The daemon block is global-only: a pushed branch's .no-mistakes.yaml cannot
// widen or shrink the machine's run capacity. RepoConfig has no such field, so
// the key is inert there and never reaches the merged per-run config.
func TestDaemonSettingsAreNotARepoSetting(t *testing.T) {
	dir := t.TempDir()
	data := "daemon:\n  max_concurrent_runs: 99\n  hold_slot_while_parked: true\n"
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	global, err := LoadGlobalFromBytes([]byte("daemon:\n  max_concurrent_runs: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if global.Daemon.MaxConcurrentRuns != 2 {
		t.Fatalf("global cap = %d, want 2", global.Daemon.MaxConcurrentRuns)
	}
	if repo.Agent != "" || repo.Commands.Test != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", repo)
	}
}

func TestSetGlobalDaemonSettingPreservesTheRestOfTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
	}{
		{"missing file", "", "daemon:\n  max_concurrent_runs: 7\n"},
		{"no daemon block", "agent: claude # keep\n", "agent: claude # keep\n\ndaemon:\n  max_concurrent_runs: 7\n"},
		{"commented template", "# daemon:\n#   max_concurrent_runs: 3\nlog_level: info\n", "# daemon:\n#   max_concurrent_runs: 3\nlog_level: info\n\ndaemon:\n  max_concurrent_runs: 7\n"},
		{"replace value", "daemon:\n  max_concurrent_runs: 3 # cap\n  hold_slot_while_parked: true\nlog_level: info\n", "daemon:\n  max_concurrent_runs: 7 # cap\n  hold_slot_while_parked: true\nlog_level: info\n"},
		{"add key to block", "daemon:\n    hold_slot_while_parked: true\nlog_level: info\n", "daemon:\n    hold_slot_while_parked: true\n    max_concurrent_runs: 7\nlog_level: info\n"},
		{"empty block", "daemon:\nlog_level: info\n", "daemon:\n  max_concurrent_runs: 7\nlog_level: info\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tc.before != "" {
				if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := SetGlobalDaemonSetting(path, "max_concurrent_runs", "7"); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.after {
				t.Fatalf("config =\n%q\nwant\n%q", got, tc.after)
			}
			cfg, err := LoadGlobal(path)
			if err != nil || cfg.Daemon.MaxConcurrentRuns != 7 {
				t.Fatalf("reloaded cap = %v, %v", cfg.Daemon, err)
			}
		})
	}
}

func TestSetGlobalDaemonSettingRefusesFlowForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("daemon: {max_concurrent_runs: 3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetGlobalDaemonSetting(path, "max_concurrent_runs", "7"); err == nil {
		t.Fatal("a flow-style daemon block must be refused, not rewritten")
	}
	if err := SetGlobalDaemonSetting(filepath.Join(t.TempDir(), "c.yaml"), "max_concurrent_runs", "-1"); err == nil {
		t.Fatal("a negative cap must be rejected before it is written")
	}
}

func TestSetGlobalDaemonSettingPreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	for _, mode := range []os.FileMode{0o600, 0o640, 0o644} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("daemon:\n  max_concurrent_runs: 3\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // beat the umask
			t.Fatal(err)
		}
		if err := SetGlobalDaemonSetting(path, "max_concurrent_runs", "4"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("mode after rewrite = %o, want the original %o", info.Mode().Perm(), mode)
		}
	}
}

func TestSetGlobalDaemonSettingCreatesNewFilePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := SetGlobalDaemonSetting(path, "max_concurrent_runs", "4"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new config mode = %o, want 600", info.Mode().Perm())
	}
}

// A config that is a symlink is updated through the link: the target is
// replaced atomically with its own mode, and the link stays a link.
func TestSetGlobalDaemonSettingUpdatesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlinks and modes")
	}
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "dotfiles")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "no-mistakes.yaml")
	if err := os.WriteFile(target, []byte("# mine\ndaemon:\n  max_concurrent_runs: 3\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := SetGlobalDaemonSetting(link, "max_concurrent_runs", "9"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the config symlink was replaced: %v, %v", info, err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Fatalf("link now points at %q, want %q", got, target)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "# mine\ndaemon:\n  max_concurrent_runs: 9\n" {
		t.Fatalf("target = %q", data)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Fatalf("target mode = %o, want 640", info.Mode().Perm())
	}
	// A dangling link is refused rather than replaced by a regular file.
	dangling := filepath.Join(dir, "dangling.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing", "x.yaml"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := SetGlobalDaemonSetting(dangling, "max_concurrent_runs", "2"); err == nil {
		t.Fatal("a dangling config symlink was rewritten")
	}
	if info, err := os.Lstat(dangling); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dangling symlink was replaced: %v, %v", info, err)
	}
}
