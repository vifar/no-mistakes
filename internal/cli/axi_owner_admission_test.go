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

// The CLI and Git transport are real. The daemon boundary admits submissions
// only after an owner-policy probe and exposes a newly received run once Git
// creates the fresh branch. Refusing a first submission must leave every ref
// unchanged, so an immediate owned retry can still submit that branch.
func TestCLIEmptyOwnerAdmissionBeforeMirrorAndOwnedRetry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) { testCLIOwnerAdmission(t, legacy) })
	}
}

func testCLIOwnerAdmission(t *testing.T, legacy bool) {
	bin := buildNoMistakesBinary(t)
	var mu sync.Mutex
	var admitted bool
	var gateDir, head string
	ready := make(chan struct{})
	var created *ipc.RunInfo
	olderDaemonFixture(t, nil, func(s *ipc.Server) {
		if !legacy {
			s.Handle(ipc.MethodProbeRunOwner, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var p ipc.ProbeRunOwnerParams
				if err := json.Unmarshal(raw, &p); err != nil {
					return nil, err
				}
				if p.Owner == "" {
					return nil, fmt.Errorf("launch requires an owner: pass --owner or NO_MISTAKES_OWNER")
				}
				mu.Lock()
				admitted = true
				mu.Unlock()
				return &ipc.ProbeRunOwnerResult{OK: true}, nil
			})
		}
		s.Handle(ipc.MethodGetRunsForHead, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
			<-ready
			mu.Lock()
			defer mu.Unlock()
			if admitted || legacy {
				out, err := exec.CommandContext(ctx, "git", "--git-dir", gateDir, "rev-parse", "--verify", "refs/heads/feature/omit").Output()
				if err == nil && strings.TrimSpace(string(out)) == head {
					owner := "Firstmate"
					if legacy {
						owner = ""
					}
					created = &ipc.RunInfo{ID: "fresh-owned-run", Branch: "feature/omit", HeadSHA: head, Owner: owner, Status: types.RunCompleted}
				}
			}
			if created != nil {
				return &ipc.GetRunsResult{Runs: []ipc.RunInfo{*created}}, nil
			}
			return &ipc.GetRunsResult{}, nil
		})
		s.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			return &ipc.GetRunResult{Run: created}, nil
		})
		s.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
			return nil, fmt.Errorf("no previous run for branch")
		})
		s.HandleStream(ipc.MethodSubscribe, hangSubscribe)
	})
	writeGlobalConfig(t, "")
	t.Setenv("NO_MISTAKES_OWNER", "")
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := findRepo(d)
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
	before := cliGit(t, gateDir, "show-ref")
	invoke := func(owner string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "axi", "run", "--intent", "owner admission", "--owner="+owner).CombinedOutput()
		t.Logf("owner=%q: %v\n%s", owner, err, out)
		return string(out), err
	}
	out, err := invoke("")
	if legacy {
		if err != nil || !strings.Contains(out, "fresh-owned-run") {
			t.Fatalf("ownerless legacy launch: %v\n%s", err, out)
		}
		return
	}
	if err == nil || !strings.Contains(out, "launch requires an owner: pass --owner") {
		t.Errorf("empty-owner policy refusal: %v\n%s", err, out)
	}
	if got := cliGit(t, gateDir, "show-ref"); got != before {
		t.Errorf("refusal changed mirror refs: before=%q after=%q", before, got)
	}
	out, err = invoke("Firstmate")
	if err != nil || !strings.Contains(out, "fresh-owned-run") {
		t.Fatalf("immediate owned retry: %v\n%s", err, out)
	}
	if got := cliGit(t, gateDir, "rev-parse", "refs/heads/feature/omit"); got != head {
		t.Fatalf("owned submission head=%s want=%s", got, head)
	}
}

func TestEmptyOwnerProbeFailsClosedExceptLegacy(t *testing.T) {
	for _, response := range []string{"legacy", "declined", "malformed", "error", "supported"} {
		t.Run(response, func(t *testing.T) {
			olderDaemonFixture(t, nil, func(s *ipc.Server) {
				if response != "legacy" {
					s.Handle(ipc.MethodProbeRunOwner, func(context.Context, json.RawMessage) (interface{}, error) {
						switch response {
						case "declined":
							return &ipc.ProbeRunOwnerResult{}, nil
						case "malformed":
							return json.RawMessage(`{}`), nil
						case "error":
							return nil, fmt.Errorf("policy unavailable")
						default:
							return &ipc.ProbeRunOwnerResult{OK: true}, nil
						}
					})
				}
			})
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			c, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			err = probeLaunchOwner(c, "")
			allowed := response == "legacy" || response == "supported"
			if (err == nil) != allowed {
				t.Fatalf("empty-owner %s: %v", response, err)
			}
			if response == "legacy" && probeLaunchOwner(c, "Firstmate") == nil {
				t.Fatal("explicit owner accepted legacy daemon")
			}
		})
	}
}

func TestEmptyOwnerProbeUsesLiveDaemonPolicy(t *testing.T) {
	_, p, d, _ := setupReaderRepo(t)
	writeGlobalConfig(t, "intent:\n  require_owner: true\n")
	startTestDaemon(t, p, d)
	c, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := probeLaunchOwner(c, ""); err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("live policy refusal: %v", err)
	}
	if err := probeLaunchOwner(c, "Firstmate"); err != nil {
		t.Fatal(err)
	}
}
