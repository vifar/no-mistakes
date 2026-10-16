//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the requested public commit-status contract: retain every POST
// in order, rather than overwriting a fake's latest state. Real CLI subprocesses
// drive the daemon, Git and pipeline; only the agent and forge are substitutes.
type journeyStatusPost struct {
	SHA, State string
	At         time.Time
}

type journeyStatusServer struct {
	t      *testing.T
	mu     sync.Mutex
	posts  []journeyStatusPost
	reject bool
}

func (s *journeyStatusServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{ State, Context string }
	sha := strings.TrimPrefix(r.URL.Path, "/repos/example/closes/statuses/")
	if r.Method != http.MethodPost || sha == r.URL.Path || len(sha) != 40 ||
		json.NewDecoder(r.Body).Decode(&body) != nil || body.Context != "no-mistakes" ||
		!slices.Contains([]string{"pending", "success", "failure"}, body.State) {
		s.t.Errorf("unexpected status request: %s %s, payload=%+v", r.Method, r.URL.Path, body)
		http.Error(w, "invalid commit status", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.posts = append(s.posts, journeyStatusPost{sha, body.State, time.Now()})
	reject := s.reject
	s.mu.Unlock()
	if reject {
		http.Error(w, "disposable forge unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{}`))
}

func (s *journeyStatusServer) history(sha string) []journeyStatusPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	var posts []journeyStatusPost
	for _, p := range s.posts {
		if p.SHA == sha {
			posts = append(posts, p)
		}
	}
	return posts
}

func (s *journeyStatusServer) assert(t *testing.T, sha string, want ...string) {
	t.Helper()
	var got []string
	for _, p := range s.history(sha) {
		got = append(got, p.State)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("status POSTs for %s = %v, want %v", sha, got, want)
	}
	t.Logf("status POSTs for %s: %v", sha, got)
}

func newStatusJourney(t *testing.T, provider string) (*Harness, *journeyStatusServer) {
	t.Helper()
	forge := &journeyStatusServer{t: t}
	server := httptest.NewServer(forge)
	t.Cleanup(server.Close)
	opts := SetupOpts{Agent: "claude", Scenario: axiScenario(t)}
	if provider == "plugin" {
		opts.GlobalConfigExtra = "provider_plugins:\n  fixture:\n    command: nm-fake-provider-plugin\n    hosts: [github.com]\n"
	}
	h := NewHarness(t, opts)
	if provider == "plugin" {
		state := filepath.Join(t.TempDir(), "provider.json")
		if err := fakeplugin.Save(state, fakeplugin.State{}); err != nil {
			t.Fatal(err)
		}
		t.Setenv(fakeplugin.EnvState, state)
	}
	if provider != "local" {
		setupStatefulGitHub(t, h)
		// An insteadOf rewrite changes `remote get-url` to file:// on recovery,
		// losing the forge identity. Keep the actual GitHub URL stable and route
		// only the SSH transport to this harness's disposable bare upstream.
		ssh := filepath.Join(h.BinDir, "local-git-ssh")
		script := "#!/bin/sh\ncase \"$*\" in\n" +
			"*git-upload-pack*) exec git upload-pack " + shellQuote(h.UpstreamDir) + ";;\n" +
			"*git-receive-pack*) exec git receive-pack " + shellQuote(h.UpstreamDir) + ";;\n" +
			"*) exit 1;;\nesac\n"
		if err := os.WriteFile(ssh, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_SSH_COMMAND", shellQuote(ssh))
		t.Setenv("GIT_SSH_VARIANT", "ssh")
		if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", "git@github.com:example/closes.git"); err != nil {
			t.Fatalf("set stable forge identity: %v\n%s", err, out)
		}
	}
	t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
	t.Setenv("FAKE_CLI_MODE", "gh")

	// Keep the existing stateful PR fixture, forwarding only status API calls
	// through the standard fakecli shim to the HTTP request boundary.
	statusCLI := filepath.Join(h.BinDir, "status-cli")
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", statusCLI, "./internal/pipeline/fakecli")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build status shim: %v\n%s", err, out)
	}
	providerDir := filepath.Join(h.BinDir, "provider")
	if err := os.Mkdir(providerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(h.BinDir, "gh")
	backup := filepath.Join(providerDir, "gh")
	if err := os.Rename(gh, backup); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in\n*'/statuses/'*) exec " + shellQuote(statusCLI) + " \"$@\";;\n" +
		"*) exec " + shellQuote(backup) + " \"$@\";;\nesac\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	statusCommand(t, h, h.WorkDir, "init")
	return h, forge
}

func statusCommand(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.RunInDir(dir, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func statusBranch(t *testing.T, h *Harness, published bool) (string, string, string) {
	t.Helper()
	branch := "feature/status-journey"
	head := h.CommitChange(branch, "feature.txt", "status lifecycle\n", "add status probe")
	if published {
		if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", head+":refs/heads/"+branch); err != nil {
			t.Fatalf("publish fixture: %v\n%s", err, out)
		}
	}
	return branch, head, h.AddWorktree(branch)
}

func statusRun(t *testing.T, h *Harness, wt, skip string) {
	t.Helper()
	statusCommand(t, h, wt, "axi", "run", "--intent", "validate commit status lifecycle", "--skip", skip)
}

// The status must precede the real review adapter invocation, not merely be
// present by the time the operator happens to observe an approval gate.
func assertStatusBeforeReview(t *testing.T, h *Harness, forge *journeyStatusServer, sha string, index int, since time.Time) {
	t.Helper()
	posts := forge.history(sha)
	if len(posts) <= index || posts[index].State != "pending" {
		t.Fatalf("missing attach pending: %+v", posts)
	}
	for _, invocation := range h.AgentInvocations() {
		at, err := time.Parse(time.RFC3339Nano, invocation.Time)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(since) || !strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
			continue
		}
		if !posts[index].At.Before(at) {
			t.Fatalf("attach status at %s did not precede review at %s", posts[index].At, at)
		}
		return
	}
	t.Fatal("real review invocation was not recorded")
}

func TestAttachCommitStatusJourney(t *testing.T) {
	t.Run("failed same SHA resubmit", func(t *testing.T) {
		h, forge := newStatusJourney(t, "github")
		branch, head, wt := statusBranch(t, h, true)
		clean, err := os.ReadFile(h.Scenario)
		if err != nil {
			t.Fatal(err)
		}
		// Malformed native review output is a real failed validation step.
		if err := os.WriteFile(h.Scenario, []byte("actions:\n  - text: unavailable\n    structured_raw: '{\"findings\":null}'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = h.RunInDir(wt, "axi", "run", "--intent", "exercise failed validation", "--skip", "pr,ci")
		failed := h.WaitForRun(branch, 90*time.Second)
		if failed.Status != types.RunFailed {
			t.Fatalf("run = %s, want failed: %s", failed.Status, deref(failed.Error))
		}
		forge.assert(t, head, "pending", "failure")
		if err := os.WriteFile(h.Scenario, clean, 0o600); err != nil {
			t.Fatal(err)
		}
		since := time.Now()
		statusRun(t, h, wt, "pr,ci")
		replacement := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
		if replacement.ID == failed.ID || replacement.HeadSHA != head {
			t.Fatalf("resubmit did not create a new run on the same head: %+v", replacement)
		}
		forge.assert(t, head, "pending", "failure", "pending")
		assertStatusBeforeReview(t, h, forge, head, 2, since)
		if test, _ := findStep(replacement.Steps, types.StepTest); test.Status != types.StepStatusPending {
			t.Fatalf("Test already ran before attach assertion: %+v", test)
		}
	})

	t.Run("abort resubmit and restart ownership", func(t *testing.T) {
		h, forge := newStatusJourney(t, "github")
		branch, head, wt := statusBranch(t, h, true)
		statusRun(t, h, wt, "pr,ci")
		old := h.ActiveRun(branch)
		if old == nil {
			t.Fatal("initial run did not park")
		}
		statusCommand(t, h, wt, "axi", "abort")
		forge.assert(t, head, "pending", "failure")
		statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
		// Crash window: the forge received failure, but its durable acknowledgement
		// was lost. Recovery would repost it unless the replacement retires it.
		database, err := db.Open(paths.WithRoot(h.NMHome).DB())
		if err != nil {
			t.Fatal(err)
		}
		p := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "example/closes", LastState: "pending"}
		if err := database.PutCommitStatusPublication(old.ID, p); err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
		// Restart first: recovery is obliged to retry this old failure.
		statusCommand(t, h, h.WorkDir, "daemon", "start")
		deadline := time.Now().Add(10 * time.Second)
		for len(forge.history(head)) < 3 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		forge.assert(t, head, "pending", "failure", "failure")
		statusRun(t, h, wt, "pr,ci")
		current := h.ActiveRun(branch)
		if current == nil || current.ID == old.ID {
			t.Fatal("abort/resubmit did not create a replacement")
		}
		forge.assert(t, head, "pending", "failure", "failure", "pending")
		statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
		database, err = db.Open(paths.WithRoot(h.NMHome).DB())
		if err != nil {
			t.Fatal(err)
		}
		// Replay the old acknowledgement loss after adoption too. A dedup-only
		// implementation would now put failure after the replacement's pending.
		if err := database.PutCommitStatusPublication(old.ID, p); err != nil {
			t.Fatal(err)
		}
		_ = database.Close()
		statusCommand(t, h, h.WorkDir, "daemon", "start")
		resumed := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
		if resumed.ID != current.ID {
			t.Fatal("restart replaced the attached run")
		}
		// Driving the resumed gate proves recovery has finished, rather than
		// reading the old persisted gate before its executor starts.
		statusCommand(t, h, wt, "axi", "respond", "--action", "approve")
		if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCompleted {
			t.Fatalf("resumed run did not complete: %+v", run)
		}
		forge.assert(t, head, "pending", "failure", "failure", "pending", "success")
	})

	t.Run("abort without resubmit", func(t *testing.T) {
		h, forge := newStatusJourney(t, "github")
		_, head, wt := statusBranch(t, h, true)
		statusRun(t, h, wt, "pr,ci")
		statusCommand(t, h, wt, "axi", "abort")
		statusCommand(t, h, h.WorkDir, "daemon", "restart")
		statusCommand(t, h, wt, "axi", "status")
		forge.assert(t, head, "pending", "failure")
	})

	for _, mode := range []string{"validated", "skipped Test", "skipped Push", "fresh head"} {
		t.Run(mode, func(t *testing.T) {
			h, forge := newStatusJourney(t, "github")
			branch, head, wt := statusBranch(t, h, mode != "fresh head")
			skip := "pr,ci"
			if mode == "skipped Test" {
				skip += ",test"
			}
			if mode == "skipped Push" {
				skip += ",push"
			}
			since := time.Now()
			statusRun(t, h, wt, skip)
			if mode == "fresh head" {
				forge.assert(t, head)
			} else {
				forge.assert(t, head, "pending")
				assertStatusBeforeReview(t, h, forge, head, 0, since)
			}
			statusCommand(t, h, wt, "axi", "respond", "--action", "approve")
			run := h.WaitForRun(branch, 60*time.Second)
			if run.Status != types.RunCompleted || run.HeadSHA != head {
				t.Fatalf("completion = %+v", run)
			}
			last := "success"
			if mode == "skipped Test" || mode == "skipped Push" {
				last = "failure"
			}
			forge.assert(t, head, "pending", last)
			if mode != "skipped Push" && h.UpstreamBranchSHA(branch) != head {
				t.Fatal("Push did not bind the exact upstream head")
			}
		})
	}

	for _, provider := range []string{"local", "plugin", "status unavailable"} {
		t.Run(provider, func(t *testing.T) {
			h, forge := newStatusJourney(t, provider)
			branch, head, wt := statusBranch(t, h, true)
			if provider == "status unavailable" {
				forge.mu.Lock()
				forge.reject = true
				forge.mu.Unlock()
			}
			statusRun(t, h, wt, "pr,ci")
			statusCommand(t, h, wt, "axi", "respond", "--action", "approve")
			if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCompleted {
				t.Fatalf("best-effort status prevented completion: %+v", run)
			}
			if provider != "status unavailable" {
				forge.assert(t, head)
			} else if len(forge.history(head)) == 0 {
				t.Fatal("status fault was never exercised")
			}
		})
	}
}

// The approval check remains pending independently of the status endpoint. The
// public contract requires a validated immutable Push to publish success before
// CI can be ready, avoiding an approval/status circular wait.
func TestImmutableAttachCommitStatusJourney(t *testing.T) {
	h, forge := newStatusJourney(t, "github")
	clean, err := os.ReadFile(h.Scenario)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(clean), "\n  - text: \"no issues found\"")
	if start < 0 {
		t.Fatal("clean scenario action is missing")
	}
	if err := os.WriteFile(h.Scenario, []byte("actions:"+string(clean[start:])), 0o600); err != nil {
		t.Fatal(err)
	}
	branch, head, wt := statusBranch(t, h, true)
	gh := filepath.Join(h.BinDir, "gh")
	delegate := filepath.Join(h.BinDir, "status-delegate")
	if err := os.Rename(gh, delegate); err != nil {
		t.Fatal(err)
	}
	checks := `{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"external-approval","status":"IN_PROGRESS","conclusion":null,"checkSuite":{"app":{"slug":"fixture-approval"}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}}`
	script := "#!/bin/sh\ncase \"$*\" in\n*'statusCheckRollup'*) printf '%s\\n' " + shellQuote(checks) + " ; exit 0 ;;\n*'mergeable,mergeStateStatus'*) printf 'MERGEABLE CLEAN\\n'; exit 0 ;;\nesac\nexec " + shellQuote(delegate) + " \"$@\"\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDir(wt, "axi", "run", "--shipping-mode", "immutable-source", "--intent", "validate immutable promotion status before external approval", "--wait", "2s")
	t.Logf("immutable launch: %v\n%s", err, out)
	if err != nil && !strings.Contains(out, "wait of 2s elapsed") {
		t.Fatalf("launch failed: %v\n%s", err, out)
	}
	run := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusRunning, 60*time.Second)
	if run.Status != types.RunRunning || run.CIReady {
		t.Fatalf("expected CI still pending before completion: %+v", run)
	}
	push, _ := findStep(run.Steps, types.StepPush)
	if push.Status != types.StepStatusCompleted {
		t.Fatalf("immutable Push did not complete: %+v", push)
	}
	forge.assert(t, head, "pending", "success")
	t.Log(statusCommand(t, h, wt, "axi", "status"))
	if h.UpstreamBranchSHA(branch) != head {
		t.Fatal("immutable promotion changed the upstream head")
	}
	statusCommand(t, h, wt, "axi", "abort")
	forge.assert(t, head, "pending", "success", "failure")
}

// A failed immutable validation must leave the attachment pending while parked,
// and cancellation must settle it as failure without ever publishing success.
func TestImmutableAttachFailedValidationJourney(t *testing.T) {
	h, forge := newStatusJourney(t, "github")
	clean, err := os.ReadFile(h.Scenario)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(clean), "\n  - text: \"no issues found\"")
	if start < 0 {
		t.Fatal("clean scenario action is missing")
	}
	scenario := `actions:
  - match: "You are validating a code change"
    text: "validation failed"
    structured:
      findings: []
      summary: "validation failed"
      tested: ["fixture: failing acceptance check"]
      testing_summary: "fixture acceptance check failed"
      artifacts: []
      scenarios:
        - {name: "fixture acceptance check", result: fail, live: true, evidence: "fixture rejection", reason: ""}
      verdict: no-go
` + string(clean[start:])
	if err := os.WriteFile(h.Scenario, []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	branch, head, wt := statusBranch(t, h, true)
	t.Log(statusCommand(t, h, wt, "axi", "run", "--shipping-mode", "immutable-source", "--intent", "refuse success for failed immutable validation"))
	run := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)
	push, _ := findStep(run.Steps, types.StepPush)
	if push.Status != types.StepStatusPending {
		t.Fatalf("unvalidated immutable source reached Push: %+v", push)
	}
	forge.assert(t, head, "pending")
	statusCommand(t, h, wt, "axi", "abort")
	forge.assert(t, head, "pending", "failure")
}
