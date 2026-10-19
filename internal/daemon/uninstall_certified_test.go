package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Integration evidence with deterministic gate and forge fixtures: uninstall
// must settle cancellation with the acknowledged SUCCESS intact before bootout
// can send raw shutdown. No certificate or preservation decision is seeded.
func TestUninstallPreservesCertifiedWatcher(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, d, "uninstall-certified")
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "gh"), "../pipeline/fakecli").CombinedOutput(); err != nil {
		t.Fatalf("fake forge: %v: %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	var mu sync.Mutex
	var states []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		states = append(states, payload.State)
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer server.Close()
	t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
	run, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
	if err := d.PutCommitStatusPublication(run.ID, publication); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	watcher := &shutdownWatchStep{execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		(&steps.PushStep{}).ReportPublishedHeadStatus(sc, "success", head)
		pub, err := d.GetCommitStatusPublication(context.Background(), run.ID, publication)
		if err != nil || pub == nil || pub.StrictCertificate == nil || pub.LastState != "success" {
			return nil, fmt.Errorf("watcher lacks acknowledged certification: %+v, %v", pub, err)
		}
		close(ready)
		<-sc.Ctx.Done()
		return nil, sc.Ctx.Err()
	}}
	core := []pipeline.Step{strictShutdownPass{types.StepReview}, strictShutdownPass{types.StepTest}, strictShutdownPass{types.StepDocument}, strictShutdownPass{types.StepLint}, watcher, &steps.PushStep{}}
	mgr := NewRunManager(d, p, nil)
	mgr.runtimeStartedAt = time.Now().UTC()
	executor := pipeline.NewExecutor(d, p, cfg, recoveredRunTestAgent{}, core, mgr.broadcast)
	executor.SetRuntimeInstance(mgr.runtimeInstance)
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(pipeline.ErrDaemonShutdown)
	registerTestRunCancel(mgr, run.ID, cancel)
	mgr.runs[run.ID].executor = executor
	mgr.wg.Add(1)
	finished := make(chan error, 1)
	go func() { defer mgr.wg.Done(); finished <- executor.Execute(ctx, run, repo, repo.WorkingPath) }()
	defer func() {
		cancel(pipeline.ErrDaemonShutdown)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("watcher did not exit during cleanup")
		}
	}()
	select {
	case <-ready:
	case err := <-finished:
		finished <- err
		t.Fatalf("watcher never certified: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never started")
	}
	defer stubServiceRuntime(t)()
	runtimeGOOS = "darwin"
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
	definition := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(definition), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, []byte("service"), 0600); err != nil {
		t.Fatal(err)
	}
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: mgr.runtimeStartedAt, Instance: mgr.runtimeInstance})
	oldStart := daemonProcessStartTime
	defer func() { daemonProcessStartTime = oldStart }()
	daemonProcessStartTime = func(int) (time.Time, error) { return mgr.runtimeStartedAt, nil }
	assertPreserved := func() {
		t.Helper()
		stored, err := d.GetRun(run.ID)
		if err != nil || stored.Status != types.RunCancelled || stored.Error == nil || *stored.Error != "cancelled: planned daemon shutdown" {
			t.Errorf("uninstall cancellation = %+v, %v", stored, err)
		}
		pub, err := d.GetCommitStatusPublication(context.Background(), run.ID, publication)
		if err != nil || pub == nil || pub.LastState != "success" || pub.StrictCertificate == nil || pub.PreserveSuccess == nil || !*pub.PreserveSuccess {
			t.Errorf("uninstall lost certified SUCCESS: %+v, %v", pub, err)
		}
	}
	serviceCommandRunner = func(string, ...string) ([]byte, error) {
		assertPreserved()
		// Simulate launchd's signal after the planned handoff has settled.
		mgr.Shutdown()
		return nil, nil
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodShutdown, func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
		var request ipc.ShutdownParams
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		ack, err := mgr.prepareControlledShutdown(ctx, request)
		if err == nil {
			err = os.Remove(p.PIDFile())
		}
		return ack, err
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeReady() }()
	defer func() { srv.Close(); <-done }()
	if got, err := Uninstall(p); err != nil || got != definition {
		t.Fatalf("uninstall = %q, %v", got, err)
	}
	assertPreserved()
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[pending success]" {
		t.Fatalf("uninstall changed acknowledged HTTP status: %v", states)
	}
}
