package cli

import (
	"fmt"
	"strconv"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// fetchSlots reads the running daemon's run-concurrency cap and slot usage. It
// returns nil when no daemon answers: slot usage is live daemon state with no
// durable copy, so a stopped daemon simply has none to show. Replaced in tests.
var fetchSlots = func(p *paths.Paths) *ipc.SlotsResult {
	if alive, _ := daemon.IsRunning(p); !alive {
		return nil
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil
	}
	defer client.Close()
	var result ipc.SlotsResult
	if err := client.CallWithTimeout(ipc.MethodGetSlots, nil, &result, 2*time.Second); err != nil {
		return nil
	}
	return &result
}

var fetchRunScheduling = func(p *paths.Paths, runID string) *ipc.RunInfo {
	if p == nil {
		return nil
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil
	}
	defer client.Close()
	var result ipc.GetRunResult
	if err := client.CallWithTimeout(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result, 2*time.Second); err != nil {
		return nil
	}
	if result.Run == nil || result.Run.ID != runID {
		return nil
	}
	return result.Run
}

// slotsSummary is the one-line slot usage: "2/3 in use, 4 queued". Runs that
// released their slot while parked are named only when there are any, unless
// always is set (doctor reports the count even at zero).
func slotsSummary(r ipc.SlotsResult, always ...bool) string {
	usage := fmt.Sprintf("%d/%d in use", r.InUse, r.MaxConcurrentRuns)
	if r.MaxConcurrentRuns == 0 {
		usage = fmt.Sprintf("%d in use (no cap)", r.InUse)
	}
	out := fmt.Sprintf("%s, %d queued", usage, r.Queued)
	if r.ParkedReleased > 0 || len(always) > 0 && always[0] {
		out += fmt.Sprintf(", %d parked (slot released)", r.ParkedReleased)
	}
	return out
}

// slotsFields renders the `slots:` line for axi status and the home view, or
// nothing when the daemon is not running.
func slotsFields(p *paths.Paths) []toon.Field {
	if p == nil {
		return nil
	}
	r := fetchSlots(p)
	if r == nil {
		return nil
	}
	return []toon.Field{{Key: "slots", Value: slotsSummary(*r)}}
}

func newDaemonSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change a daemon setting without restarting it",
	}
	cmd.AddCommand(newDaemonSetMaxConcurrentRunsCmd())
	return cmd
}

func newDaemonSetMaxConcurrentRunsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "max-concurrent-runs N",
		Short: "Set how many pipeline runs execute at once (0 = unlimited)",
		Long: "Writes daemon.max_concurrent_runs to the global config and tells the running\n" +
			"daemon to apply it immediately. Lowering the cap never stops or pauses a\n" +
			"running run: it only holds back new starts until usage drops below it. Raising\n" +
			"it starts queued runs up to the new cap at once. A stopped daemon reads the\n" +
			"value when it next starts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.set", func() error {
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 0 {
					return fmt.Errorf("max-concurrent-runs must be a whole number of 0 or more (0 = unlimited), got %q", args[0])
				}
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := config.SetGlobalDaemonSetting(p.ConfigFile(), "max_concurrent_runs", strconv.Itoa(n)); err != nil {
					return err
				}
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  daemon.max_concurrent_runs = %d written to %s\n", n, p.ConfigFile())
				alive, _ := daemonIsRunningFn(p)
				if !alive {
					fmt.Fprintln(w, "  daemon not running: the value applies when it starts")
					return nil
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					return fmt.Errorf("saved, but could not reach the daemon to apply it now (it re-reads the file within seconds): %w", err)
				}
				defer client.Close()
				var usage ipc.SlotsResult
				if err := client.CallWithTimeout(ipc.MethodReloadSlots, nil, &usage, 10*time.Second); err != nil {
					return fmt.Errorf("saved, but the daemon did not confirm it (it re-reads the file within seconds): %w", err)
				}
				fmt.Fprintf(w, "  applied: slots %s\n", slotsSummary(usage))
				return nil
			})
		},
	}
}
