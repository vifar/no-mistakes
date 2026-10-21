package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
)

type quotaErrorTransformAgent struct {
	Agent
	transform func(error) error
}

func (a quotaErrorTransformAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	result, err := a.Agent.Run(ctx, opts)
	if err != nil {
		err = a.transform(err)
	}
	return result, err
}

func quotaOpencodeFixture(t *testing.T, version string, evidence opencodeToolEvidence) (Agent, func() int) {
	t.Helper()
	if version == "v2" {
		content := `[]`
		if evidence == opencodeToolsRan {
			content = `[{"type":"tool","id":"write-file","name":"shell","state":{"status":"completed"}}]`
		} else if evidence == opencodeToolsUnknown {
			content = `null`
		}
		fixture := &opencodeV2Fixture{outcome: "failed", pages: []string{
			`{"data":[{"id":"failed","type":"assistant","content":` + content + `,"error":{"type":"provider","message":"quota exceeded","status":429}}],"cursor":{}}`,
		}}
		return v2Agent(fixture.server(t)), func() int { return fixture.count("POST /api/session/ses_fixture/prompt") }
	}
	var server *httptest.Server
	var calls func() int
	if evidence == opencodeToolsUnknown {
		fixture, sessions := opencodeSlowMessageServer(t, stepFinishEvent, "")
		server, calls = fixture, func() int { return int(sessions.Load()) }
	} else {
		parts := `[]`
		if evidence == opencodeToolsRan {
			parts = `[{"type":"tool","tool":"shell","state":{"status":"completed"}}]`
		}
		fixture, sent := opencodeErrorServer(t,
			`{"info":{"id":"failed","role":"assistant","error":{"name":"APIError","data":{"message":"quota exceeded","statusCode":429,"isRetryable":true}}},"parts":`+parts+`}`)
		server, calls = fixture, func() int { return *sent }
	}
	return &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}, calls
}

func TestFallbackQuotaSwitchRefusesUnsafeOpencodeExecutions(t *testing.T) {
	defer withFastEvidenceWait(t)()
	for _, version := range []string{"v1", "v2"} {
		for _, evidence := range []opencodeToolEvidence{opencodeToolsRan, opencodeToolsUnknown} {
			for _, envelope := range []string{"direct", "wrapped", "joined-first", "joined-last"} {
				t.Run(fmt.Sprintf("%s/%s/%s", version, evidence, envelope), func(t *testing.T) {
					adapter, attempts := quotaOpencodeFixture(t, version, evidence)
					var adapterErr error
					first := quotaErrorTransformAgent{Agent: adapter, transform: func(err error) error {
						adapterErr = err
						if evidence == opencodeToolsUnknown {
							err = errors.Join(errors.New("quota exceeded"), err)
						}
						safe := &opencodeMessageFailure{message: "quota exceeded", retryable: true}
						switch envelope {
						case "wrapped":
							return fmt.Errorf("invocation: %w", err)
						case "joined-first":
							return errors.Join(safe, fmt.Errorf("invocation: %w", err))
						case "joined-last":
							return errors.Join(fmt.Errorf("invocation: %w", err), safe)
						default:
							return err
						}
					}}
					next := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) { return &Result{Text: "replayed"}, nil }}
					chain := NewFallback([]Agent{first, next}, WithQuotaSwitch("")).(*fallbackAgent)
					switches := 0
					_, err := chain.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "write the file", OnLifecycle: func(event LifecycleEvent) {
						if event.Phase == LifecyclePhaseQuotaSwitch {
							switches++
						}
					}})
					marker := errOpencodeToolsAlreadyRan
					if evidence == opencodeToolsUnknown {
						marker = errOpencodeToolActivityUnknown
					}
					if adapterErr == nil || !errors.Is(adapterErr, marker) || !errors.Is(err, adapterErr) {
						t.Fatalf("adapter execution evidence lost: adapter=%v result=%v", adapterErr, err)
					}
					if next.calls != 0 || switches != 0 || chain.position.Load() != 0 || chain.Name() != "opencode" || attempts() != 1 {
						t.Fatalf("unsafe replay: successor=%d switches=%d position=%d active=%s attempts=%d", next.calls, switches, chain.position.Load(), chain.Name(), attempts())
					}
					if _, retry := classifyOpencodeTransient(err); retry {
						t.Fatalf("unsafe error acquired adapter retry eligibility: %v", err)
					}
				})
			}
		}
	}
}

func TestFallbackQuotaSwitchSafeOpencodeExecutionRequiresOptIn(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", version, enabled), func(t *testing.T) {
				first, attempts := quotaOpencodeFixture(t, version, opencodeToolsNone)
				next := &quotaTestAgent{name: "codex", run: func(RunOpts) (*Result, error) { return &Result{Text: "safe successor"}, nil }}
				var options []FallbackOption
				if enabled {
					options = append(options, WithQuotaSwitch(""))
				}
				chain := NewFallback([]Agent{first, next}, options...).(*fallbackAgent)
				switches := 0
				result, err := chain.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "write the file", OnLifecycle: func(event LifecycleEvent) {
					if event.Phase == LifecyclePhaseQuotaSwitch {
						switches++
					}
				}})
				if attempts() != 1 {
					t.Fatalf("quota error retried inside adapter: %d attempts", attempts())
				}
				if enabled {
					if err != nil || result == nil || result.Text != "safe successor" || next.calls != 1 || switches != 1 || chain.position.Load() != 1 {
						t.Fatalf("safe switch failed: result=%+v err=%v calls=%d switches=%d position=%d", result, err, next.calls, switches, chain.position.Load())
					}
				} else if err == nil || next.calls != 0 || switches != 0 || chain.position.Load() != 0 {
					t.Fatalf("default-off behavior changed: err=%v calls=%d switches=%d position=%d", err, next.calls, switches, chain.position.Load())
				}
			})
		}
	}
}
