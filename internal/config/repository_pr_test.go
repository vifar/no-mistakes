package config

import (
	"fmt"
	"testing"
)

func TestRepositoryPRPublicationPrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, override, trusted, remote, template, appendix string
	}{
		{"override applies", "template: operator.md\n      appendix: minimal", "", "widget", "operator.md", PRAppendixMinimal},
		{"trusted policy wins", "template: operator.md\n      appendix: minimal", "template: maintainer.md\n  appendix: full", "widget", "maintainer.md", PRAppendixFull},
		{"trusted template only", "template: operator.md\n      appendix: minimal", "template: maintainer.md", "widget", "maintainer.md", PRAppendixMinimal},
		{"trusted appendix only", "template: operator.md\n      appendix: minimal", "appendix: collapsed", "widget", "operator.md", PRAppendixCollapsed},
		{"unset override", "{}", "", "widget", "", PRAppendixFull},
		{"unset override preserves repo", "{}", "template: maintainer.md\n  appendix: collapsed", "widget", "maintainer.md", PRAppendixCollapsed},
		{"unmatched remote", "template: operator.md\n      appendix: minimal", "", "other", "", PRAppendixFull},
		{"appendix only", "appendix: collapsed", "", "widget", "", PRAppendixCollapsed},
		{"template only", "template: operator.md", "", "widget", "operator.md", PRAppendixFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget.git:\n    pr:\n      " + tc.override + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := LoadRepoFromBytes([]byte("pr:\n  " + tc.trusted + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			yes := true
			pushed := &RepoConfig{PR: PRRaw{Template: "pushed.md", Appendix: PRAppendixMinimal, PublishIntent: &yes}}
			for _, allow := range []bool{false, true} {
				cfg := MergeForRemote(global, EffectiveRepoConfig(pushed, trusted, allow), "git@github.com:acme/"+tc.remote+".git")
				if cfg.PR.Template != tc.template || cfg.PR.AppendixMode() != tc.appendix || cfg.PR.PublishIntent != nil {
					t.Fatalf("allow=%v: PR = %+v, want template %q, appendix %q, no pushed publication policy", allow, cfg.PR, tc.template, tc.appendix)
				}
			}
		})
	}
}

func TestRepositoryPRValidation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", ".github/pull_request_template.md", "docs/PR template.md", "templates/[team].md"} {
		for _, mode := range []string{"", PRAppendixFull, PRAppendixCollapsed, PRAppendixMinimal} {
			data := fmt.Sprintf("repository_overrides:\n  https://github.com/acme/widget:\n    pr:\n      template: %q\n      appendix: %q\n", name, mode)
			if _, err := LoadGlobalFromBytes([]byte(data)); err != nil {
				t.Errorf("valid template %q, appendix %q: %v", name, mode, err)
			}
		}
	}
	for _, name := range []string{".", "..", "../outside", "a/../../outside", "/etc/passwd", `C:\Users\who\x`, `a\b`, "https://example.test/a", "main:x", "./a", "a//b", "a/", ".git/config", "a/.git/x", " x", "x\n", "a\x00b", "-template"} {
		data := fmt.Sprintf("repository_overrides:\n  https://github.com/acme/widget:\n    pr:\n      template: %q\n", name)
		if _, err := LoadGlobalFromBytes([]byte(data)); err == nil {
			t.Errorf("unsafe template %q accepted", name)
		}
	}
	for _, field := range []string{"appendix: hidden", "appendix: FULL", "appendix: none", "appendix: off", "publish_intent: true", "base_branch: main"} {
		if _, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget:\n    pr:\n      " + field + "\n")); err == nil {
			t.Errorf("invalid or out-of-scope field %q accepted", field)
		}
	}
}
