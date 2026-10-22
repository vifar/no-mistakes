package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The second #100 incident published a generic code-failure status although
// authentication prevented the post-PR context check from reaching CI.
func TestIssue100ExhaustedPRContextTimeoutNamesInfrastructureInStatus(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		timeout       bool
	}{
		{"keyring", "check PR context after pr: read pull request target: PR-context read unavailable after 3 attempts: gh auth status: transient keyring timeout: exit status 1", true},
		{"real_auth", "check PR context after pr: gh CLI is not authenticated", false},
		{"code_failure", "step test failed: scenario failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			if err := sc.DB.UpdateRunErrorStatus(sc.Run.ID, tc.failure, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo"}, "failure", false)
			if len(requests) != 1 || requests[0].State != "failure" {
				t.Fatalf("requests=%+v", requests)
			}
			got := requests[0].Description
			if tc.timeout {
				if !strings.Contains(got, "PR-context") || !strings.Contains(got, "transient keyring timeout") || strings.Contains(got, "validation failed") {
					t.Fatalf("misleading description=%q", got)
				}
			} else if got != "no-mistakes pipeline failed or cancelled" {
				t.Fatalf("unrelated classification changed: %q", got)
			}
		})
	}
}
