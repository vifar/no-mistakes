package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func TestGateControlPreflightPropagatesIdentityFailureBeforeAdmission(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", p.Root())
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	outer := t.TempDir()
	run(t, outer, "git", "init")
	cwd := filepath.Join(outer, "nested")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	before, err := d.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"axi", "run"})
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	cmd.RunE = func(*cobra.Command, []string) error { admitted = true; return nil }
	root.SetArgs([]string{"axi", "run"})
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gate execution context") {
		t.Errorf("CLI discarded identity failure: %v", err)
	}
	if admitted {
		t.Error("pipeline command reached admission after identity failure")
	}
	after, err := d.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	active, err := d.GetActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || len(active) != 0 {
		t.Fatal("preflight mutated enrollment or runs")
	}
}
