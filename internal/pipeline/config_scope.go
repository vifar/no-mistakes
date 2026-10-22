package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var errSharedConfigBoundary = errors.New("shared config boundary")

// executeWithConfigProtection bounds every execution and fix round, including
// preparation and agent children. Check runs even after a step error or cancel,
// before any Git discovery in the following step can obscure the writer.
func executeWithConfigProtection(name types.StepName, sctx *StepContext, execute func(*StepContext) (*StepOutcome, error)) (out *StepOutcome, err error) {
	guard, err := git.ProtectSharedConfig(sctx.Ctx, sctx.GateDir)
	if err != nil {
		return nil, fmt.Errorf("%s shared config preflight: %w", name, err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), 10*time.Second)
		defer cancel()
		if configErr := errors.Join(guard.Check(ctx), guard.Close()); configErr != nil {
			err = errors.Join(err, fmt.Errorf("%s %w: %w", name, errSharedConfigBoundary, configErr))
		}
	}()
	return execute(sctx)
}
