package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestGateContextIngressPropagatesIdentityFailureWithoutAdmission(t *testing.T) {
	root, err := os.MkdirTemp("", "nm-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	outer := t.TempDir()
	runGit(t, outer, "init")
	cwd := filepath.Join(outer, "nested")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(cwd, "https://example.com/identity.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	beforeRepos, err := d.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	beforeRuns, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer()
	registerHandlers(srv, NewRunManager(d, p, nil), d, func() {})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeReady() }()
	defer func() { srv.Close(); <-done }()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.GateContextResult
	err = client.Call(ipc.MethodGateContext, &ipc.GateContextParams{CWD: cwd}, &result)
	if err == nil || !strings.Contains(err.Error(), "gate execution context") {
		t.Errorf("identity failure was admitted: %+v, %v", result, err)
	}
	afterRepos, err := d.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	afterRuns, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRepos, afterRepos) || !reflect.DeepEqual(beforeRuns, afterRuns) {
		t.Fatal("classification mutated enrollment or runs")
	}
	entries, err := os.ReadDir(p.ReposDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("classification created a gate")
	}
}
