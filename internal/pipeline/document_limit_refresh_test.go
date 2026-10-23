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
