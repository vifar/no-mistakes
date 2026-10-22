package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type refreshingTestOwner struct {
	calls         int
	claim         types.Finding
	beforeRefresh func(*StepContext) error
}

func (s *refreshingTestOwner) Name() types.StepName                         { return types.StepTest }
func (s *refreshingTestOwner) Execute(c *StepContext) (*StepOutcome, error) { return s.proof(c) }
func (s *refreshingTestOwner) RefreshReviewSupport(c *StepContext) (*StepOutcome, error) {
	s.calls++
	if s.beforeRefresh != nil {
		if err := s.beforeRefresh(c); err != nil {
			return nil, err
		}
	}
	c.LogFile(strings.Repeat("x", 1100000))
	c.Log("bounded configured-command output")
	return s.proof(c)
}
func (s *refreshingTestOwner) proof(c *StepContext) (*StepOutcome, error) {
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		return nil, err
	}
	zero := 0
	item := s.claim
	support := *item.Support
	item.Support = &support
	item.ID = types.ReviewSupportClaimID(s.claim)
	item.Category = types.FindingCategoryReviewSupportResolved
	item.Severity = types.FindingSeverityInfo
	item.Support.OwnerResult = &types.FindingOwnerResult{ReviewFindingID: s.claim.ID, HeadSHA: receipt.LocalHeadSHA, TargetSHA: receipt.TargetSHA, DiffDigest: receipt.DiffDigest, Generation: receipt.Generation, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{item}})
	return &StepOutcome{Findings: encoded}, err
}

func TestExecutorRefreshesPendingTestProofAfterForwardDocumentCommit(t *testing.T) {
	testFinalTestSupportRefresh(t, "ordinary")
}

func TestIssue131ProtectsEvidenceRefresh(t *testing.T) {
	for _, mode := range []string{"git-write", "direct-write", "direct-write-error"} {
		t.Run(mode, func(t *testing.T) { testFinalTestSupportRefresh(t, mode) })
	}
}

func testFinalTestSupportRefresh(t *testing.T, mode string) {
	database, p, run, repo := setupTest(t)
	ctx := context.Background()
	seed := t.TempDir()
	initGitRepo(t, seed)
	gate := p.RepoDir(repo.ID)
	if _, err := git.Run(ctx, seed, "push", gate, "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "run")
	if err := git.WorktreeAdd(ctx, gate, dir, "main"); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"user.name": "Test", "user.email": "test@test.com"} {
		if err := git.SetWorktreeConfig(ctx, dir, key, value); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(gate, "config")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{LocalHeadSHA: head, TargetBranch: "main", TargetSHA: head, MergeBaseSHA: head, DiffDigest: fmt.Sprintf("%064d", 1)}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "pending-test", Category: types.FindingCategoryReviewSupportPending, Severity: types.FindingSeverityInfo, Action: types.ActionNoOp, Description: "historical configured test failure", Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "trusted test"}}}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	owner := &refreshingTestOwner{claim: claim}
	if mode != "ordinary" {
		owner.beforeRefresh = func(c *StepContext) error {
			if mode == "git-write" {
				_, err := git.Run(c.Ctx, c.WorkDir, "config", "remote.no-mistakes.url", "https://example.test/escaped.git")
				if err == nil {
					t.Fatal("evidence refresh wrote shared config")
				}
				if !strings.Contains(err.Error(), "could not lock config file") {
					t.Fatalf("write failed without the shared-config lock: %v", err)
				}
				return nil
			}
			if err := os.WriteFile(configPath, append(append([]byte{}, before...), []byte("\n[remote \"no-mistakes\"]\nurl = https://example.test/escaped.git\n")...), 0o600); err != nil {
				return err
			}
			if mode == "direct-write-error" {
				return fmt.Errorf("refresh command failed")
			}
			return nil
		}
	}
	executor := NewExecutor(database, p, &config.Config{}, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(c *StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: encoded, ReviewApprovedHeadSHA: c.Run.HeadSHA}, nil
		}}, owner,
		&adaptiveCallStep{name: types.StepDocument, fn: func(c *StepContext) (*StepOutcome, error) {
			writeTestFile(t, dir, "docs.md", "updated docs\n")
			execGit(t, dir, "add", "docs.md")
			execGit(t, dir, "commit", "-m", "document")
			advanced, err := git.HeadSHA(c.Ctx, dir)
			if err != nil {
				return nil, err
			}
			c.Run.HeadSHA = advanced
			if err := c.DB.UpdateRunHeadSHA(c.Run.ID, advanced); err != nil {
				return nil, err
			}
			receipt.LocalHeadSHA = advanced
			receipt.DiffDigest = fmt.Sprintf("%064d", 2)
			_, err = c.DB.AdvanceRunPRContext(c.Run.ID, receipt)
			return &StepOutcome{}, err
		}}, newPassStep(types.StepCI)}, func(event ipc.Event) {
		if event.Type == ipc.EventLogChunk && event.Content != nil && len(*event.Content) > 65536 {
			t.Errorf("refresh emitted oversized log chunk: %d bytes", len(*event.Content))
		}
	})
	err = executor.Execute(ctx, run, repo, dir)
	if mode == "direct-write" || mode == "direct-write-error" {
		if err == nil || !strings.Contains(err.Error(), "refresh final Test support:") || !strings.Contains(err.Error(), "test shared config boundary") || !strings.Contains(err.Error(), configPath) {
			t.Fatalf("unreported evidence-refresh config pollution: %v", err)
		}
		if mode == "direct-write-error" && !strings.Contains(err.Error(), "refresh command failed") {
			t.Fatalf("refresh error lost: %v", err)
		}
		if owner.calls != 1 {
			t.Fatalf("refresh calls = %d, want 1", owner.calls)
		}
		return
	}
	if err != nil {
		t.Fatalf("forward edit could not finish with fresh proof: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("evidence refresh changed shared config")
	}
	if owner.calls != 1 {
		t.Fatalf("fresh owner executions=%d, want 1", owner.calls)
	}
	log, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "test.log"))
	if err != nil || !strings.Contains(string(log), strings.Repeat("x", 1100000)) {
		t.Fatalf("full configured-command output missing from durable log: %v", err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatal(err)
	}
}
