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

// The controller run recorded two Document commits and stopped. Its machine
// config set document.max_commits_per_run to 6 on the repository's upstream
// URL. The effective cap was the built-in default of 2, because merge kept
// the trusted repository value and never applied that override.
func TestDocumentCommitLimitUsesRepositoryOverrideByUpstreamURL(t *testing.T) {
	const remote = "https://github.com/vifar/controller.git"
	global, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/vifar/controller.git:
    document:
      max_commits_per_run: 6
`))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("commands:\n  lint: 'true'\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective := EffectiveRepoConfig(repo, repo, false)
	got := MergeForRemote(global, effective, remote).Document.MaxCommitsPerRun
	if got != 6 {
		t.Fatalf("matching upstream limit = %d, want the override 6 (default is %d)", got, DefaultDocumentMaxCommitsPerRun)
	}
	// Equivalent GitHub URLs identify the same repository.
	for _, alias := range []string{
		"https://github.com/vifar/controller",
		"git@github.com:vifar/controller.git",
	} {
		if aliasGot := MergeForRemote(global, effective, alias).Document.MaxCommitsPerRun; aliasGot != 6 {
			t.Fatalf("upstream %s limit = %d, want 6", alias, aliasGot)
		}
	}
	if other := MergeForRemote(global, effective, "https://github.com/vifar/other.git").Document.MaxCommitsPerRun; other != DefaultDocumentMaxCommitsPerRun {
		t.Fatalf("unmatched upstream limit = %d, want default %d", other, DefaultDocumentMaxCommitsPerRun)
	}

	// An explicit override replaces a trusted repository value, including a
	// raise from the previous override of 3 and a zero cap.
	trustedThree := 3
	trusted := &RepoConfig{Document: DocumentRaw{MaxCommitsPerRun: &trustedThree}}
	if got := MergeForRemote(global, EffectiveRepoConfig(&RepoConfig{}, trusted, false), remote).Document.MaxCommitsPerRun; got != 6 {
		t.Fatalf("override over trusted 3 = %d, want 6", got)
	}
	if got := MergeForRemote(global, EffectiveRepoConfig(&RepoConfig{}, trusted, false), "https://github.com/vifar/other.git").Document.MaxCommitsPerRun; got != 3 {
		t.Fatalf("trusted limit without a matching override = %d, want 3", got)
	}
	zero := 0
	zeroGlobal, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/vifar/controller.git:
    document:
      max_commits_per_run: 0
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := MergeForRemote(zeroGlobal, EffectiveRepoConfig(&RepoConfig{}, trusted, false), remote).Document.MaxCommitsPerRun; got != zero {
		t.Fatalf("explicit zero override = %d, want 0", got)
	}
	if _, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/vifar/controller.git:
    document:
      max_commits_per_run: -1
`)); err == nil {
		t.Fatal("negative repository_overrides document.max_commits_per_run accepted")
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
