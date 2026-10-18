package cli

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

func newDaemonNotificationsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "notifications", Short: "Inspect and replay retained post-receive notifications"}
	var after string
	var limit int
	for _, action := range []string{"list", "inspect", "replay"} {
		action := action
		use := action
		argCheck := cobra.NoArgs
		if action != "list" {
			use += " <id>"
			argCheck = cobra.ExactArgs(1)
		}
		sub := &cobra.Command{Use: use, Args: argCheck, RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			var result json.RawMessage
			method := ipc.MethodListPushNotifications
			params := ipc.PushNotificationParams{}
			if action == "list" {
				params.After = after
				params.Limit = limit
			}
			if action != "list" {
				params.ID = args[0]
			}
			if action == "inspect" {
				method = ipc.MethodInspectPushNotification
			}
			if action == "replay" {
				method = ipc.MethodReplayPushNotification
			}
			if err := client.Call(method, &params, &result); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
		if action == "list" {
			sub.Flags().StringVar(&after, "after", "", "continue after the previous page's next_cursor")
			sub.Flags().IntVar(&limit, "limit", 100, "maximum summaries per page (1-100)")
		}
		cmd.AddCommand(sub)
	}
	return cmd
}
