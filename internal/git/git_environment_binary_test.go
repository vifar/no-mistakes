package git

import (
	"context"
	"os"
	"path/filepath"
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
	marker := filepath.Join(dir, "used-the-context-git")
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'used' > '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
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
