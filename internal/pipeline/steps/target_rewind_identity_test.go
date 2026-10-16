package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Continued PR absence is required through integration and its successful bind.
// An unpublished local commit makes real discovery leave the new PR unattached.
func TestTargetRewindRejectsNewPR(t *testing.T) {
	for _, attached := range []bool{false, true} {
		for _, phase := range []string{"before-rebase", "parked", "integration", "fix-integration", "completion"} {
			t.Run(map[bool]string{false: "unattached", true: "attached"}[attached]+"/"+phase, func(t *testing.T) {
				c, selected := rewindFixture(t)
				remote := c.Repo.UpstreamURL
				forgeURL := "https://github.com/test/repo"
				gitCmd(t, c.WorkDir, "config", "url."+remote+".insteadOf", forgeURL)
				c.Repo.UpstreamURL = forgeURL
				c.Repo.URLsVerified = true
				c.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
				env, _ := fakeGH(t, "")
				c.Env = append(env, "FAKE_CLI_PR_FACTS_LIST_JSON=[[]]")
				var selectionErr error
				selected, selectionErr = resolvePRTargetWithReader(c, &fakePRFactsReader{}, false)
				if selectionErr != nil {
					t.Fatal(selectionErr)
				}
				if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
					t.Fatal(err)
				}
				remoteHead := c.Run.HeadSHA
				if !attached {
					gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "owned unpublished fix")
					c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
					if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
						t.Fatal(err)
					}
					c.PRContextAfterStep = true
					if _, err := guardPRContextWithSelection(c, types.StepDocument, selected, false); err != nil {
						t.Fatal(err)
					}
					c.PRContextAfterStep = false
				}
				if attached && phase == "completion" {
					gitCmd(t, c.WorkDir, "push", "origin", "feature:main")
				} else {
					advanceRewindTarget(t, c)
				}
				if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err != nil {
					t.Fatal(err)
				}
				if phase == "completion" {
					if out, err := (&RebaseStep{}).Execute(c); err != nil || out.NeedsApproval {
						t.Fatalf("integrate: %+v %v", out, err)
					}
				}
				// Discovery of a new PR at the unchanged remote source tip, while local
				// unpublished commits mean the resolver leaves its PRURL unattached.
				selected, selectionErr = resolvePRTargetWithReader(c, &fakePRFactsReader{list: []scm.PRFacts{{PR: scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}, State: scm.PRStateOpen, SourceRepository: "test/repo", SourceBranch: "feature", HeadSHA: remoteHead, BaseBranch: "main"}}}, false)
				if selectionErr != nil || (selected.PRURL != "") != attached || selected.ForgeHeadSHA != remoteHead {
					t.Fatalf("unexpected discovery %+v: %v", selected, selectionErr)
				}
				before, err := c.DB.GetPendingTargetRewind(c.Run.ID)
				if err != nil || before == nil {
					t.Fatalf("missing baseline: %+v %v", before, err)
				}
				receipt, err := c.DB.GetRunPRContext(c.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				c.PRContextAfterStep = phase == "parked" || phase == "completion"
				c.PRContextRebaseCompleted = phase == "completion"
				if phase == "integration" || phase == "fix-integration" {
					c.Env = append(env, `FAKE_CLI_PR_FACTS_LIST_JSON=[[{"number":42,"html_url":"https://github.com/test/repo/pull/42","state":"open","head":{"ref":"feature","sha":"`+remoteHead+`","repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}]]`)
					c.Fixing = phase == "fix-integration"
					_, err = (&RebaseStep{}).Execute(c)
				} else {
					_, err = guardPRContextWithSelection(c, types.StepRebase, selected, false)
				}
				if err == nil {
					pending, readErr := c.DB.GetPendingTargetRewind(c.Run.ID)
					t.Fatalf("accepted newly discovered PR during %s: pending=%+v readErr=%v", phase, pending, readErr)
				}
				if !strings.Contains(err.Error(), "PR identity changed") && !strings.Contains(err.Error(), "PR comparison changed") && !strings.Contains(err.Error(), "pending Rebase completion") {
					t.Fatalf("wrong refusal: %v", err)
				}
				after, readErr := c.DB.GetPendingTargetRewind(c.Run.ID)
				if readErr != nil || after == nil || after.ID != before.ID {
					t.Fatalf("refusal consumed pending integration: %+v %v", after, readErr)
				}
				got, readErr := c.DB.GetRunPRContext(c.Run.ID)
				if readErr != nil || got == nil || *got != *receipt {
					t.Fatalf("refusal changed receipt: %+v %v", got, readErr)
				}
				if err = c.DB.UpdateRunStatus(c.Run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				if p, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || p != nil {
					t.Fatalf("terminal refusal retained pending state: %+v %v", p, err)
				}
			})
		}
	}
}
