package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is issue #60: cancellation of admitted auto-fix work must retain
// caller and reason in persisted state, independently of log verbosity.
type endingLog struct {
	sync.Mutex
	bytes.Buffer
}

func (b *endingLog) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *endingLog) snapshot() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }
func captureEndingLog(t *testing.T) *endingLog {
	t.Helper()
	b := &endingLog{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}
func endingPayload(t *testing.T, run *db.Run) map[string]any {
	t.Helper()
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	event, _ := doc["EndingEvent"].(map[string]any)
	if event == nil {
		t.Fatalf("run %s has no ending attribution in durable record (status=%s error=%v)", run.ID, run.Status, run.Error)
	}
	return event
}
func endingRun(t *testing.T, f *schedFixture, id string) *db.Run {
	t.Helper()
	f.waitStatus(id, types.RunCancelled)
	r, err := f.d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestEndingCancelRecordAndDaemonLogNameCaller(t *testing.T) {
	logs := captureEndingLog(t)
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	id, _ := f.push("caller")
	f.waitStarted(id)
	if err := f.client.Call(ipc.MethodCancelRun, map[string]any{"run_id": id, "caller": "thread-60", "client": "test-ipc"}, nil); err != nil {
		t.Fatal(err)
	}
	event := endingPayload(t, endingRun(t, f, id))
	if event["caller"] != "thread-60" || event["client"] != "test-ipc" || event["attribution_source"] != "caller" {
		t.Fatalf("attribution = %#v", event)
	}
	if !strings.Contains(logs.snapshot(), `"caller":"thread-60"`) {
		t.Fatalf("daemon log has no caller: %s", logs.snapshot())
	}
	var result ipc.GetRunResult
	if err := f.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: id}, &result); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Run)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if doc["ending_event"] == nil {
		t.Fatalf("get_run omitted ending_event: %s", raw)
	}
}
func TestEndingCancelExplicitReasonPreserved(t *testing.T) {
	f := newSchedFixture(t, "")
	id, _ := f.push("reason")
	f.waitStarted(id)
	if err := f.client.Call(ipc.MethodCancelRun, map[string]any{"run_id": id, "caller": "coordinator", "reason": "priority line needs slot"}, nil); err != nil {
		t.Fatal(err)
	}
	r := endingRun(t, f, id)
	e := endingPayload(t, r)
	if e["caller"] != "coordinator" || e["reason"] != "priority line needs slot" {
		t.Fatalf("caller/reason lost: %#v", e)
	}
	if r.Error == nil || *r.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("cancel semantics changed: %+v", r)
	}
}
func TestEndingLegacyCancelWithoutIdentityWorks(t *testing.T) {
	f := newSchedFixture(t, "")
	id, _ := f.push("legacy")
	f.waitStarted(id)
	if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: id}, nil); err != nil {
		t.Fatal(err)
	}
	e := endingPayload(t, endingRun(t, f, id))
	if e["caller"] != nil || e["attribution_source"] != "unavailable" {
		t.Fatalf("invented caller: %#v", e)
	}
}
func TestEndingLaunchOwnerDefaultAttribution(t *testing.T) {
	f := newSchedFixture(t, "")
	var result ipc.PushReceivedResult
	if err := f.client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: f.p.RepoDir(f.repoID), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: f.head, Owner: "launch-thread-60"}, &result); err != nil {
		t.Fatal(err)
	}
	f.waitStarted(result.RunID)
	if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: result.RunID}, nil); err != nil {
		t.Fatal(err)
	}
	e := endingPayload(t, endingRun(t, f, result.RunID))
	if e["caller"] != "launch-thread-60" || e["attribution_source"] != "launch_owner" {
		t.Fatalf("owner default lost or mislabelled: %#v", e)
	}
}
func TestEndingSupersessionNamesPushAndReplacement(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	holder, _ := f.push("holder")
	f.waitStarted(holder)
	old, _ := f.push("superseded")
	f.waitStatus(old, types.RunQueued)
	newer, _ := f.push("superseded")
	r := endingRun(t, f, old)
	e := endingPayload(t, r)
	if e["event"] != "run_superseded" || e["superseding_sha"] != f.head || e["superseding_run_id"] != newer || e["superseding_ref"] != "refs/heads/superseded" {
		t.Fatalf("untraceable supersession: %#v", e)
	}
	if e["superseding_generation"] != nil {
		t.Fatalf("invented unpublished push generation: %#v", e)
	}
	if r.Error == nil || *r.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("supersession semantics changed: %+v", r)
	}
}

type endingAutoFixStep struct {
	started chan struct{}
	once    sync.Once
}

func (*endingAutoFixStep) Name() types.StepName { return types.StepTest }
func (s *endingAutoFixStep) Execute(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if c.Fixing {
		s.once.Do(func() { close(s.started) })
		<-c.Ctx.Done()
		return nil, context.Cause(c.Ctx)
	}
	if err := bindMockRunComparison(c); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{AutoFixable: true, Findings: `{"findings":[{"id":"test-fix","severity":"error","description":"test fails","action":"auto-fix"}],"summary":"fix test"}`}, nil
}
func TestEndingMeasuredMidAutoFixCancellation(t *testing.T) {
	logs := captureEndingLog(t)
	step := &endingAutoFixStep{started: make(chan struct{})}
	p, d := startTestDaemonWithStepsConfig(t, func() []pipeline.Step { return []pipeline.Step{step} }, "auto_fix:\n  test: 3\n")
	repo, head := setupTestGitRepo(t, p, d, "ending-autofix")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte("auto_fix:\n  test: 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "enable fixture auto-fix")
	head = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: head}, &result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-step.started:
	case <-time.After(30 * time.Second):
		r, _ := d.GetRun(result.RunID)
		rows, _ := d.GetStepsByRun(result.RunID)
		t.Fatalf("never reached auto-fix: run=%+v steps=%+v logs=%s", r, rows, logs.snapshot())
	}
	if !strings.Contains(logs.snapshot(), `"msg":"auto-fixing step","step":"test","attempt":1,"max":3`) {
		t.Fatalf("not measured auto-fix phase: %s", logs.snapshot())
	}
	if err := client.Call(ipc.MethodCancelRun, map[string]any{"run_id": result.RunID, "caller": "Firstmate-thread", "client": "axi", "reason": "owner priority"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.snapshot(), `"caller":"Firstmate-thread"`) {
		t.Errorf("mid-auto-fix cancel has no caller in daemon log: %s", logs.snapshot())
	}
	f := &schedFixture{t: t, d: d}
	r := endingRun(t, f, result.RunID)
	e := endingPayload(t, r)
	if e["caller"] != "Firstmate-thread" {
		t.Fatalf("admitted auto-fix abort anonymous: %#v", e)
	}
	if !strings.Contains(logs.snapshot(), `"caller":"Firstmate-thread"`) {
		t.Fatalf("daemon cancel anonymous: %s", logs.snapshot())
	}
}

func TestEndingSupersessionOfAdmittedWork(t *testing.T) {
	f := newSchedFixture(t, "")
	old, _ := f.push("admitted-supersession")
	f.waitStarted(old)
	newer, _ := f.push("admitted-supersession")
	e := endingPayload(t, endingRun(t, f, old))
	if e["superseding_run_id"] != newer || e["superseding_sha"] != f.head {
		t.Fatalf("admitted supersession anonymous: %#v", e)
	}
}

func TestEndingAttributionSurvivesDisabledInfoLogs(t *testing.T) {
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&endingLog{}, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	f := newSchedFixture(t, "")
	id, _ := f.push("error-log-level")
	f.waitStarted(id)
	if err := f.client.Call(ipc.MethodCancelRun, map[string]any{"run_id": id, "caller": "record-only-caller"}, nil); err != nil {
		t.Fatal(err)
	}
	if e := endingPayload(t, endingRun(t, f, id)); e["caller"] != "record-only-caller" {
		t.Fatalf("logging level lost durable actor: %#v", e)
	}
}

func TestEndingSupersessionRecordsKnownValidationGeneration(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	holder, _ := f.push("generation-holder")
	f.waitStarted(holder)
	old, _ := f.push("generation-superseded")
	f.waitStatus(old, types.RunQueued)
	var replacement ipc.PushReceivedResult
	if err := f.client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: f.p.RepoDir(f.repoID), Ref: "refs/heads/generation-superseded", Old: f.head, New: f.head, LaunchNonce: "nm60-nonce", ValidationGeneration: "nm60-validation", Intent: "record the received push causing supersession", Owner: "replacement-owner"}, &replacement); err != nil {
		t.Fatal(err)
	}
	e := endingPayload(t, endingRun(t, f, old))
	if e["superseding_validation_generation"] != "nm60-validation" || e["superseding_run_id"] != replacement.RunID || e["superseding_old_sha"] != f.head || e["push_notification_id"] == nil {
		t.Fatalf("known push provenance dropped: %#v", e)
	}
}

func TestEndingShutdownLeavesCIMonitorInterruptedWithoutCancelEvent(t *testing.T) {
	f := newFaultFixture(t, "2")
	// Reuse the shutdown fixture's real lifecycle, but isolate its CI case from
	// the separate mid-agent expectation currently tracked by the fork.
	// pushBranchRun expects the shutdown fixture's registered repository ID.
	p, d := f.p, f.d
	repo, head := setupTestGitRepo(t, p, d, "shutdown-ci-monitor")
	errCh := startMixedShutdownDaemon(t, p, d)
	id := pushBranchRun(t, p, repo.WorkingPath, shutdownCIBranch, head)
	waitForStepStatus(t, d, id, types.StepCI, types.StepStatusRunning)
	stopParkedRunDaemon(t, p, errCh)
	stopped, err := d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status == types.RunFailed {
		t.Fatalf("shutdown failed CI monitor: %+v", stopped)
	}
	errCh = startMixedShutdownDaemon(t, p, d)
	defer stopParkedRunDaemon(t, p, errCh)
	recovered, err := d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(recovered)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if recovered.Status != types.RunCIMonitorInterrupted || doc["EndingEvent"] != nil {
		t.Fatalf("shutdown changed run status or invented cancel caller: %s", raw)
	}
}
