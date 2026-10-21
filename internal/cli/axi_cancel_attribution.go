package cli

import (
	"os"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func cancelRunParams(cmd *cobra.Command, runID string) *ipc.CancelRunParams {
	caller := os.Getenv("NO_MISTAKES_OWNER")
	if cmd.Flags().Changed("owner") {
		caller, _ = cmd.Flags().GetString("owner")
	}
	reason, _ := cmd.Flags().GetString("reason")
	return &ipc.CancelRunParams{RunID: runID, Caller: caller, Client: "axi", Reason: reason}
}
