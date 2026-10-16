//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CI check-scope live-validation journeys. Each test drives the real
// no-mistakes binary and daemon through the CI step against an owned fake
// forge: the stateful gh stub (cmd/fakeagent/gh_stateful.go) answers the commit
// check rollup, branch protection, rulesets, Actions runs, and review threads
// from a forge file this test rewrites atomically, so a poll sees whatever the
// test has set at that moment. Nothing leaves the machine.
//
// Run them with:
//
//	go test -tags e2e ./internal/e2e -run 'TestCIScope' -count=1 -timeout 30m
//
// The CI poll interval is not configurable (30s, and a 5-minute floor for
// strict shipping runs), so every journey is decided on the monitor's first
// poll except where it deliberately waits for one more (cache reuse, a stale
// attempt clearing).

// forgeCheck, forgeEndpoint, forgeRules, and forgeState mirror the fake forge's
// JSON (cmd/fakeagent/gh_stateful_forge.go).
type forgeCheck struct {
	DatabaseID  int64  `json:"database_id,omitempty"`
	Name        string `json:"name"`
	Status      string `json:"status,omitempty"`
	Conclusion  string `json:"conclusion,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	DetailsURL  string `json:"details_url,omitempty"`
	AppSlug     string `json:"app_slug,omitempty"`
	AppID       int64  `json:"app_id,omitempty"`
	Workflow    string `json:"workflow,omitempty"`
}

type forgeEndpoint struct {
	Status  int             `json:"status,omitempty"`
	Message string          `json:"message,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

type forgeRules struct {
	Status  int               `json:"status,omitempty"`
	Message string            `json:"message,omitempty"`
	Pages   []json.RawMessage `json:"pages,omitempty"`
}

type forgeThread struct {
	Author string `json:"author"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Body   string `json:"body"`
}

type forgeStatus struct {
	ID        string `json:"id,omitempty"`
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"target_url,omitempty"`
}

type forgeState struct {
	Checks           []forgeCheck       `json:"checks,omitempty"`
	Statuses         []forgeStatus      `json:"statuses,omitempty"`
	Protection       *forgeEndpoint     `json:"protection,omitempty"`
	Rules            *forgeRules        `json:"rules,omitempty"`
	Runs             []json.RawMessage  `json:"runs,omitempty"`
	AttemptJobs      map[string][]int64 `json:"attempt_jobs,omitempty"`
	ReviewThreads    []forgeThread      `json:"review_threads,omitempty"`
	Mergeable        string             `json:"mergeable,omitempty"`
	MergeStateStatus string             `json:"merge_state_status,omitempty"`
}

const (
	githubActionsApp   = 15368 // the GitHub Actions app's real database ID
	scopeCheckWaitTime = 90 * time.Second
)

func forgeID(name string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum32()%900000) + 100
}

func actionsCheck(name, conclusion string) forgeCheck {
	return forgeCheck{DatabaseID: forgeID(name), Name: name, Conclusion: conclusion, CompletedAt: "2026-10-15T12:00:00Z", AppSlug: "github-actions", AppID: githubActionsApp}
}

func greenCheck(name string) forgeCheck { return actionsCheck(name, "SUCCESS") }
func redCheck(name string) forgeCheck   { return actionsCheck(name, "FAILURE") }
func runningCheck(name string) forgeCheck {
	c := actionsCheck(name, "")
	c.Status, c.CompletedAt = "IN_PROGRESS", ""
	return c
}

// protectionJSON is a classic required_status_checks answer; each entry is a
// context, or "context@appID" for an application-bound requirement.
func protectionJSON(requirements ...string) *forgeEndpoint {
	type check struct {
		Context string `json:"context"`
		AppID   int64  `json:"app_id"`
	}
	body := struct {
		Contexts []string `json:"contexts"`
		Checks   []check  `json:"checks"`
	}{Contexts: []string{}, Checks: []check{}}
	for _, r := range requirements {
		context, app, bound := strings.Cut(r, "@")
		if !bound {
			body.Contexts = append(body.Contexts, context)
			body.Checks = append(body.Checks, check{Context: context, AppID: -1})
			continue
		}
		var id int64
		if _, err := fmt.Sscan(app, &id); err != nil {
			panic(err)
		}
		body.Contexts = append(body.Contexts, context)
		body.Checks = append(body.Checks, check{Context: context, AppID: id})
	}
	data, _ := json.Marshal(body)
	return &forgeEndpoint{Body: data}
}

// rulesetPage is one page of branch rules that requires the given contexts.
func rulesetPage(contexts ...string) json.RawMessage {
	type required struct {
		Context string `json:"context"`
	}
	var list []required
	for _, c := range contexts {
		list = append(list, required{Context: c})
	}
	page := []map[string]any{{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": list}}}
	data, _ := json.Marshal(page)
	return data
}

// unrelatedRulesPage is a page whose rules name no check context.
func unrelatedRulesPage() json.RawMessage {
	return json.RawMessage(`[{"type":"pull_request","parameters":{"required_approving_review_count":1}}]`)
}

type scopeJourney struct {
	t         *testing.T
	h         *Harness
	statePath string
	forgePath string
	ghLogPath string
	revision  int
}

func newScopeJourney(t *testing.T, opts SetupOpts) *scopeJourney {
	t.Helper()
	h := NewHarness(t, opts)
	j := &scopeJourney{t: t, h: h}
	j.statePath = setupStatefulGitHub(t, h)
	j.forgePath = filepath.Join(filepath.Dir(h.AgentLog), "gh-forge.json")
	j.ghLogPath = filepath.Join(filepath.Dir(h.AgentLog), "gh-stateful.log")
	t.Setenv("FAKEAGENT_GH_FORGE", j.forgePath)
	return j
}

// setForge atomically replaces the fake forge; the next gh call, whether from
// the daemon's monitor or from a receipt process, observes it.
func (j *scopeJourney) setForge(f forgeState) {
	j.t.Helper()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		j.t.Fatal(err)
	}
	tmp := j.forgePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		j.t.Fatal(err)
	}
	if err := os.Rename(tmp, j.forgePath); err != nil {
		j.t.Fatal(err)
	}
}

// setTrusted commits the repository's trusted default-branch config (the only
// place ci.* and no_ci are read from) and pushes it to the forge's main. The
// base keys are the ones the harness always writes.
func (j *scopeJourney) setTrusted(extra string) {
	j.t.Helper()
	j.revision++
	cfg := fmt.Sprintf("# trusted revision %d\nignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\n", j.revision) + extra
	j.h.CommitChange("main", ".no-mistakes.yaml", cfg, "configure CI scope")
	if out, err := j.h.runGit(j.t.Context(), j.h.WorkDir, "push", "origin", "main"); err != nil {
		j.t.Fatalf("push trusted config: %v\n%s", err, out)
	}
}

func (j *scopeJourney) init() {
	j.t.Helper()
	if out, err := j.h.Run("init"); err != nil {
		j.t.Fatalf("init: %v\n%s", err, out)
	}
}

// push sends a new branch through the gate (an ordinary, non-strict run).
func (j *scopeJourney) push(branch string) {
	j.t.Helper()
	j.h.CommitChange(branch, "scope-"+strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n", "add "+branch)
	j.h.PushToGate(branch)
}

// runFor returns the newest run on branch.
func (j *scopeJourney) runFor(branch string) *ipc.RunInfo {
	j.t.Helper()
	for _, r := range j.h.Runs() {
		if r.Branch == branch {
			r := r
			return &r
		}
	}
	j.t.Fatalf("no run for %s", branch)
	return nil
}

func (j *scopeJourney) ciLog(runID string) string {
	data, _ := os.ReadFile(filepath.Join(j.h.NMHome, "logs", runID, "ci.log"))
	return string(data)
}

// waitCILog waits for the CI step log of runID to contain want.
func (j *scopeJourney) waitCILog(runID, want string, timeout time.Duration) {
	j.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(j.ciLog(runID), want) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	j.h.dumpDebugState()
	j.t.Fatalf("CI log of run %s never contained %q within %v; log:\n%s", runID, want, timeout, j.ciLog(runID))
}

// monitoring waits for the monitor to report want and returns the run with
// the CI step still executing: readiness is withheld and no gate is raised.
func (j *scopeJourney) expectWaiting(branch, want string) *ipc.RunInfo {
	j.t.Helper()
	run := waitForStepStatus(j.t, j.h, branch, types.StepCI, types.StepStatusRunning, scopeCheckWaitTime)
	j.waitCILog(run.ID, want, scopeCheckWaitTime)
	time.Sleep(time.Second)
	info := j.h.RunInfo(run.ID)
	ci, _ := findStep(info.Steps, types.StepCI)
	if info.CIReady || ci.Status != types.StepStatusRunning || ci.FindingsJSON != nil && *ci.FindingsJSON != "" {
		j.t.Fatalf("run %s should be monitoring without readiness: ready=%v ci=%s findings=%s\nlog:\n%s", run.ID, info.CIReady, ci.Status, deref(ci.FindingsJSON), j.ciLog(run.ID))
	}
	j.t.Logf("CI waiting on %s (ready=false):\n%s", branch, j.ciLog(run.ID))
	return info
}

// expectReady waits for the persisted checks-passed readiness.
func (j *scopeJourney) expectReady(branch string, timeout time.Duration) *ipc.RunInfo {
	j.t.Helper()
	run := waitForStepStatus(j.t, j.h, branch, types.StepCI, types.StepStatusRunning, scopeCheckWaitTime)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info := j.h.RunInfo(run.ID)
		if info.CIReady {
			j.t.Logf("CI ready on %s:\n%s", branch, j.ciLog(run.ID))
			return info
		}
		ci, _ := findStep(info.Steps, types.StepCI)
		if ci.Status != types.StepStatusRunning {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	j.h.dumpDebugState()
	info := j.h.RunInfo(run.ID)
	ci, _ := findStep(info.Steps, types.StepCI)
	j.t.Fatalf("run %s never became ready: ready=%v ci=%s findings=%s\nlog:\n%s", run.ID, info.CIReady, ci.Status, deref(ci.FindingsJSON), j.ciLog(run.ID))
	return nil
}

// expectGate waits for the CI step to park at an approval gate and returns the
// run and the checks named by its CI-check findings.
func (j *scopeJourney) expectGate(branch string) (*ipc.RunInfo, []string) {
	j.t.Helper()
	run := waitForStepStatus(j.t, j.h, branch, types.StepCI, types.StepStatusAwaitingApproval, scopeCheckWaitTime)
	ci, _ := findStep(run.Steps, types.StepCI)
	if run.CIReady {
		j.t.Fatalf("run %s parked at a CI gate while reporting readiness", run.ID)
	}
	j.t.Logf("CI gate on %s (ready=false), findings=%s:\n%s", branch, deref(ci.FindingsJSON), j.ciLog(run.ID))
	return run, findingChecks(j.t, ci.FindingsJSON)
}

func findingChecks(t *testing.T, raw *string) []string {
	t.Helper()
	if raw == nil || *raw == "" {
		return nil
	}
	parsed, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		t.Fatalf("parse CI findings: %v", err)
	}
	var checks []string
	for _, item := range parsed.Items {
		if item.Check != "" {
			checks = append(checks, item.Check)
		}
	}
	return checks
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// ghCalls counts the logged gh invocations having an argument that contains
// every one of substrs.
func (j *scopeJourney) ghCalls(substrs ...string) int {
	j.t.Helper()
	data, err := os.ReadFile(j.ghLogPath)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var inv ghStubInvocation
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			j.t.Fatalf("parse gh log line %q: %v", line, err)
		}
		joined := strings.Join(inv.Args, "\x00")
		all := true
		for _, s := range substrs {
			all = all && strings.Contains(joined, s)
		}
		if all {
			n++
		}
	}
	return n
}

func (j *scopeJourney) protectionReads() int {
	return j.ghCalls("/protection/required_status_checks")
}
func (j *scopeJourney) rulesReads() int  { return j.ghCalls("/rules/branches/") }
func (j *scopeJourney) rollupReads() int { return j.ghCalls("statusCheckRollup") }

// ciRepairPrompts returns the prompts of every CI repair agent turn so far.
func (j *scopeJourney) ciRepairPrompts() []string {
	var prompts []string
	for _, inv := range j.h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "- failing checks:") {
			prompts = append(prompts, inv.Prompt)
		}
	}
	return prompts
}

// recordedScope returns the check-scope policy JSON the CI step recorded.
func (j *scopeJourney) recordedScope(runID string) string {
	j.t.Helper()
	database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
	if err != nil {
		j.t.Fatal(err)
	}
	defer database.Close()
	raw, err := database.GetRunCheckScope(runID)
	if err != nil {
		j.t.Fatal(err)
	}
	return raw
}

// finish cancels an unfinished run so the next case starts from an idle branch.
func (j *scopeJourney) finish(branch string) {
	j.t.Helper()
	if run := j.h.ActiveRun(branch); run != nil {
		j.h.CancelRun(run.ID)
		j.h.WaitForRun(branch, time.Minute)
	}
}

// strictRun starts a strict-shipping run for a new branch with `axi run` and
// waits (bounded) for its CI-ready point. It returns the worktree, the run,
// and the axi output.
func (j *scopeJourney) strictRun(branch string) (string, *ipc.RunInfo, string) {
	j.t.Helper()
	j.h.CommitChange(branch, "strict-"+strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n", "add "+branch)
	wt := j.h.AddWorktree(branch)
	out, err := j.h.RunInDir(wt, "axi", "run", "--intent", "validate the CI check scope", "--shipping-mode", "full", "--wait", "90s")
	if err != nil {
		j.t.Fatalf("strict axi run: %v\n%s", err, out)
	}
	j.t.Logf("strict axi run on %s:\n%s", branch, out)
	return wt, j.runFor(branch), out
}

func (j *scopeJourney) receipt(wt, runID string) (string, error) {
	j.t.Helper()
	out, err := j.h.RunInDir(wt, "axi", "review-receipt", "--run", runID, "--phase", "ci-ready")
	j.t.Logf("ci-ready receipt for %s (error=%v):\n%s", runID, err, out)
	return out, err
}

// A completed required job on attempt 1 owns its result even while an external
// approval job keeps the workflow running. A success must ship; a failure must
// reach the repair gate instead of being converted back to pending.
func TestCIScopeFirstAttemptKeepsCompletedRequiredJob(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  external_checks:\n    - approve\n")
	j.init()
	for _, conclusion := range []string{"SUCCESS", "FAILURE"} {
		t.Run(conclusion, func(t *testing.T) {
			branch := "feature/scope-first-attempt-" + strings.ToLower(conclusion)
			build := actionsCheck("build", conclusion)
			build.DatabaseID, build.Workflow = 5, "CI"
			build.DetailsURL = "https://github.com/example/closes/actions/runs/7/job/5"
			approval := runningCheck("approve")
			approval.DatabaseID, approval.Workflow = 6, "CI"
			approval.DetailsURL = "https://github.com/example/closes/actions/runs/7/job/6"
			forge := forgeState{
				Protection: protectionJSON("build@15368"),
				Checks:     []forgeCheck{build, approval},
				Runs:       []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"id":7,"workflow_id":10,"run_attempt":1,"name":"CI","event":"push","head_branch":%q,"status":"in_progress","run_started_at":"2026-10-15T10:00:00Z","html_url":"https://github.com/example/closes/actions/runs/7"}`, branch))},
			}
			j.setForge(forge)
			if conclusion == "SUCCESS" {
				wt, run, out := j.strictRun(branch)
				if !run.CIReady || !strings.Contains(out, "outcome: checks-passed") {
					t.Fatalf("completed first-attempt build did not establish readiness: %s", out)
				}
				if _, err := j.receipt(wt, run.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				j.push(branch)
				run, checks := j.expectGate(branch)
				if !sameSet(checks, "build") {
					t.Fatalf("completed first-attempt failure must name only build, got %v", checks)
				}
				forge.Checks[0].Conclusion = "SUCCESS"
				j.setForge(forge)
				j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
				done := j.h.WaitForRun(branch, scopeCheckWaitTime)
				if done.Status != types.RunCompleted || done.CIOverrideReason != "" {
					t.Fatalf("approval after build passed: status=%s override=%q", done.Status, done.CIOverrideReason)
				}
				t.Logf("approval after the first-attempt build passed: status=%s override=%q", done.Status, done.CIOverrideReason)
			}
			j.finish(branch)
		})
	}
}

// A commit status is independent of the Actions run its URL happens to name.
// Both attempt reconciliation and retirement must retain a failing must-pass
// status for monitoring, approval verification, and live receipt checks.
func TestCIScopeActionsLinkedStatusStillBlocksReadiness(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  must_pass:\n    - audit-*\n")
	j.init()
	for _, retirement := range []bool{false, true} {
		t.Run(fmt.Sprintf("retirement=%v", retirement), func(t *testing.T) {
			variant := "reconciliation"
			if retirement {
				variant = "retirement"
			}
			forgeFor := func(branch, status string) forgeState {
				build := greenCheck("build")
				build.DatabaseID, build.Workflow = 5, "CI"
				build.DetailsURL = "https://github.com/example/closes/actions/runs/7/job/5"
				run := func(id, attempt int, started string) json.RawMessage {
					return json.RawMessage(fmt.Sprintf(`{"id":%d,"workflow_id":10,"run_attempt":%d,"name":"CI","event":"push","head_branch":%q,"status":"completed","conclusion":"success","run_started_at":%q,"html_url":"https://github.com/example/closes/actions/runs/%d"}`, id, attempt, branch, started, id))
				}
				forge := forgeState{
					Protection:  protectionJSON("build@15368"),
					Checks:      []forgeCheck{build},
					Statuses:    []forgeStatus{{ID: "audit", Context: "audit-status", State: status, TargetURL: "https://github.com/example/closes/actions/runs/7"}},
					Runs:        []json.RawMessage{run(7, 2, "2026-10-15T10:00:00Z")},
					AttemptJobs: map[string][]int64{"7/2": {5}},
				}
				if retirement {
					newBuild := build
					newBuild.DatabaseID = 6
					newBuild.DetailsURL = "https://github.com/example/closes/actions/runs/8/job/6"
					forge.Checks = append(forge.Checks, newBuild)
					forge.Runs = []json.RawMessage{run(7, 1, "2026-10-15T10:00:00Z"), run(8, 1, "2026-10-15T11:00:00Z")}
				}
				return forge
			}

			branch := "feature/scope-status-" + variant
			j.setForge(forgeFor(branch, "FAILURE"))
			j.push(branch)
			run, checks := j.expectGate(branch)
			if !sameSet(checks, "audit-status") {
				t.Fatalf("independent failing status must survive %s, findings=%v", variant, checks)
			}
			j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
			done := j.h.WaitForRun(branch, scopeCheckWaitTime)
			if done.Status != types.RunCompleted || !strings.Contains(done.CIOverrideReason, "audit-status") {
				t.Fatalf("approval must record the failing status: status=%s override=%q", done.Status, done.CIOverrideReason)
			}
			t.Logf("approval over the independent failing status: status=%s override=%q", done.Status, done.CIOverrideReason)

			strictBranch := branch + "-receipt"
			j.setForge(forgeFor(strictBranch, "SUCCESS"))
			wt, strict, out := j.strictRun(strictBranch)
			if !strict.CIReady || !strings.Contains(out, "outcome: checks-passed") {
				t.Fatalf("baseline green status did not establish readiness: %s", out)
			}
			j.setForge(forgeFor(strictBranch, "FAILURE"))
			receipt, err := j.receipt(wt, strict.ID)
			if err == nil || !strings.Contains(receipt, "strict readiness requires all live checks green") {
				t.Fatalf("receipt ignored the independent failing status: err=%v\n%s", err, receipt)
			}
			j.finish(strictBranch)
		})
	}
}

// A third-party app owns its check even when its details URL names an Actions
// run. A passing rerun must not erase that check from any readiness consumer.
func TestCIScopeActionsLinkedThirdPartyCheckStillBlocksReadiness(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  must_pass:\n    - audit-*\n")
	j.init()
	forgeFor := func(branch, conclusion string) forgeState {
		build := greenCheck("build")
		build.DatabaseID, build.Workflow = 5, "CI"
		build.DetailsURL = "https://github.com/example/closes/actions/runs/7/job/5"
		audit := actionsCheck("audit-security", conclusion)
		audit.DatabaseID, audit.AppSlug, audit.AppID = 9, "security-audit", 1234
		audit.DetailsURL = "https://github.com/example/closes/actions/runs/7"
		return forgeState{
			Protection:  protectionJSON("build@15368"),
			Checks:      []forgeCheck{build, audit},
			Runs:        []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"push","head_branch":%q,"status":"completed","conclusion":"success","run_started_at":"2026-10-15T10:00:00Z","html_url":"https://github.com/example/closes/actions/runs/7"}`, branch))},
			AttemptJobs: map[string][]int64{"7/2": {5}},
		}
	}

	strictBranch := "feature/scope-third-party-receipt"
	j.setForge(forgeFor(strictBranch, "SUCCESS"))
	wt, strict, out := j.strictRun(strictBranch)
	if !strict.CIReady || !strings.Contains(out, "outcome: checks-passed") {
		t.Fatalf("baseline green check did not establish readiness: %s", out)
	}
	j.setForge(forgeFor(strictBranch, "FAILURE"))
	receipt, err := j.receipt(wt, strict.ID)
	if err == nil || !strings.Contains(receipt, "strict readiness requires all live checks green") {
		t.Fatalf("receipt ignored the independent failing app check: err=%v\n%s", err, receipt)
	}
	j.finish(strictBranch)

	branch := "feature/scope-third-party-monitor"
	j.setForge(forgeFor(branch, "FAILURE"))
	j.push(branch)
	run, checks := j.expectGate(branch)
	if !sameSet(checks, "audit-security") {
		t.Fatalf("independent failing app check must reach the gate, findings=%v", checks)
	}
	j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
	done := j.h.WaitForRun(branch, scopeCheckWaitTime)
	if done.Status != types.RunCompleted || !strings.Contains(done.CIOverrideReason, "audit-security") {
		t.Fatalf("approval must record the failing app check: status=%s override=%q", done.Status, done.CIOverrideReason)
	}
	t.Logf("approval over the independent failing app check: status=%s override=%q", done.Status, done.CIOverrideReason)
	j.finish(branch)
}

// TestCIScopeStrictReadinessIgnoresNonRequiredAndExternalChecks (scenario 1): a
// strict run reaches checks-passed, and the ci-ready receipt is issued, while
// a non-required check and an explicitly external approval check stay red.
func TestCIScopeStrictReadinessIgnoresNonRequiredAndExternalChecks(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  external_checks:\n    - approve\n")
	// The ruleset requires the approval check as well; external still outranks it.
	j.setForge(forgeState{
		Protection: protectionJSON("build@15368"),
		Rules:      &forgeRules{Pages: []json.RawMessage{unrelatedRulesPage(), rulesetPage("approve")}},
		Checks:     []forgeCheck{greenCheck("build"), redCheck("approve"), redCheck("optional-lint")},
	})
	j.init()

	wt, run, out := j.strictRun("feature/scope-strict-ready")
	if !strings.Contains(out, "outcome: checks-passed") {
		t.Fatalf("strict run did not reach checks-passed:\n%s", out)
	}
	info := j.h.RunInfo(run.ID)
	if info.Shipping == nil || !info.CIReady || info.CIReadyNoCI {
		t.Fatalf("run = shipping %+v ready=%v no_ci=%v, want a strict run with real (not no_ci) readiness", info.Shipping, info.CIReady, info.CIReadyNoCI)
	}
	log := j.ciLog(run.ID)
	for _, want := range []string{
		`CI scope: "main" requires 2 status check(s): approve, build (app 15368)`,
		"external (awaiting outside approval): approve",
		"informational (not required, ignored): optional-lint",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("CI log lacks %q:\n%s", want, log)
		}
	}
	if prompts := j.ciRepairPrompts(); len(prompts) != 0 {
		t.Errorf("a CI repair agent ran for checks outside the required scope: %v", prompts)
	}
	if receipt, err := j.receipt(wt, run.ID); err != nil || !strings.Contains(receipt, `"phase":"ci-ready"`) {
		t.Fatalf("ci-ready receipt was refused while only out-of-scope checks are red: %v\n%s", err, receipt)
	}
	t.Logf("strict run %s: checks-passed with approve (external) and optional-lint (not required) red; receipt issued", run.ID)
}

// TestCIScopeRequiredChecksBlockReadiness (scenario 2): a failing, pending,
// missing, wrong-app, or stale required check prevents readiness and never
// reaches checks-passed; the control case with a satisfied requirement does.
func TestCIScopeRequiredChecksBlockReadiness(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setForge(forgeState{Protection: protectionJSON("build@15368")})
	j.init()

	t.Run("control satisfied requirement is ready", func(t *testing.T) {
		const branch = "feature/scope-required-ok"
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{greenCheck("build"), redCheck("optional-lint")}})
		j.push(branch)
		j.expectReady(branch, scopeCheckWaitTime)
		j.finish(branch)
	})

	t.Run("failing required check parks a gate and invokes no repair agent", func(t *testing.T) {
		const branch = "feature/scope-required-failing"
		before := len(j.ciRepairPrompts())
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{redCheck("build"), greenCheck("other")}})
		j.push(branch)
		run, checks := j.expectGate(branch)
		if !sameSet(checks, "build") {
			t.Fatalf("gate findings name %v, want exactly the failing required check build", checks)
		}
		if got := len(j.ciRepairPrompts()); got != before {
			t.Fatalf("a repair agent ran with auto_fix.ci = 0 (%d turns)", got-before)
		}
		if j.h.RunInfo(run.ID).CIReady {
			t.Fatal("readiness reported while the required check fails")
		}
		j.finish(branch)
	})

	for _, tc := range []struct {
		name   string
		branch string
		checks []forgeCheck
	}{
		{"pending required check", "feature/scope-required-pending", []forgeCheck{runningCheck("build")}},
		{"missing required check", "feature/scope-required-missing", []forgeCheck{greenCheck("other")}},
		{"required check from the wrong app", "feature/scope-required-wrong-app", []forgeCheck{func() forgeCheck {
			c := greenCheck("build")
			c.AppSlug, c.AppID = "some-other-app", 99
			return c
		}()}},
	} {
		t.Run(tc.name+" withholds readiness", func(t *testing.T) {
			j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: tc.checks})
			j.push(tc.branch)
			j.expectWaiting(tc.branch, "CI checks running, waiting for results...")
			j.finish(tc.branch)
		})
	}

	t.Run("stale required check from a superseded attempt withholds readiness until the current attempt reports", func(t *testing.T) {
		const branch = "feature/scope-required-stale"
		// Run 7 was re-run (attempt 2) and is still going. The rollup still
		// carries attempt 1's green build job, completed before the re-run
		// started; only the attempt's own job list can tell it is stale.
		runJSON := func(status, conclusion string) json.RawMessage {
			return json.RawMessage(fmt.Sprintf(`{"id":7,"workflow_id":10,"run_attempt":2,"name":"CI","event":"push","head_branch":%q,"run_started_at":"2026-10-15T10:00:00Z","status":%q,"conclusion":%q,"html_url":"https://github.com/example/closes/actions/runs/7"}`, branch, status, conclusion))
		}
		job := func(id int64, completedAt string) forgeCheck {
			c := greenCheck("build")
			c.DatabaseID, c.CompletedAt, c.Workflow = id, completedAt, "CI"
			c.DetailsURL = fmt.Sprintf("https://github.com/example/closes/actions/runs/7/job/%d", id)
			return c
		}
		stale := job(5, "2026-10-15T09:00:00Z")
		j.setForge(forgeState{
			Protection:  protectionJSON("build@15368"),
			Checks:      []forgeCheck{stale},
			Runs:        []json.RawMessage{runJSON("in_progress", "")},
			AttemptJobs: map[string][]int64{"7/2": {}},
		})
		j.push(branch)
		run := j.expectWaiting(branch, "CI checks running, waiting for results...")
		if ci, _ := findStep(run.Steps, types.StepCI); ci.Status != types.StepStatusRunning || run.CIReady {
			t.Fatalf("stale attempt-1 success certified the head: ready=%v ci=%s", run.CIReady, ci.Status)
		}
		// The re-run finishes; the next poll sees the current attempt's job.
		j.setForge(forgeState{
			Protection:  protectionJSON("build@15368"),
			Checks:      []forgeCheck{stale, job(6, "2026-10-15T11:00:00Z")},
			Runs:        []json.RawMessage{runJSON("completed", "success")},
			AttemptJobs: map[string][]int64{"7/2": {6}},
		})
		j.expectReady(branch, 75*time.Second)
		j.finish(branch)
	})
}

// TestCIScopeAutoDiscovery (scenario 3): auto mode unions branch protection
// with every page of the ruleset, reads the requirements once and reuses them
// across polls, and falls back to judging every check when they are unreadable.
func TestCIScopeAutoDiscovery(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.init()

	t.Run("classic protection absent leaves rulesets authoritative", func(t *testing.T) {
		const branch = "feature/scope-ruleset-only"
		j.setForge(forgeState{
			Rules:  &forgeRules{Pages: []json.RawMessage{rulesetPage("ruleset-check")}},
			Checks: []forgeCheck{redCheck("ruleset-check"), redCheck("unrelated")},
		})
		j.push(branch)
		_, checks := j.expectGate(branch)
		if !sameSet(checks, "ruleset-check") {
			t.Fatalf("explicit no-protection 404 must leave rulesets authoritative, findings=%v", checks)
		}
		j.finish(branch)
	})

	t.Run("required empty set still evaluates every non-external check", func(t *testing.T) {
		const branch = "feature/scope-empty-required"
		j.setForge(forgeState{Protection: protectionJSON(), Checks: []forgeCheck{greenCheck("build"), redCheck("docs")}})
		j.push(branch)
		_, checks := j.expectGate(branch)
		if !sameSet(checks, "docs") {
			t.Fatalf("empty requirements must fail safe to every check, findings=%v", checks)
		}
		j.finish(branch)
	})

	t.Run("protection and a later ruleset page are unioned", func(t *testing.T) {
		const branch = "feature/scope-union"
		j.setForge(forgeState{
			Protection: protectionJSON("build"),
			Rules:      &forgeRules{Pages: []json.RawMessage{unrelatedRulesPage(), rulesetPage("ruleset-check")}},
			Checks:     []forgeCheck{redCheck("build"), redCheck("ruleset-check"), redCheck("unrelated")},
		})
		j.push(branch)
		run, checks := j.expectGate(branch)
		if !sameSet(checks, "build", "ruleset-check") {
			t.Fatalf("gate findings name %v, want the union {build, ruleset-check} and not the unrelated check", checks)
		}
		if log := j.ciLog(run.ID); !strings.Contains(log, `requires 2 status check(s): build, ruleset-check`) {
			t.Fatalf("scope log does not show the unioned requirements:\n%s", log)
		}
		j.finish(branch)
	})

	t.Run("requirements are discovered once and cached across polls", func(t *testing.T) {
		const branch = "feature/scope-cache"
		protectionBefore, rulesBefore, rollupBefore := j.protectionReads(), j.rulesReads(), j.rollupReads()
		j.setForge(forgeState{Protection: protectionJSON("build"), Checks: []forgeCheck{runningCheck("build")}})
		j.push(branch)
		j.expectWaiting(branch, "CI checks running, waiting for results...")
		// The next 30s poll sees the finished check. A re-read would also see
		// the requirement added here and keep waiting for it, so becoming ready
		// proves the first read is reused.
		j.setForge(forgeState{Protection: protectionJSON("build", "never-reported"), Checks: []forgeCheck{greenCheck("build")}})
		j.expectReady(branch, 75*time.Second)
		if got := j.rollupReads() - rollupBefore; got < 2 {
			t.Fatalf("only %d rollup reads: the monitor did not poll twice", got)
		}
		if got := j.protectionReads() - protectionBefore; got != 1 {
			t.Fatalf("branch protection was read %d times across polls, want exactly 1 (cached)", got)
		}
		if got := j.rulesReads() - rulesBefore; got != 1 {
			t.Fatalf("branch rules were read %d times across polls, want exactly 1 (cached)", got)
		}
		j.finish(branch)
	})

	for _, tc := range []struct {
		name  string
		forge forgeState
	}{
		{"forbidden protection", forgeState{Protection: &forgeEndpoint{Status: 403, Message: "Resource not accessible by integration"}}},
		{"forbidden rules", forgeState{Protection: protectionJSON("build"), Rules: &forgeRules{Status: 403, Message: "Resource not accessible by integration"}}},
		{"bare protection not found", forgeState{Protection: &forgeEndpoint{Status: 404, Message: "Not Found"}}},
		{"bare rules not found", forgeState{Protection: protectionJSON("build"), Rules: &forgeRules{Status: 404, Message: "Not Found"}}},
		{"protection server error", forgeState{Protection: &forgeEndpoint{Status: 500, Message: "Server Error"}}},
	} {
		t.Run("unreadable requirements fall back to every check: "+tc.name, func(t *testing.T) {
			branch := "feature/scope-fallback-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.forge.Checks = []forgeCheck{greenCheck("build"), redCheck("docs")}
			j.setForge(tc.forge)
			j.push(branch)
			run, checks := j.expectGate(branch)
			if !sameSet(checks, "docs") {
				t.Fatalf("gate findings name %v, want the otherwise out-of-scope docs check judged too", checks)
			}
			if log := j.ciLog(run.ID); !strings.Contains(log, "evaluating all checks") {
				t.Fatalf("the fallback was not reported:\n%s", log)
			}
			j.finish(branch)
		})
	}
}

// TestCIScopeExternalOverridesAndAllExcludedRestriction (scenario 4): external
// patterns outrank must-pass and required contexts; when every observed check
// is excluded, readiness needs a known required set or a trusted no_ci.
func TestCIScopeExternalOverridesAndAllExcludedRestriction(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.init()

	t.Run("qualified glob exclusions cover an absent required approval context", func(t *testing.T) {
		const branch = "feature/scope-qualified-external"
		j.setTrusted("ci:\n  must_pass:\n    - 'CI / audit-*'\n  external_checks:\n    - 'Review / approval-*'\n")
		checks := []forgeCheck{greenCheck("build"), redCheck("audit-123")}
		checks[1].Workflow = "CI"
		j.setForge(forgeState{Protection: protectionJSON("build", "approval-123"), Checks: checks})
		j.push(branch)
		_, findings := j.expectGate(branch)
		if !sameSet(findings, "audit-123") {
			t.Fatalf("qualified must-pass must retain audit and external must exclude absent approval: findings=%v", findings)
		}
		checks[1].Conclusion = "SUCCESS"
		j.setForge(forgeState{Protection: protectionJSON("build", "approval-123"), Checks: checks})
		run := j.runFor(branch)
		j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
		done := j.h.WaitForRun(branch, scopeCheckWaitTime)
		if done.Status != types.RunCompleted || done.CIOverrideReason != "" {
			t.Fatalf("absent external approval blocked clean verification: status=%s override=%q", done.Status, done.CIOverrideReason)
		}
		t.Logf("qualified audit passed with required approval-123 still absent: status=%s override=%q", done.Status, done.CIOverrideReason)
	})

	t.Run("external outranks must_pass and the required set", func(t *testing.T) {
		const branch = "feature/scope-external-outranks"
		j.setTrusted("ci:\n  must_pass:\n    - approve\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Protection: protectionJSON("build", "approve"), Checks: []forgeCheck{greenCheck("build"), redCheck("approve")}})
		j.push(branch)
		run := j.expectReady(branch, scopeCheckWaitTime)
		if log := j.ciLog(run.ID); !strings.Contains(log, "external (awaiting outside approval): approve") {
			t.Fatalf("approve was not reported as external:\n%s", log)
		}
		j.finish(branch)
	})

	t.Run("required_checks all still honors external exclusions", func(t *testing.T) {
		const branch = "feature/scope-all-external"
		j.setTrusted("ci:\n  required_checks: all\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build"), redCheck("approve")}})
		j.push(branch)
		j.expectReady(branch, scopeCheckWaitTime)
		j.finish(branch)
	})

	t.Run("everything observed is external with no known required set is not ready", func(t *testing.T) {
		const branch = "feature/scope-all-excluded-unknown"
		j.setTrusted("ci:\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Checks: []forgeCheck{redCheck("approve")}}) // no protection: set unknown
		j.push(branch)
		j.expectWaiting(branch, "no CI checks reported yet, waiting for checks to register...")
		j.finish(branch)
	})

	t.Run("everything observed is external or informational with a known required set is ready", func(t *testing.T) {
		const branch = "feature/scope-all-excluded-known"
		j.setTrusted("ci:\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Protection: protectionJSON("approve"), Checks: []forgeCheck{redCheck("approve"), redCheck("optional-lint")}})
		j.push(branch)
		j.expectReady(branch, scopeCheckWaitTime)
		j.finish(branch)
	})

	t.Run("everything observed is external under trusted no_ci is ready", func(t *testing.T) {
		const branch = "feature/scope-all-excluded-no-ci"
		j.setTrusted("no_ci: true\nci:\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Checks: []forgeCheck{redCheck("approve")}})
		j.push(branch)
		j.expectReady(branch, scopeCheckWaitTime)
		j.finish(branch)
	})
}

// TestCIScopeApprovalAndReceiptEnforceRecordedScope (scenario 5): approval
// verification and the ci-ready receipt judge the scope the monitor recorded,
// not whatever the forge or configuration says by the time they run.
func TestCIScopeApprovalAndReceiptEnforceRecordedScope(t *testing.T) {
	t.Run("receipt", func(t *testing.T) {
		j := newScopeJourney(t, SetupOpts{Agent: "claude"})
		j.setTrusted("ci:\n  external_checks:\n    - approve\n")
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{greenCheck("build"), redCheck("approve")}})
		j.init()
		wt, run, out := j.strictRun("feature/scope-receipt")
		if !strings.Contains(out, "outcome: checks-passed") {
			t.Fatalf("strict run did not reach checks-passed:\n%s", out)
		}
		if scope := j.recordedScope(run.ID); !strings.Contains(scope, `"derived":true`) || !strings.Contains(scope, `"context":"build"`) || !strings.Contains(scope, `"external_checks":["approve"]`) {
			t.Fatalf("recorded scope does not carry the resolved requirement and external pattern: %s", scope)
		}
		accepts := func(label string) {
			t.Helper()
			if receipt, err := j.receipt(wt, run.ID); err != nil || !strings.Contains(receipt, `"phase":"ci-ready"`) {
				t.Fatalf("%s: receipt refused: %v\n%s", label, err, receipt)
			}
		}
		refuses := func(label, reason string) {
			t.Helper()
			receipt, err := j.receipt(wt, run.ID)
			if err == nil || strings.Contains(receipt, `"phase":"ci-ready"`) || !strings.Contains(receipt, reason) {
				t.Fatalf("%s: receipt must be refused with %q, got err=%v\n%s", label, reason, err, receipt)
			}
		}
		accepts("baseline")

		// The forge now claims a new requirement that is red, and the external
		// check stays red: neither is part of the recorded scope.
		protectionBefore := j.protectionReads()
		j.setForge(forgeState{Protection: protectionJSON("build@15368", "late-required"), Checks: []forgeCheck{greenCheck("build"), redCheck("approve"), redCheck("late-required")}})
		accepts("a requirement added after monitoring")
		if got := j.protectionReads(); got != protectionBefore {
			t.Fatalf("the receipt re-read branch protection (%d reads) instead of using the recorded requirements", got-protectionBefore)
		}

		// The recorded requirement itself failing, missing, or from the wrong
		// app refuses.
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{redCheck("build")}})
		refuses("recorded requirement failing", "strict readiness requires all live checks green")
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{greenCheck("approve")}})
		refuses("recorded requirement missing", "strict readiness requires all live checks green")
		wrong := greenCheck("build")
		wrong.AppSlug, wrong.AppID = "some-other-app", 99
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{wrong}})
		refuses("recorded requirement from the wrong app", "strict readiness requires all live checks green")
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{runningCheck("build")}})
		refuses("recorded requirement pending", "strict readiness requires all live checks green")
		j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build"), redCheck("approve")}})
		accepts("restored")
		j.h.CancelRun(run.ID)
	})

	t.Run("approval verification", func(t *testing.T) {
		j := newScopeJourney(t, SetupOpts{Agent: "claude"})
		j.setTrusted("ci:\n  external_checks:\n    - approve\n")
		j.init()

		// The gate is raised on a failing required check. By the time it is
		// approved the check is green, but the forge now also claims a red
		// requirement and a red external check: approval is judged on the
		// requirements recorded when the gate was raised, so it is a clean pass.
		const clean = "feature/scope-approve-clean"
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{redCheck("build"), redCheck("approve")}})
		j.push(clean)
		run, checks := j.expectGate(clean)
		if !sameSet(checks, "build") {
			t.Fatalf("gate findings name %v, want only build (approve is external)", checks)
		}
		if scope := j.recordedScope(run.ID); !strings.Contains(scope, `"derived":true`) || !strings.Contains(scope, `"context":"build"`) {
			t.Fatalf("scope was not recorded with its resolved requirement before the gate: %s", scope)
		}
		j.setForge(forgeState{Protection: protectionJSON("build@15368", "docs"), Checks: []forgeCheck{greenCheck("build"), redCheck("approve"), redCheck("docs")}})
		j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
		done := j.h.WaitForRun(clean, 90*time.Second)
		if done.Status != types.RunCompleted || done.CIOverrideReason != "" {
			t.Fatalf("approval under the recorded scope = status %s override %q, want a clean completion", done.Status, done.CIOverrideReason)
		}

		// Approving over a recorded requirement that is still red is an
		// explicit override, never a plain pass.
		const override = "feature/scope-approve-override"
		j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{redCheck("build"), redCheck("approve")}})
		j.push(override)
		run, _ = j.expectGate(override)
		j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
		done = j.h.WaitForRun(override, 90*time.Second)
		if done.Status != types.RunCompleted || !strings.Contains(done.CIOverrideReason, "build") {
			t.Fatalf("approval over a red required check = status %s override %q, want an override naming build", done.Status, done.CIOverrideReason)
		}
	})
}

// TestCIScopeCarriedReplayKeepsAppBoundRequiredRepair (scenario 6): selecting a
// repair for an application-bound required check that is literally named
// "build / test" keeps it in scope, and the same selection is dropped when the
// check's producer no longer satisfies the application binding.
//
// The pending-carried-replay state itself cannot be reached through the real
// daemon: ci.carry_review_on_replay is honored only when no PR comparison is
// pinned (sctx.PRTarget == nil), and the daemon pins one for every run that
// has a Rebase step, so a base move fails the run closed instead (see
// TestCIReplayOptionCannotBypassMovedComparisonOnRealBinary). This journey
// drives the live selection path with the option on; the carried-replay
// variant of the same scoping is covered by the step tests
// (TestCIStep_CarryReviewOnReplay_AFailedFixRoundStillRestartsAtTest).
func TestCIScopeCarriedReplayKeepsAppBoundRequiredRepair(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "ci-scope-repair.yaml")
	content := `actions:
  - match: "- failing checks: build / test"
    structured:
      summary: "the required build / test check does not fail because of this change"
      code_change_needed: false
  - text: "clean"
    structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "fixture has no defects"
      risk_scope: source-or-external
      tested: ["fixture check"]
      testing_summary: "fixture check passed"
      artifacts: []
      scenarios:
        - {name: "fixture check", result: pass, live: true, evidence: "scripted input", reason: ""}
      verdict: go
      title: "feat: ci scope repair probe"
      body: "CI scope repair probe"
`
	if err := os.WriteFile(scenario, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "ci:\n  carry_review_on_replay: true\n"})
	j.init()
	required := protectionJSON("build / test@15368")

	t.Run("selected repair of the app-bound required check reaches the fixer", func(t *testing.T) {
		const branch = "feature/scope-repair-kept"
		before := len(j.ciRepairPrompts())
		j.setForge(forgeState{Protection: required, Checks: []forgeCheck{redCheck("build / test")}})
		j.push(branch)
		run, checks := j.expectGate(branch)
		if !sameSet(checks, "build / test") {
			t.Fatalf("gate findings name %v, want the literal required context build / test", checks)
		}
		ci, _ := findStep(run.Steps, types.StepCI)
		items, _ := types.ParseFindingsJSON(deref(ci.FindingsJSON))
		j.h.RespondWithFindings(run.ID, types.StepCI, types.ActionFix, []string{items.Items[0].ID})
		deadline := time.Now().Add(scopeCheckWaitTime)
		for len(j.ciRepairPrompts()) == before && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
		}
		prompts := j.ciRepairPrompts()
		if len(prompts) != before+1 {
			j.h.dumpDebugState()
			t.Fatalf("the selected build / test repair never reached the fixer (%d new turns)\nlog:\n%s", len(prompts)-before, j.ciLog(run.ID))
		}
		j.finish(branch)
	})

	t.Run("selected repair is dropped once the producer no longer satisfies the binding", func(t *testing.T) {
		const branch = "feature/scope-repair-dropped"
		before := len(j.ciRepairPrompts())
		j.setForge(forgeState{Protection: required, Checks: []forgeCheck{redCheck("build / test")}})
		j.push(branch)
		run, _ := j.expectGate(branch)
		ci, _ := findStep(run.Steps, types.StepCI)
		items, _ := types.ParseFindingsJSON(deref(ci.FindingsJSON))
		// Before the human answers, the failing run of build / test is replaced
		// by one produced by another app: the application-bound requirement is
		// now unsatisfied (and unobserved), so the selection names nothing in scope.
		other := redCheck("build / test")
		other.AppSlug, other.AppID = "some-other-app", 99
		j.setForge(forgeState{Protection: required, Checks: []forgeCheck{other}})
		j.h.RespondWithFindings(run.ID, types.StepCI, types.ActionFix, []string{items.Items[0].ID})
		j.waitCILog(run.ID, "fix requested with no CI findings to repair, resuming monitoring...", scopeCheckWaitTime)
		if got := len(j.ciRepairPrompts()); got != before {
			t.Fatalf("a repair agent ran for a selection whose producer no longer matches (%d turns)", got-before)
		}
		j.finish(branch)
	})
}

// TestCIScopeTrustedConfigAndRestartSurvival (scenario 7): a pushed branch
// cannot weaken CI scope (ci.* and no_ci come only from the trusted
// default-branch copy, even with allow_repo_commands on), and the recorded
// scope survives a daemon and database restart.
func TestCIScopeTrustedConfigAndRestartSurvival(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  external_checks:\n    - approve\n")
	j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{redCheck("build"), redCheck("approve")}})
	j.init()

	// The pushed branch tries to declare no CI and to make the required check
	// external, which would let its red build pass.
	const branch = "feature/scope-weakening"
	pushedCfg := "ignore_patterns:\n  - '*.generated.go'\nallow_repo_commands: true\nno_ci: true\nci:\n  required_checks: all\n  must_pass: []\n  external_checks:\n    - build\n    - approve\n"
	j.h.CommitChange(branch, ".no-mistakes.yaml", pushedCfg, "weaken CI scope")
	j.h.PushToGate(branch)
	run, checks := j.expectGate(branch)
	if !sameSet(checks, "build") {
		t.Fatalf("gate findings name %v: the pushed branch weakened the scope (build must still be required)", checks)
	}
	scope := j.recordedScope(run.ID)
	for _, forbidden := range []string{`"no_ci"`, `"must_pass"`, `"external_checks":["build"`, `"required_checks":"all"`} {
		if strings.Contains(scope, forbidden) {
			t.Fatalf("recorded scope took %s from the pushed branch: %s", forbidden, scope)
		}
	}
	if !strings.Contains(scope, `"external_checks":["approve"]`) || !strings.Contains(scope, `"derived":true`) || !strings.Contains(scope, `"context":"build"`) {
		t.Fatalf("recorded scope lost the trusted policy or the resolved requirement: %s", scope)
	}

	// Stop the daemon with the gate parked, then start it again: the same run
	// comes back with the same recorded scope.
	if out, err := j.h.Run("daemon", "stop", "--force"); err != nil {
		t.Fatalf("stop daemon: %v\n%s", err, out)
	}
	if got := j.recordedScope(run.ID); got != scope {
		t.Fatalf("recorded scope changed while the daemon was down:\nbefore %s\nafter  %s", scope, got)
	}
	// While down, the forge changes: a new red requirement appears and the
	// recorded one turns green.
	j.setForge(forgeState{Protection: protectionJSON("build@15368", "docs"), Checks: []forgeCheck{greenCheck("build"), redCheck("approve"), redCheck("docs")}})
	if out, err := j.h.Run("daemon", "start"); err != nil {
		t.Fatalf("start daemon: %v\n%s", err, out)
	}
	resumed := waitForStepStatus(t, j.h, branch, types.StepCI, types.StepStatusAwaitingApproval, scopeCheckWaitTime)
	if resumed.ID != run.ID {
		t.Fatalf("restart replaced run %s with %s", run.ID, resumed.ID)
	}
	if got := j.recordedScope(run.ID); got != scope {
		t.Fatalf("recorded scope changed across the restart:\nbefore %s\nafter  %s", scope, got)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := j.h.RespondError(run.ID, types.StepCI, types.ActionApprove)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered CI gate never accepted the approval: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	done := j.h.WaitForRun(branch, 90*time.Second)
	if done.Status != types.RunCompleted || done.CIOverrideReason != "" {
		t.Fatalf("post-restart approval = status %s override %q: it must be judged under the recorded requirements (build only), as a clean pass", done.Status, done.CIOverrideReason)
	}
}
