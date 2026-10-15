package config

import (
	"reflect"
	"strings"
	"testing"
)

func resolveCI(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) CI {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes(%q): %v", globalYAML, err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(trusted %q): %v", trustedRepoYAML, err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(pushed %q): %v", pushedRepoYAML, err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).CI
}

func TestCICheckScope_DefaultsToAutoWithNoPatterns(t *testing.T) {
	t.Parallel()
	ci := resolveCI(t, "{}\n", "{}\n", "{}\n")
	if ci.RequiredChecks != CIRequiredChecksAuto || len(ci.MustPass) != 0 || len(ci.ExternalChecks) != 0 {
		t.Fatalf("defaults = %+v, want auto with no patterns", ci)
	}
}

func TestCICheckScope_RepositoryOverridesGlobal(t *testing.T) {
	t.Parallel()
	global := "ci:\n  required_checks: all\n  must_pass: [lint]\n  external_checks: [approval-status]\n"
	repo := "ci:\n  required_checks: auto\n  must_pass: ['test*', ' ', test*]\n  external_checks: []\n"
	ci := resolveCI(t, global, "{}\n", "{}\n")
	if ci.RequiredChecks != CIRequiredChecksAll || !reflect.DeepEqual(ci.MustPass, []string{"lint"}) || !reflect.DeepEqual(ci.ExternalChecks, []string{"approval-status"}) {
		t.Fatalf("global-only = %+v", ci)
	}
	ci = resolveCI(t, global, repo, "{}\n")
	if ci.RequiredChecks != CIRequiredChecksAuto || !reflect.DeepEqual(ci.MustPass, []string{"test*"}) || len(ci.ExternalChecks) != 0 {
		t.Fatalf("repo over global = %+v: lists replace, blanks and duplicates drop, [] clears", ci)
	}
}

// The scope decides what a head must pass, so like the rest of the ci block it
// comes from the trusted default branch: a pushed branch cannot declare its
// failing checks external.
func TestCICheckScope_TrustedOnly(t *testing.T) {
	t.Parallel()
	pushed := "ci:\n  required_checks: all\n  external_checks: ['*']\n  must_pass: [x]\n"
	ci := resolveCI(t, "{}\n", "{}\n", pushed)
	if ci.RequiredChecks != CIRequiredChecksAuto || len(ci.ExternalChecks) != 0 || len(ci.MustPass) != 0 {
		t.Fatalf("pushed branch changed the check scope: %+v", ci)
	}
}

func TestCICheckScope_RejectsUnknownRequiredChecks(t *testing.T) {
	t.Parallel()
	const typo = "ci:\n  required_checks: required\n"
	if _, err := LoadGlobalFromBytes([]byte(typo)); err == nil || !strings.Contains(err.Error(), "ci.required_checks") {
		t.Errorf("global: err = %v, want a ci.required_checks error", err)
	}
	if _, err := LoadRepoFromBytes([]byte(typo)); err == nil || !strings.Contains(err.Error(), "ci.required_checks") {
		t.Errorf("repo: err = %v, want a ci.required_checks error", err)
	}
}

func TestCICheckScope_PolicyCopiesPatterns(t *testing.T) {
	t.Parallel()
	ci := CI{RequiredChecks: "auto", ReviewBotComments: CIReviewBotCommentsAlways, MustPass: []string{"a"}, ExternalChecks: []string{"b"}}
	policy := ci.CheckScopePolicy()
	if policy.ReviewBotComments != CIReviewBotCommentsAlways {
		t.Fatalf("policy lost review-bot setting: %+v", policy)
	}
	policy.MustPass[0], policy.ExternalChecks[0] = "x", "y"
	if ci.MustPass[0] != "a" || ci.ExternalChecks[0] != "b" {
		t.Fatalf("policy aliases the config: %+v", ci)
	}
}
