package steps

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRetainedTargetTerminalInclusion(t *testing.T) {
	for _, mode := range []string{"merge", "squash", "rebase"} {
		for _, entry := range []bool{true, false} {
			for _, kind := range []string{"rollback", "included", "no retained"} {
				t.Run(fmt.Sprintf("%s/entry=%t/%s", mode, entry, kind), func(t *testing.T) {
					c, selection, receipt := retainedTargetFixture(t)
					first := ""
					if kind != "no retained" {
						gitCmd(t, c.WorkDir, "checkout", "main")
						gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "empty T1")
						first = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
						gitCmd(t, c.WorkDir, "checkout", "feature")
						if _, err := guardPRContextWithSelection(c, types.StepCI, selection, false); err != nil {
							t.Fatal(err)
						}
					}
					gitCmd(t, c.WorkDir, "checkout", "main")
					if kind == "rollback" {
						gitCmd(t, c.WorkDir, "reset", "--hard", receipt.TargetSHA)
					}
					switch mode {
					case "merge":
						gitCmd(t, c.WorkDir, "merge", "--no-ff", "feature", "-m", "merged")
					case "squash":
						gitCmd(t, c.WorkDir, "merge", "--squash", "feature")
						gitCmd(t, c.WorkDir, "commit", "-m", "squashed")
					case "rebase":
						for _, commit := range strings.Fields(gitCmd(t, c.WorkDir, "rev-list", "--reverse", receipt.TargetSHA+".."+receipt.LocalHeadSHA)) {
							gitCmd(t, c.WorkDir, "-c", "user.name=Merge bot", "cherry-pick", commit)
						}
					}
					merged := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
					wantError := kind == "rollback"
					if first != "" {
						_, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", first, merged)
						if (err != nil) != wantError {
							t.Fatalf("invalid Git ancestry oracle: %v", err)
						}
					}
					if wantError && gitCmd(t, c.WorkDir, "rev-parse", merged+"^{tree}") != gitCmd(t, c.WorkDir, "rev-parse", receipt.LocalHeadSHA+"^{tree}") {
						t.Fatal("rollback must preserve the original result tree")
					}
					gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "later target")
					gitCmd(t, c.WorkDir, "checkout", "feature")
					state := "merged"
					if !entry {
						c.Run.PRState = &state
					}
					facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":true,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, selection.PRURL, merged, receipt.LocalHeadSHA)
					bin := fakeCLIBinDir(t)
					linkTestBinary(t, bin, "gh")
					c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh-reconcile", "FAKE_CLI_PR_FACTS_JSON": facts, "FAKE_CLI_PROTECTION_JSON": `{"strict":false}`, "FAKE_CLI_RULES_JSON": `[]`})
					c.PRContextAfterStep = !entry
					_, err := GuardPRContext(c, types.StepCI)
					if (err != nil) != wantError {
						t.Fatalf("terminal error=%v; want refusal=%t", err, wantError)
					}
					if after, e := c.DB.GetRunPRContext(c.Run.ID); e != nil || *after != *receipt {
						t.Fatalf("receipt changed: %+v %v", after, e)
					}
				})
			}
		}
	}
}
