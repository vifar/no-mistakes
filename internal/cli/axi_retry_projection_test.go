package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRetryProjectionFromReadOnlyDatabase(t *testing.T) {
	dir, p, database, repo := setupReaderRepo(t)
	chdir(t, dir)
	head := cliGit(t, dir, "rev-parse", "HEAD")
	source, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(source.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(source.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	for i, trigger := range []string{"initial", "auto_fix", "coverage"} {
		if _, err := database.InsertStepRound(step.ID, i+1, trigger, nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 3, ""); err != nil {
		t.Fatal(err)
	}
	step, err = database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(retry.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	settleUnmanagedRunOnCleanup(t, database, retry.ID)
	link := db.RunRetry{RunID: retry.ID, SourceRunID: source.ID, StepName: types.StepTest, SourceRound: 4, SourceError: "provider unavailable", CheckpointHeadSHA: head}
	if err := database.InsertRunRetry(link); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertCarriedStepResult(retry.ID, step); err != nil {
		t.Fatal(err)
	}
	reader, err := db.OpenReader(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	steps, err := reader.GetStepsByRun(retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	rv := runViewFromDB(retry, steps, reader)
	annotateRunView(&axiEnv{d: reader, p: p}, &rv)
	if rv.RetryOf == nil || rv.RetryOf.RunID != source.ID || rv.RetryOf.Step != types.StepTest || rv.RetryOf.Round != 4 || rv.RetryOf.Error != link.SourceError || rv.RetryOf.CheckpointHeadSHA != head {
		t.Fatalf("missing retry lineage: %+v", rv.RetryOf)
	}
	if len(rv.Steps) != 1 || rv.Steps[0].RoundCount != 3 || rv.Steps[0].FixRoundCount != 1 {
		t.Fatalf("carried step lost source rounds: %+v", rv.Steps)
	}
	sourceView := runViewFromDB(source, nil, reader)
	if sourceView.RetriedBy != retry.ID {
		t.Fatalf("retried_by=%q want=%q", sourceView.RetriedBy, retry.ID)
	}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"axi", "status"}, []string{"run:", "retry_of:", fmt.Sprintf("run: %q", source.ID), "failed_round: 4"}},
		{[]string{"axi", "status", "--run", source.ID}, []string{fmt.Sprintf("retried_by: %q", retry.ID)}},
		{[]string{"axi"}, []string{"active_run:", "retry_of:", fmt.Sprintf("run: %q", source.ID)}},
	} {
		out, err := executeCmd(tc.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Fatalf("%v missing %q:\n%s", tc.args, want, out)
			}
		}
	}
	cliGit(t, dir, "checkout", "-b", "other")
	out, err := executeCmd("axi")
	if err != nil || !strings.Contains(out, "other_branch_active_run:") || !strings.Contains(out, "retry_of:") || !strings.Contains(out, fmt.Sprintf("run: %q", source.ID)) {
		t.Fatalf("other branch home lost lineage: %v\n%s", err, out)
	}
}
