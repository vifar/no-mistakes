package config

import "testing"

// document.doc_only_paths decides which commits skip Review revalidation, so a
// pushed branch must never widen it for its own run.
func TestEffectiveRepoConfig_DocOnlyPathsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Document: DocumentRaw{DocOnlyPaths: []string{"**"}}}
	trusted := &RepoConfig{Document: DocumentRaw{DocOnlyPaths: []string{"documentation/**"}}}
	if got := EffectiveRepoConfig(pushed, trusted, true).Document.DocOnlyPaths; len(got) != 1 || got[0] != "documentation/**" {
		t.Fatalf("doc_only_paths = %v, want the trusted copy", got)
	}
	if got := EffectiveRepoConfig(pushed, nil, true).Document.DocOnlyPaths; len(got) != 0 {
		t.Fatalf("doc_only_paths = %v without a trusted copy, want none", got)
	}
}

func TestParseRepoConfig_DocOnlyPaths(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("document:\n  doc_only_paths:\n    - \" documentation/** \"\n    - \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved := Merge(&GlobalConfig{}, cfg)
	if got := resolved.Document.DocOnlyPaths; len(got) != 1 || got[0] != "documentation/**" {
		t.Fatalf("resolved doc_only_paths = %q, want trimmed non-empty patterns", got)
	}
}
