package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

const ownerPushOptionPrefix = "no-mistakes.owner="

func bindOwnerFlag(cmd *cobra.Command) {
	cmd.Flags().String("owner", "", "session/thread or person responsible for driving gates (default: NO_MISTAKES_OWNER)")
}
func launchOwner(cmd *cobra.Command) (string, error) {
	owner := os.Getenv("NO_MISTAKES_OWNER")
	if cmd.Flags().Changed("owner") {
		owner, _ = cmd.Flags().GetString("owner")
	}
	if err := types.ValidateRunOwner(owner); err != nil {
		return "", err
	}
	return strings.TrimSpace(owner), nil
}
func formatOwnerPushOption(owner string) string {
	return ownerPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(owner))
}
func parseOwnerPushOptions(options []string) (string, error) {
	owner := ""
	seen := false
	for _, opt := range options {
		encoded, ok := strings.CutPrefix(opt, ownerPushOptionPrefix)
		if !ok {
			continue
		}
		if seen {
			return "", fmt.Errorf("duplicate owner push option")
		}
		seen = true
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode owner push option: %w", err)
		}
		owner = string(decoded)
	}
	if err := types.ValidateRunOwner(owner); err != nil {
		return "", err
	}
	return strings.TrimSpace(owner), nil
}

func probeLaunchOwner(client *ipc.Client, owner string) error {
	var result ipc.ProbeRunOwnerResult
	if err := client.Call(ipc.MethodProbeRunOwner, &ipc.ProbeRunOwnerParams{Owner: owner}, &result); err != nil {
		// Legacy daemons have no owner policy. Only an ownerless launch can
		// remain compatible; an explicit owner must never be silently dropped.
		var rpcErr *ipc.RPCError
		if owner == "" && errors.As(err, &rpcErr) && rpcErr.Code == ipc.ErrMethodNotFound {
			return nil
		}
		return fmt.Errorf("daemon cannot accept launch ownership (update it before launching with --owner): %w", err)
	}
	if !result.OK {
		return fmt.Errorf("daemon did not confirm launch ownership support")
	}
	return nil
}

func ownerPolicyWarning(owner string, cfg *config.GlobalConfig) string {
	if owner != "" || cfg == nil || !cfg.Intent.RequireOwner {
		return ""
	}
	return "Ownerless run. Current require_owner policy: enforced on AXI launches; TUI and direct push are warned, not refused, until #82"
}
