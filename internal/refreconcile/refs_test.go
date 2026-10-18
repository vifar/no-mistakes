package refreconcile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func fixture(t *testing.T) (*paths.Paths, *db.DB, *db.Repo, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	gate := p.RepoDir("repo")
	command(t, "init", "--bare", gate)
	tree := command(t, "--git-dir="+gate, "mktree")
	head := command(t, "--git-dir="+gate, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", tree, "-m", "seed")
	command(t, "--git-dir="+gate, "update-ref", "refs/heads/main", head)
	repo, err := d.InsertRepoWithID("repo", t.TempDir(), "local", "main")
	if err != nil {
		t.Fatal(err)
	}
	return p, d, repo, head
}

func command(t *testing.T, args ...string) string {
	t.Helper()
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(realGit, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestExactBranchHeadRunEvidenceAndResolution(t *testing.T) {
	p, d, repo, head := fixture(t)
	assertOrphan := func(want bool) {
		t.Helper()
		issues := Inspect(t.Context(), d, p, repo)
		if want {
			if len(issues) != 1 || issues[0].Code != OrphanedRef || issues[0].Ref != "refs/heads/main" || issues[0].Head != head {
				t.Fatalf("want exact orphan, got %+v", issues)
			}
		} else if len(issues) != 0 {
			t.Fatalf("recorded head remains orphaned: %+v", issues)
		}
	}
	assertOrphan(true) // Includes the default branch, not silently exempted.
	if _, err := d.InsertRun(repo.ID, "other", head, "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, "main", "older-head", "base"); err != nil {
		t.Fatal(err)
	}
	command(t, "--git-dir="+p.RepoDir(repo.ID), "update-ref", "refs/remotes/origin/main", head)
	command(t, "--git-dir="+p.RepoDir(repo.ID), "update-ref", "refs/no-mistakes/recover/test", head)
	assertOrphan(true) // Wrong branch/older head/internal refs cannot cover it.
	recorded, err := d.InsertRun(repo.ID, "main", head, "base")
	if err != nil {
		t.Fatal(err)
	}
	assertOrphan(false)
	if err := d.UpdateRunHeadSHA(recorded.ID, "rewritten-pipeline-head"); err != nil {
		t.Fatal(err)
	}
	assertOrphan(false) // Immutable submitted evidence survives a pipeline rewrite.
}

func TestRetainedNotificationEvidenceVersionsAndCorruption(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			p, d, repo, head := fixture(t)
			gate := p.RepoDir(repo.ID)
			// Unknown owner/options must survive byte-exact checksum validation.
			params, _ := json.Marshal(struct {
				Owner string `json:"owner"`
				Gate  string `json:"gate"`
				Ref   string `json:"ref"`
				New   string `json:"new"`
			}{"caller", gate, "refs/heads/main", head})
			record := notificationEnvelope{Version: version, RepoID: repo.ID, WorkingPath: repo.WorkingPath,
				GateIdentity: gate, PreviousRunID: "previous", Params: params, Validated: true}
			identity := record
			identity.Validated = false
			if version == 1 {
				identity.PreviousRunID = ""
			}
			id := digest(identity)
			if version == 3 {
				id = strings.Repeat("a", 64)
				record.ReceiveID = id
			}
			record.Checksum = digest(record)
			raw, _ := json.Marshal(record)
			dir := filepath.Join(p.Root(), "push-notifications")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, id+".json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if issues := Inspect(t.Context(), d, p, repo); len(issues) != 0 {
				t.Fatalf("valid notification did not cover head: %+v", issues)
			}
			// A record for the right head on a different branch is insufficient.
			wrong := strings.Replace(string(raw), "refs/heads/main", "refs/heads/other", 1)
			if err := os.WriteFile(path, []byte(wrong), 0600); err != nil {
				t.Fatal(err)
			}
			issues := Inspect(t.Context(), d, p, repo)
			if len(issues) != 2 || issues[0].Code != Failed || issues[1].Code != OrphanedRef {
				t.Fatalf("corrupt metadata hid orphan: %+v", issues)
			}
			if err := os.Rename(path, filepath.Join(dir, ".notification-uncommitted")); err != nil {
				t.Fatal(err)
			}
			issues = Inspect(t.Context(), d, p, repo)
			if len(issues) != 1 || issues[0].Code != OrphanedRef {
				t.Fatalf("pre-rename file counted as durable evidence: %+v", issues)
			}
		})
	}
}

func TestUnreadableGateReportsNamedFailure(t *testing.T) {
	p, d, repo, _ := fixture(t)
	if err := os.RemoveAll(p.RepoDir(repo.ID)); err != nil {
		t.Fatal(err)
	}
	issues := Inspect(t.Context(), d, p, repo)
	if len(issues) != 1 || issues[0].Code != Failed {
		t.Fatalf("missing gate disappeared silently: %+v", issues)
	}
}

func TestNotificationCannotHideDifferentRepositoryBranchOrHead(t *testing.T) {
	p, d, repo, head := fixture(t)
	gate := p.RepoDir(repo.ID)
	foreign := t.TempDir()
	dir := filepath.Join(p.Root(), "push-notifications")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []string{"repo", "working_path", "gate_identity", "params_gate", "ref", "head"} {
		t.Run(mismatch, func(t *testing.T) {
			params := struct{ Gate, Ref, New string }{gate, "refs/heads/main", head}
			record := notificationEnvelope{Version: 2, RepoID: repo.ID, WorkingPath: repo.WorkingPath, GateIdentity: gate}
			switch mismatch {
			case "repo":
				record.RepoID = "other-repo"
			case "working_path":
				record.WorkingPath = foreign
			case "gate_identity":
				record.GateIdentity = foreign
			case "params_gate":
				params.Gate = foreign
			case "ref":
				params.Ref = "refs/heads/other"
			case "head":
				params.New = strings.Repeat("b", 40)
			}
			// Lowercase gate/ref/new keys match the actual producer format.
			record.Params, _ = json.Marshal(struct {
				Gate string `json:"gate"`
				Ref  string `json:"ref"`
				New  string `json:"new"`
			}{params.Gate, params.Ref, params.New})
			identity := record
			identity.Params, _ = canonicalGateParams(record.Params, record.GateIdentity)
			id := digest(identity)
			record.Checksum = digest(record)
			raw, _ := json.Marshal(record)
			path := filepath.Join(dir, id+".json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(path) })
			issues := Inspect(t.Context(), d, p, repo)
			for _, issue := range issues {
				if issue.Code == OrphanedRef && issue.Ref == "refs/heads/main" && issue.Head == head {
					return
				}
			}
			t.Fatalf("mismatched notification hid orphan: %+v", issues)
		})
	}
}
