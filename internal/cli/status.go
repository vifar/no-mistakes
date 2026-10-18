package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := git.FindGitRoot("."); err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "not in a git repository")
				return nil
			}
			p, d, err := openReadResources()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(cmd.OutOrStdout(), "repo not initialized (run 'no-mistakes init' first)")
					return nil
				}
				return err
			}
			defer d.Close()

			w := cmd.OutOrStdout()

			// Look up repo from current directory.
			repo, err := findRepo(d)
			if err != nil {
				fmt.Fprintln(w, err)
				return nil
			}

			fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo:"), repo.WorkingPath)
			remoteURL := repo.UpstreamURL
			if repo.ForkURL != "" {
				remoteURL = safeurl.Redact(remoteURL)
			}
			fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote:"), remoteURL)
			if repo.ForkURL != "" {
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork:"), safeurl.Redact(repo.ForkURL))
			}
			fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  gate:"), p.RepoDir(repo.ID))

			// Check daemon status.
			alive, _ := daemon.IsRunning(p)
			if alive {
				fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("daemon:"), sGreen.Render("●"), "running")
			} else {
				fmt.Fprintf(w, "  %s  %s %s\n", sDim.Render("daemon:"), sDim.Render("○"), "stopped")
			}

			// Check for active run.
			activeRun, err := d.GetActiveRun(repo.ID, "")
			if err != nil {
				return fmt.Errorf("check active run: %w", err)
			}
			if syncState := (&branchsync.Service{DB: d, Repo: repo, WorkDir: "."}).InspectCached(cmd.Context()); relevantCachedSyncState(syncState) {
				fmt.Fprintf(w, "\n  %s  %s\n", sDim.Render("local branch:"), humanSyncSummary(syncState))
			}
			if activeRun != nil {
				cfg, _ := config.LoadGlobal(p.ConfigFile())
				admission, err := d.GetOwnerAdmission(activeRun.ID)
				if err != nil {
					return err
				}
				if admission != nil {
					fmt.Fprintln(w, admission.Warning)
				} else if warning := ownerPolicyWarning(activeRun.Owner(), cfg); warning != "" {
					fmt.Fprintln(w, warning)
				}
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Active run"))
				sha := activeRun.HeadSHA[:minLen(len(activeRun.HeadSHA), 8)]
				ts := time.Unix(activeRun.CreatedAt, 0).Format(time.DateTime)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("     id:"), activeRun.ID)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" branch:"), activeRun.Branch)
				owner := activeRun.Owner()
				if owner == "" {
					owner = "OWNERLESS (nobody recorded to drive approval gates)"
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  owner:"), owner)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" status:"), runStatusStyle(activeRun.Status))
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("   head:"), sDim.Render(sha))
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("started:"), sDim.Render(ts))
			} else {
				fmt.Fprintf(w, "\n  %s\n", sDim.Render("no active run"))
			}

			return nil
		},
	}
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
