package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The executable and Git are real; the legacy daemon is an existing IPC fixture,
// not an old released executable. In the polling case its capability disappears
// after receive, so skipping the second probe would consume the seeded receipt.
func TestOwnerReceiptOnRealBinary(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	for _, polling := range []bool{false, true} {
		for _, owner := range []string{"Firstmate", ""} {
			t.Run(fmt.Sprintf("polling=%t/owner=%q", polling, owner), func(t *testing.T) {
				var mu sync.Mutex
				claimed := false
				var gateDir, head string
				ready := make(chan struct{})
				present := func() bool {
					<-ready
					out, err := exec.Command("git", "--git-dir", gateDir, "rev-parse", "--verify", "refs/heads/feature/omit").Output()
					return err == nil && strings.TrimSpace(string(out)) == head
				}
				olderDaemonFixture(t, nil, func(s *ipc.Server) {
					s.HandleStream(ipc.MethodSubscribe, hangSubscribe)
					s.Handle(ipc.MethodProbeRunOwner, func(context.Context, json.RawMessage) (interface{}, error) {
						if polling && !present() {
							return &ipc.ProbeRunOwnerResult{OK: true}, nil
						}
						return nil, &ipc.RPCError{Code: ipc.ErrMethodNotFound, Message: "method not found: probe_run_owner"}
					})
					s.Handle(ipc.MethodClaimLaunchReceipt, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
						<-ready
						if polling && !present() {
							return &ipc.ClaimLaunchReceiptResult{}, nil
						}
						var p ipc.ClaimLaunchReceiptParams
						if err := json.Unmarshal(raw, &p); err != nil {
							return nil, err
						}
						mu.Lock()
						claimed = true
						mu.Unlock()
						return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{RunID: "legacy-run", Disposition: "created", LaunchNonce: p.LaunchNonce, ValidationGeneration: p.ValidationGeneration, Branch: p.Branch, HeadSHA: head, SubmittedHeadSHA: head, IntentDigest: p.IntentDigest}}, nil
					})
					s.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
						return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "legacy-run", Status: types.RunCompleted}}, nil
					})
				})
				writeGlobalConfig(t, "")
				t.Setenv("NO_MISTAKES_OWNER", "")
				p, err := paths.New()
				if err != nil {
					t.Fatal(err)
				}
				database, err := db.OpenReadOnly(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				repo, err := findRepo(database)
				database.Close()
				if err != nil {
					t.Fatal(err)
				}
				local, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				gateDir = p.RepoDir(repo.ID)
				cliGit(t, local, "clone", "--bare", local, gateDir)
				cliGit(t, gateDir, "update-ref", "-d", "refs/heads/feature/omit")
				cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
				cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
				head = cliGit(t, local, "rev-parse", "HEAD")
				close(ready)
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				out, err := exec.CommandContext(ctx, bin, "axi", "run", "--intent", "legacy intent", "--owner="+owner, "--launch-nonce", "legacy-nonce", "--validation-generation", "generation").CombinedOutput()
				t.Logf("CLI: %v\n%s", err, out)
				mu.Lock()
				defer mu.Unlock()
				if owner == "" {
					if err != nil || !claimed {
						t.Fatalf("ownerless legacy compatibility: claimed=%t err=%v", claimed, err)
					}
				} else if err == nil || claimed || !strings.Contains(string(out), "daemon cannot accept launch ownership (update it before launching with --owner): method not found: probe_run_owner") {
					t.Fatalf("legacy ownership refusal consumed receipt or lost diagnostic: claimed=%t err=%v", claimed, err)
				}
			})
		}
	}
}
