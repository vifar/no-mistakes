package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOwnerReceiptCapabilityBeforeClaim(t *testing.T) {
	for _, path := range []string{"initial replay", "polling"} {
		for _, capability := range []string{"missing", "declined", "malformed", "error", "supported", "ownerless legacy", "ownerless current policy"} {
			t.Run(path+"/"+capability, func(t *testing.T) {
				var mu sync.Mutex
				var receipt ipc.LaunchReceipt
				claimed := false
				var calls []string
				owner := "Firstmate"
				if capability == "ownerless legacy" || capability == "ownerless current policy" {
					owner = ""
				}
				allowed := capability == "supported" || owner == ""
				olderDaemonFixture(t, nil, func(srv *ipc.Server) {
					srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
					if capability != "missing" && capability != "ownerless legacy" {
						srv.Handle(ipc.MethodProbeRunOwner, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
							mu.Lock()
							defer mu.Unlock()
							calls = append(calls, "probe")
							var p ipc.ProbeRunOwnerParams
							if err := json.Unmarshal(raw, &p); err != nil {
								return nil, err
							}
							if p.Owner != owner {
								return nil, fmt.Errorf("wrong probe owner %q", p.Owner)
							}
							switch capability {
							case "malformed":
								return json.RawMessage(`"unsupported"`), nil
							case "ownerless current policy":
								return nil, fmt.Errorf("launch requires an owner: pass --owner")
							case "error":
								return nil, fmt.Errorf("probe failed")
							default:
								return &ipc.ProbeRunOwnerResult{OK: capability == "supported"}, nil
							}
						})
					}
					srv.Handle(ipc.MethodClaimLaunchReceipt, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
						mu.Lock()
						defer mu.Unlock()
						calls = append(calls, "claim")
						var p ipc.ClaimLaunchReceiptParams
						if err := json.Unmarshal(raw, &p); err != nil {
							return nil, err
						}
						if p.LaunchNonce != receipt.LaunchNonce || p.SubmittedHeadSHA != receipt.SubmittedHeadSHA || p.ValidationGeneration != receipt.ValidationGeneration || p.IntentDigest != receipt.IntentDigest || p.Branch != receipt.Branch {
							return nil, fmt.Errorf("proof does not match seeded run")
						}
						disposition := "created"
						if claimed {
							disposition = "reused"
						}
						claimed = true
						result := receipt
						result.Disposition = disposition
						return &ipc.ClaimLaunchReceiptResult{Receipt: &result}, nil
					})
					srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
						return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "existing-run", Status: types.RunCompleted}}, nil
					})
				})
				head := cliGit(t, ".", "rev-parse", "HEAD")
				mu.Lock()
				receipt = ipc.LaunchReceipt{RunID: "existing-run", Branch: "feature/omit", HeadSHA: head, SubmittedHeadSHA: head, LaunchNonce: "existing-nonce", ValidationGeneration: "generation", IntentDigest: digestLaunchIntent("existing intent")}
				if capability == "supported" {
					receipt.Owner = owner
				}
				before := receipt
				mu.Unlock()
				writeGlobalConfig(t, "")
				t.Setenv("NO_MISTAKES_OWNER", "")
				var err error
				if path == "initial replay" {
					cmd := newAxiRunCmd()
					cmd.SetArgs([]string{"--intent", "existing intent", "--owner=" + owner, "--launch-nonce", "existing-nonce", "--validation-generation", "generation"})
					cmd.SetOut(io.Discard)
					cmd.SetErr(io.Discard)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					cmd.SetContext(ctx)
					err = cmd.Execute()
				} else {
					p, e := paths.New()
					if e != nil {
						t.Fatal(e)
					}
					client, e := ipc.Dial(p.Socket())
					if e != nil {
						t.Fatal(e)
					}
					defer client.Close()
					ctx := types.WithRunOwner(context.Background(), owner)
					_, err = waitForLaunchReceipt(ctx, client, "repo", "feature/omit", "existing-nonce", head, "generation", "existing intent", "", false, "", time.Second)
				}
				mu.Lock()
				defer mu.Unlock()
				if allowed {
					if err != nil || !claimed {
						t.Fatalf("compatible claim: claimed=%v, err=%v, calls=%v", claimed, err, calls)
					}
					want := "[probe claim]"
					if owner == "" {
						want = "[claim]"
					}
					if fmt.Sprint(calls) != want {
						t.Errorf("operation order = %v, want %s", calls, want)
					}
				} else {
					if err == nil {
						t.Error("owned replay accepted unsupported daemon")
					}
					if claimed || receipt != before {
						t.Errorf("unsupported owner capability changed receipt state: claimed=%v", claimed)
					}
					for _, call := range calls {
						if call == "claim" {
							t.Error("mutating claim reached unsupported daemon")
						}
					}
				}
			})
		}
	}
}
