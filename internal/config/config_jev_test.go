package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadGlobal_JevDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Jev.ReviewAssist {
		t.Fatal("jev.review_assist must default to false: the assist is opt-in")
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.Jev.ReviewAssist {
		t.Fatal("merged jev.review_assist must default to false")
	}
}

func TestLoadGlobal_JevReviewAssist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("jev:\n  review_assist: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if !cfg.Jev.ReviewAssist {
		t.Fatal("jev.review_assist = false, want true")
	}
	merged := Merge(cfg, &RepoConfig{})
	if !merged.Jev.ReviewAssist {
		t.Fatal("merged jev.review_assist = false, want the global value copied straight through")
	}
}

// TestRepoConfig_JevKeyIsNotARepoField pins the trust boundary: jev settings
// are global-only, so a pushed branch cannot enable or steer the pre-screen
// that feeds the reviewer gating it. RepoConfig ignores unknown keys, which
// is exactly what makes a repo-level jev: block inert.
func TestRepoConfig_JevKeyIsNotARepoField(t *testing.T) {
	var repo RepoConfig
	if err := yaml.Unmarshal([]byte("jev:\n  review_assist: true\n  candidate_excerpt_bytes: 2048\n"), &repo); err != nil {
		t.Fatalf("repo config with a jev key must stay parseable (it is ignored): %v", err)
	}
	global := DefaultGlobalConfig()
	merged := Merge(global, &repo)
	if merged.Jev.ReviewAssist {
		t.Fatal("a repository configuration must not be able to enable the jev assist")
	}
	if merged.Jev.CandidateExcerptBytes != 0 {
		t.Fatal("a repository configuration must not be able to enable candidate excerpts")
	}
}

func TestLoadGlobal_JevCandidateExcerptBytesDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Jev.CandidateExcerptBytes != 0 {
		t.Fatalf("jev.candidate_excerpt_bytes = %d, want 0 (path-only candidates)", cfg.Jev.CandidateExcerptBytes)
	}
}

func TestLoadGlobal_JevCandidateExcerptBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("jev:\n  candidate_excerpt_bytes: 2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Jev.CandidateExcerptBytes != 2048 {
		t.Fatalf("jev.candidate_excerpt_bytes = %d, want 2048", cfg.Jev.CandidateExcerptBytes)
	}
	if cfg.Jev.ReviewAssist {
		t.Fatal("setting the excerpt budget must not enable the assist")
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.Jev.CandidateExcerptBytes != 2048 {
		t.Fatalf("merged jev.candidate_excerpt_bytes = %d, want the global value copied straight through", merged.Jev.CandidateExcerptBytes)
	}
}

func TestLoadGlobal_JevCandidateExcerptBytesNegativeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("jev:\n  candidate_excerpt_bytes: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("negative jev.candidate_excerpt_bytes must fail the config closed")
	}
}
