package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// executeWithConfigProtection bounds every execution and fix round, including
// preparation and agent children. Check runs even after a step error or cancel,
// before any Git discovery in the following step can obscure the writer.
func executeWithConfigProtection(step Step, sctx *StepContext) (out *StepOutcome, err error) {
	if _, statErr := os.Stat(sctx.GateDir); os.IsNotExist(statErr) {
		// Some embedding/unit-test executors use an ordinary checkout without a gate.
		return step.Execute(sctx)
	} else if statErr != nil {
		return nil, statErr
	}
	guard, err := git.ProtectSharedConfig(sctx.Ctx, sctx.GateDir)
	if err != nil {
		return nil, fmt.Errorf("%s shared config preflight: %w", step.Name(), err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), 10*time.Second)
		defer cancel()
		if configErr := errors.Join(guard.Check(ctx), guard.Close()); configErr != nil {
			err = errors.Join(err, fmt.Errorf("%s shared config boundary: %w", step.Name(), configErr))
		}
	}()
	return step.Execute(sctx)
}
