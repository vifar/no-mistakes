package steps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestCIStepNoCIDeclarationSurvivesSelfStatusReadiness(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), `FAKE_CLI_PROTECTION_JSON={"contexts":["no-mistakes"]}`, `FAKE_CLI_RULES_JSON=[]`)
	bindExistingPRMutationFixture(t, sctx, "https://github.com/test/repo/pull/42", "main")
	sctx.Config.NoCI = true
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil || !run.CIReadyNoCI {
		t.Fatalf("trusted no_ci with only self status must record its declaration: at=%v no_ci=%v", run.CIReadyAt, run.CIReadyNoCI)
	}
}
