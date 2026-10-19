package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the shipped command tree against real persisted state. The oracle
// is the owner/awaiting CLI contract: responsibility survives provenance edits,
// only unanswered durable parks appear, and full question text is preserved.
func TestOwnerObservationOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir, p, database, repo := setupReaderRepo(t)
	chdir(t, dir)
	writeGlobalConfig(t, "intent:\n  require_owner: false\n")
	startTestDaemon(t, p, database)
	writer, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	invoke := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		t.Logf("no-mistakes %s: %v\n%s", strings.Join(args, " "), err, out)
		if err != nil {
			t.Fatalf("command failed: %v", err)
		}
		return string(out)
	}
	description := strings.Repeat("界🙂", 700)
	encoded, err := json.Marshal(map[string]any{"summary": "", "findings": []map[string]string{{"id": "choice", "severity": "warning", "action": "ask-user", "description": description}}})
	if err != nil {
		t.Fatal(err)
	}
	findings := string(encoded)
	wantAwaiting := map[string]bool{}
	var mainID string
	for _, name := range []string{"main", "fix-review", "unknown-age", "future-age", "completed-step", "terminal", "answered", "incomplete"} {
		intent := &db.RunIntent{Owner: "responsible-thread", SessionID: "original-transcript", Summary: "original goal", Source: db.RunIntentSourceAgent}
		if name != "main" {
			intent.Owner = ""
		}
		r, err := database.InsertRunWithIntent(repo.ID, name, "head", "base", intent, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		step, err := database.InsertStepResult(r.ID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
			t.Fatal(err)
		}
		status := types.StepStatusAwaitingApproval
		if name == "fix-review" {
			status = types.StepStatusFixReview
		}
		if err := database.ParkStepForApproval(r.ID, step.ID, status, 0, 1, &findings); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "main":
			mainID = r.ID
			if err := database.UpdateRunIntent(r.ID, db.RunIntent{Owner: "must-not-replace-responsibility", Summary: "inferred goal", Source: "inferred", SessionID: "new-transcript"}); err != nil {
				t.Fatal(err)
			}
			_, err = writer.Exec("UPDATE runs SET awaiting_agent_since=? WHERE id=?", time.Now().Unix()-150, r.ID)
			wantAwaiting[r.ID] = true
			if !strings.Contains(invoke("status"), "responsible-thread") {
				t.Fatal("human status omitted responsibility")
			}
		case "fix-review":
			wantAwaiting[r.ID] = true
		case "unknown-age":
			_, err = writer.Exec("UPDATE runs SET awaiting_agent_since=NULL WHERE id=?", r.ID)
			wantAwaiting[r.ID] = true
		case "future-age":
			_, err = writer.Exec("UPDATE runs SET awaiting_agent_since=? WHERE id=?", time.Now().Unix()+3600, r.ID)
			wantAwaiting[r.ID] = true
		case "completed-step":
			err = database.UpdateStepStatus(step.ID, types.StepStatusCompleted)
		case "terminal":
			err = database.UpdateRunStatus(r.ID, types.RunCompleted)
		case "answered":
			_, err = writer.Exec("UPDATE step_results SET pending_response=? WHERE id=?", `{"action":"approve"}`, step.ID)
		case "incomplete":
			_, err = writer.Exec("UPDATE step_results SET gate_nonce=NULL WHERE id=?", step.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var statusDoc struct {
		Run struct {
			ID        string `json:"id"`
			Owner     string `json:"owner"`
			Ownerless bool   `json:"ownerless"`
			Awaiting  bool   `json:"awaiting_person"`
			Seconds   *int64 `json:"awaiting_seconds"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(invoke("axi", "status", "--json")), &statusDoc); err != nil {
		t.Fatal(err)
	}
	if statusDoc.Run.ID != mainID || statusDoc.Run.Owner != "responsible-thread" || statusDoc.Run.Ownerless || !statusDoc.Run.Awaiting || statusDoc.Run.Seconds == nil || *statusDoc.Run.Seconds < 150 || *statusDoc.Run.Seconds > 170 {
		t.Fatalf("current-branch owner/age contract: %+v", statusDoc.Run)
	}
	for _, args := range [][]string{{"axi"}, {"axi", "status"}} {
		out := invoke(args...)
		if !strings.Contains(out, "responsible-thread") {
			t.Fatal("text output omitted responsibility")
		}
	}
	var awaiting struct {
		Runs []struct {
			ID        string   `json:"id"`
			Branch    string   `json:"branch"`
			Ownerless bool     `json:"ownerless"`
			Seconds   *int64   `json:"awaiting_seconds"`
			Question  string   `json:"gate_question"`
			Questions []string `json:"gate_questions"`
		} `json:"awaiting_runs"`
	}
	if err := json.Unmarshal([]byte(invoke("axi", "status", "--awaiting", "--json")), &awaiting); err != nil {
		t.Fatal(err)
	}
	if len(awaiting.Runs) != len(wantAwaiting) {
		t.Fatalf("awaiting rows=%d, want %d", len(awaiting.Runs), len(wantAwaiting))
	}
	for _, row := range awaiting.Runs {
		if !wantAwaiting[row.ID] || row.Question != strings.Repeat("界🙂", 600)+"… (truncated, 1400 chars total)" || len(row.Questions) != 1 || row.Questions[0] != description || row.Ownerless != (row.Branch != "main") {
			t.Fatalf("unexpected awaiting row: %+v", row)
		}
		if row.Branch == "unknown-age" && row.Seconds != nil || row.Branch == "future-age" && (row.Seconds == nil || *row.Seconds != 0) {
			t.Fatalf("unknown/future age contract: %+v", row)
		}
	}
	if !strings.Contains(invoke("axi", "status", "--awaiting"), description) {
		t.Fatal("text awaiting lost full question")
	}
	if !strings.Contains(invoke("status"), "OWNERLESS") {
		t.Fatal("human status omitted ownerless marker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "axi", "status", "--awaiting", "--run", mainID).CombinedOutput()
	t.Logf("incompatible flags: %v\n%s", err, out)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatal("incompatible selection accepted")
	}
}

// Seed the durable records a normal and a diagnostic receive leave, then use
// the public eject command. No pipeline is launched by this fixture.
func TestEjectNotificationReferencesOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir, p, database, repo := setupReaderRepo(t)
	chdir(t, dir)
	writeGlobalConfig(t, "")
	cliGit(t, dir, "clone", "--bare", dir, p.RepoDir(repo.ID))
	cliGit(t, dir, "remote", "add", gate.RemoteName, p.RepoDir(repo.ID))
	r, err := database.InsertRun(repo.ID, "main", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BindPushNotification("admitted", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.BindPushDiagnostic("diagnostic", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	other, err := database.InsertRepo(filepath.Join(t.TempDir(), "unrelated"), "https://example.com/unrelated.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "eject").CombinedOutput()
	t.Logf("no-mistakes eject: %v\n%s", err, out)
	if err != nil || !strings.Contains(string(out), "Gate removed") {
		t.Fatalf("eject: %v", err)
	}
	removed, err := database.GetRepo(repo.ID)
	if err != nil || removed != nil {
		t.Fatalf("repository remains: %+v %v", removed, err)
	}
	retained, err := database.GetRepo(other.ID)
	if err != nil || retained == nil {
		t.Fatalf("unrelated repository removed: %+v %v", retained, err)
	}
	for _, id := range []string{"admitted", "diagnostic"} {
		a, d, err := database.PushNotificationRuns(id)
		if err != nil || a != nil || d != nil {
			t.Fatalf("notification remains: %s %+v %+v %v", id, a, d, err)
		}
	}
	if _, err := os.Stat(p.RepoDir(repo.ID)); !os.IsNotExist(err) {
		t.Fatalf("gate directory remains: %v", err)
	}
	if remotes := cliGit(t, dir, "remote"); strings.Contains(remotes, gate.RemoteName) {
		t.Fatalf("gate remote remains: %s", remotes)
	}
}

func TestEjectNotificationRollbackOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir, p, database, repo := setupReaderRepo(t)
	chdir(t, dir)
	writeGlobalConfig(t, "")
	cliGit(t, dir, "clone", "--bare", dir, p.RepoDir(repo.ID))
	cliGit(t, dir, "remote", "add", gate.RemoteName, p.RepoDir(repo.ID))
	r, err := database.InsertRun(repo.ID, "main", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BindPushNotification("rollback-admitted", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.BindPushDiagnostic("rollback-diagnostic", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("CREATE TRIGGER reject_repo_delete BEFORE DELETE ON repos BEGIN SELECT RAISE(ABORT,'test deletion blocked'); END"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "eject").CombinedOutput()
	t.Logf("no-mistakes eject (injected database refusal): %v\n%s", err, out)
	if err == nil || !strings.Contains(string(out), "test deletion blocked") {
		t.Fatal("injected repository deletion failure was not surfaced")
	}
	retained, err := database.GetRepo(repo.ID)
	if err != nil || retained == nil {
		t.Fatalf("failed deletion lost repository: %+v %v", retained, err)
	}
	a, _, err := database.PushNotificationRuns("rollback-admitted")
	if err != nil || a == nil || a.ID != r.ID {
		t.Fatalf("failed deletion lost admitted reference: %+v %v", a, err)
	}
	_, d, err := database.PushNotificationRuns("rollback-diagnostic")
	if err != nil || d == nil || d.ID != r.ID {
		t.Fatalf("failed deletion lost diagnostic reference: %+v %v", d, err)
	}
}

// Git diagnostics must win over an unusable application home. This drives the
// actual executable, including its exit status, rather than just Cobra helpers.
func TestRepositoryReaderPrecedenceOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir := t.TempDir()
	for _, state := range []string{"absent", "blocked", "old-schema"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "nmhome")
			var before []byte
			if state == "blocked" {
				if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if state == "old-schema" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				old, err := sql.Open("sqlite", filepath.Join(root, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := old.Exec("CREATE TABLE legacy(id INTEGER)"); err != nil {
					t.Fatal(err)
				}
				if err := old.Close(); err != nil {
					t.Fatal(err)
				}
				before, err = os.ReadFile(filepath.Join(root, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("NM_HOME", root)
			for _, args := range [][]string{{"status"}, {"runs"}, {"axi"}, {"axi", "status", "--json"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				cmd := exec.CommandContext(ctx, bin, args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				cancel()
				t.Logf("%s: no-mistakes %s: %v\n%s", state, strings.Join(args, " "), err, out)
				if (err == nil) != (args[0] == "status") || !strings.Contains(string(out), "not in a git repository") {
					t.Fatalf("Git error precedence: %v\n%s", err, out)
				}
			}
			if state == "absent" {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("query created state: %v", err)
				}
			}
			if state == "old-schema" {
				after, err := os.ReadFile(filepath.Join(root, "state.sqlite"))
				if err != nil || string(before) != string(after) {
					t.Fatalf("query changed legacy bytes: %v", err)
				}
			}
		})
	}
}

func TestOldSchemaRefusalOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir, p, database, _ := setupReaderRepo(t)
	chdir(t, dir)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("ALTER TABLE runs DROP COLUMN launch_owner; PRAGMA user_version=0; PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"runs"}, {"stats"}, {"axi"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		cancel()
		t.Logf("old schema: no-mistakes %s: %v\n%s", strings.Join(args, " "), err, out)
		if err == nil || !strings.Contains(string(out), "schema too old") || !strings.Contains(string(out), "daemon") {
			t.Fatal("old schema was not refused with migration guidance")
		}
	}
	after, err := os.ReadFile(p.DB())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("reader modified old schema bytes: %v", err)
	}
}

func TestDurableOwnerWarningOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	dir, _, database, repo := setupReaderRepo(t)
	chdir(t, dir)
	writeGlobalConfig(t, "log_level: error\nintent:\n  require_owner: false\n")
	warning := "invalid launch ownership: duplicate owner push option; admitted ownerless after receive (follow-up #82)"
	r, err := database.InsertRunWithIntent(repo.ID, "main", "head", "base", &db.RunIntent{OwnerAdmission: &types.OwnerAdmissionEvent{
		Event: "launch_owner_policy_bypassed", CallerPath: "post-receive (TUI/direct push)", OwnerlessAdmitted: true,
		OwnerValidationError: "duplicate owner push option", Warning: warning,
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"axi"}, {"axi", "status"}, {"axi", "status", "--json"}} {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		cancel()
		t.Logf("no-mistakes %s: %v\n%s", strings.Join(args, " "), err, out)
		if err != nil || !strings.Contains(string(out), warning) {
			t.Fatal("persisted warning lost after policy change")
		}
		if args[len(args)-1] == "--json" {
			var doc struct {
				Run struct {
					Event types.OwnerAdmissionEvent `json:"owner_admission_event"`
				} `json:"run"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Run.Event.RunID != r.ID || doc.Run.Event.Branch != "main" || doc.Run.Event.Event != "launch_owner_policy_bypassed" || doc.Run.Event.OwnerValidationError != "duplicate owner push option" || !doc.Run.Event.OwnerlessAdmitted {
				t.Fatalf("incomplete durable event: %+v", doc.Run.Event)
			}
		}
	}
}
