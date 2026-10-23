package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRunningDocumentPassPicksUpRaisedCommitLimit(t *testing.T) {
	root := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf("repository_overrides:\n  https://github.com/vifar/controller.git:\n    document:\n      max_commits_per_run: %d\n", 3))
	app := paths.WithRoot(root)
	global, err := config.LoadGlobal(app.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MergeForRemote(global, &config.RepoConfig{}, "https://github.com/vifar/controller.git")
	if cfg.Document.MaxCommitsPerRun != 3 {
		t.Fatalf("snapshot limit = %d, want the override 3 (the ignored override left the default %d)", cfg.Document.MaxCommitsPerRun, config.DefaultDocumentMaxCommitsPerRun)
	}
	exec := NewExecutor(nil, app, cfg, nil, nil, nil)
	write(fmt.Sprintf("repository_overrides:\n  https://github.com/vifar/controller.git:\n    document:\n      max_commits_per_run: %d\n", 6))
	exec.refreshDocumentCommitLimit(&db.Repo{UpstreamURL: "git@github.com:vifar/controller.git"})
	if exec.config.Document.MaxCommitsPerRun != 6 {
		t.Fatalf("next Document pass limit = %d, want the raised override 6", exec.config.Document.MaxCommitsPerRun)
	}
	write("log_level: info\n")
	exec.refreshDocumentCommitLimit(&db.Repo{UpstreamURL: "https://github.com/vifar/controller.git"})
	if exec.config.Document.MaxCommitsPerRun != config.DefaultDocumentMaxCommitsPerRun {
		t.Fatalf("limit after removing the override = %d, want the trusted default %d", exec.config.Document.MaxCommitsPerRun, config.DefaultDocumentMaxCommitsPerRun)
	}
}

func TestDocumentRefreshKeepsLimitWhenConfigFileIsAbsent(t *testing.T) {
	app := paths.WithRoot(t.TempDir())
	cfg := &config.Config{Document: config.Document{MaxCommitsPerRun: 4}}
	exec := NewExecutor(nil, app, cfg, nil, nil, nil)
	exec.refreshDocumentCommitLimit(&db.Repo{UpstreamURL: "https://github.com/vifar/controller.git"})
	if exec.config.Document.MaxCommitsPerRun != 4 {
		t.Fatalf("absent config changed the limit to %d", exec.config.Document.MaxCommitsPerRun)
	}
}

// The operator can save an incomplete config while a run is parked. A failed
// reload must preserve its last effective cap, including a directly set cap.
func TestDocumentRefreshKeepsLimitWhenConfigCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		directory bool
	}{
		{name: "malformed YAML", body: "repository_overrides: [\n"},
		{name: "negative override", body: "repository_overrides:\n  https://github.com/vifar/controller.git:\n    document:\n      max_commits_per_run: -1\n"},
		{name: "unreadable file", directory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := paths.WithRoot(t.TempDir())
			var err error
			if tc.directory {
				err = os.Mkdir(app.ConfigFile(), 0o700)
			} else {
				err = os.WriteFile(app.ConfigFile(), []byte(tc.body), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Document: config.Document{MaxCommitsPerRun: 4}}
			exec := NewExecutor(nil, app, cfg, nil, nil, nil)
			exec.refreshDocumentCommitLimit(&db.Repo{UpstreamURL: "https://github.com/vifar/controller.git"})
			if got := cfg.Document.MaxCommitsPerRun; got != 4 {
				t.Fatalf("failed config reload changed the cap to %d, want the previous 4", got)
			}
		})
	}
}

func TestDocumentRefreshRestoresTheTrustedRepositoryCap(t *testing.T) {
	app := paths.WithRoot(t.TempDir())
	global, err := config.LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/vifar/controller.git:\n    document:\n      max_commits_per_run: 6\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := config.LoadRepoFromBytes([]byte("document:\n  max_commits_per_run: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MergeForRemote(global, config.EffectiveRepoConfig(&config.RepoConfig{}, trusted, false), "https://github.com/vifar/controller.git")
	if err := os.WriteFile(app.ConfigFile(), []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(nil, app, cfg, nil, nil, nil)
	exec.refreshDocumentCommitLimit(&db.Repo{UpstreamURL: "git@github.com:vifar/controller.git"})
	if got := cfg.Document.MaxCommitsPerRun; got != 3 {
		t.Fatalf("removing the override left cap %d, want the captured trusted cap 3", got)
	}
}
