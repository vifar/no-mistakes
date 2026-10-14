package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// reviewReceipt exposes the durable comparison for an ordinary completed run.
// Callers must still reread the live PR and target before acting on it.
type reviewReceipt struct {
	RunID        string `json:"run_id"`
	SourceRepo   string `json:"source_repo"`
	SourceBranch string `json:"source_branch"`
	PRURL        string `json:"pr_url"`
	ForgeHeadSHA string `json:"forge_head_sha"`
	LocalHeadSHA string `json:"local_head_sha"`
	TargetBranch string `json:"target_branch"`
	TargetSHA    string `json:"target_sha"`
	MergeBaseSHA string `json:"merge_base_sha"`
	DiffDigest   string `json:"diff_digest"`
	Generation   int64  `json:"generation"`
}

func newAxiReviewReceiptCmd() *cobra.Command {
	var runID, phase string
	var sign bool
	cmd := &cobra.Command{
		Use: "review-receipt", Short: "Export the run's review comparison as JSON",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runID == "" {
				return emitError(cmd, 2, "--run is required")
			}
			if phase != "review" && phase != "ci-ready" {
				return emitError(cmd, 2, "receipt phase must be review or ci-ready")
			}
			var caller gatecontext.Result
			if sign {
				var err error
				caller, err = classifyGateControlCaller(cmd.Context())
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				if caller.Nested {
					return emitGateContextRefusal(cmd, caller)
				}
			}
			p, err := paths.New()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			database, err := db.OpenReadOnly(p.DB())
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			defer database.Close()
			run, err := database.GetRun(runID)
			if err != nil || run == nil {
				return emitError(cmd, 1, "selected run not found")
			}
			if run.Shipping == nil {
				if phase == "ci-ready" || sign {
					return emitError(cmd, 2, "--phase ci-ready and signing are only supported for strict shipping runs")
				}
				receipt, err := buildReviewReceipt(database, run)
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
			}
			repo, err := database.GetRepo(run.RepoID)
			if err != nil || repo == nil {
				return emitError(cmd, 1, "selected repository not found")
			}
			global, err := config.LoadGlobal(p.ConfigFile())
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			forge, err := forgecontext.Resolve(cmd.Context(), global.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			sctx := &pipeline.StepContext{Ctx: cmd.Context(), Run: run, Repo: repo, DB: database, WorkDir: run.WorktreePath(), Config: config.Merge(global, &config.RepoConfig{}), ForgeContext: forge}
			host, err := steps.ShippingProofHost(sctx)
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			payload, err := shipping.Export(cmd.Context(), database, repo, runID, phase, host)
			if err != nil {
				return emitError(cmd, 1, safeurl.RedactText(err.Error()))
			}
			if sign {
				payload, err = shipping.Sign(payload, cmd.InOrStdin())
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(payload))
			return err
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact run ID to inspect")
	cmd.Flags().StringVar(&phase, "phase", "review", "proof phase: review or ci-ready (strict shipping only)")
	cmd.Flags().BoolVar(&sign, "signing-key-stdin", false, "sign strict shipping proof using an Ed25519 seed on stdin")
	return cmd
}

func buildReviewReceipt(database *db.DB, run *db.Run) (reviewReceipt, error) {
	if run == nil || run.Status != types.RunCompleted || run.TerminalHeadVerifiedAt == nil {
		return reviewReceipt{}, fmt.Errorf("run has no verified terminal result")
	}
	context, err := database.GetRunPRContext(run.ID)
	if err != nil {
		return reviewReceipt{}, err
	}
	if context == nil || context.LocalHeadSHA != run.HeadSHA || context.TargetSHA == "" || context.DiffDigest == "" {
		return reviewReceipt{}, fmt.Errorf("run has no current exact PR comparison receipt")
	}
	sourceRepo, sourceBranch, err := receiptSourceIdentity(context)
	if err != nil {
		return reviewReceipt{}, err
	}
	return reviewReceipt{RunID: run.ID, SourceRepo: sourceRepo, SourceBranch: sourceBranch,
		PRURL: context.PRURL, ForgeHeadSHA: context.ForgeHeadSHA, LocalHeadSHA: context.LocalHeadSHA,
		TargetBranch: context.TargetBranch, TargetSHA: context.TargetSHA,
		MergeBaseSHA: context.MergeBaseSHA, DiffDigest: context.DiffDigest,
		Generation: context.Generation}, nil
}

func receiptSourceIdentity(context *db.PRContext) (string, string, error) {
	if context == nil || strings.TrimSpace(context.SourceRepo) == "" || strings.TrimSpace(context.SourceBranch) == "" {
		return "", "", fmt.Errorf("comparison receipt has no source identity")
	}
	return context.SourceRepo, context.SourceBranch, nil
}
