package config

import (
	"fmt"
	"testing"
)

func TestDocumentCommitLimitRejectsNegative(t *testing.T) {
	if _, err := LoadRepoFromBytes([]byte("document:\n  max_commits_per_run: -1\n")); err == nil {
		t.Fatal("negative document.max_commits_per_run accepted")
	}
}

func TestDocumentCommitLimitDefaultsAndTrust(t *testing.T) {
	for _, limit := range []int{0, 1, 3} {
		raw, err := LoadRepoFromBytes([]byte(fmt.Sprintf("document:\n  max_commits_per_run: %d\n", limit)))
		if err != nil {
			t.Fatal(err)
		}
		if got := Merge(&GlobalConfig{}, raw).Document.MaxCommitsPerRun; got != limit {
			t.Fatalf("limit=%d, want %d", got, limit)
		}
		pushed := &RepoConfig{Document: DocumentRaw{MaxCommitsPerRun: &limit}}
		for _, optIn := range []bool{false, true} {
			for _, trusted := range []*RepoConfig{nil, {}} {
				resolved := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, trusted, optIn))
				if got := resolved.Document.MaxCommitsPerRun; got != DefaultDocumentMaxCommitsPerRun {
					t.Fatalf("untrusted limit=%d (opt-in %v)", got, optIn)
				}
			}
			resolved := Merge(&GlobalConfig{}, EffectiveRepoConfig(&RepoConfig{}, raw, optIn))
			if got := resolved.Document.MaxCommitsPerRun; got != limit {
				t.Fatalf("trusted limit=%d, want %d", got, limit)
			}
		}
	}
}
