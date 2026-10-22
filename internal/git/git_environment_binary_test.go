package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Every git command in this package resolves its BINARY from the environment
// it is given, not from the process's PATH.
//
// exec.CommandContext resolves a bare name against the PROCESS's PATH, so a
// command whose environment names its own PATH - the context's overlay - would
// have that environment honoured for the child and ignored for the binary.
// The command would then run a different git than the caller chose.
func TestGitCommandsResolveTheBinaryFromTheirEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	marker := filepath.Join(dir, "used-the-context-git")
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The fixture has to be a program the PLATFORM will run, because the
	// resolver's usability rule is the platform's: a Windows candidate is
	// one with an extension, and a Unix one has to carry the execute bit.
	fake := filepath.Join(binDir, "git")
	if runtime.GOOS == "windows" {
		fake += ".exe"
		source := filepath.Join(dir, "main.go")
		sourceCode := "package main\nimport \"os\"\nfunc main() { os.WriteFile(\"" + marker + "\", []byte(\"used\"), 0o644) }\n"
		if err := os.WriteFile(source, []byte(sourceCode), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("go", "build", "-o", fake, source).CombinedOutput(); err != nil {
			t.Fatalf("build the git fixture: %v\n%s", err, out)
		}
	} else {
		script := "#!/bin/sh\nprintf 'used' > '" + marker + "'\nexit 0\n"
		if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := WithEnvironment(context.Background(), runenv.Overlay{Set: map[string]string{"PATH": binDir}})

	// A command that does not go through the ordinary runner still has to
	// resolve the binary the same way.
	if _, err := IsDetachedHEAD(ctx, dir); err != nil {
		t.Fatalf("IsDetachedHEAD: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the command did not run the git its environment names: %v", err)
	}
}
