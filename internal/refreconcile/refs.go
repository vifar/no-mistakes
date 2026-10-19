// Package refreconcile finds managed branch heads without durable receive evidence.
// It never infers launch options, owner identity, or permission to start a run.
package refreconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const OrphanedRef = "STARTUP_ORPHANED_REF"
const Failed = "STARTUP_RECONCILIATION_FAILED"

// Issue contains only ref identity and diagnostics, never receive options or intent.
type Issue struct {
	Code   string `json:"code"`
	RepoID string `json:"repo_id"`
	Ref    string `json:"ref,omitempty"`
	Head   string `json:"head,omitempty"`
	Detail string `json:"detail"`
}

func (i Issue) String() string {
	return fmt.Sprintf("%s: repo=%s ref=%s head=%s; %s", i.Code, i.RepoID, i.Ref, i.Head, i.Detail)
}

// Inspect is read-only and deliberately checks all branch heads, including the
// default branch. Seeded branches can lack receive evidence too; guessing that
// such a head is harmless would hide a lost receive on the same branch.
// Remote-tracking, tag/archive and recovery refs are not pipeline submissions.
// Status calls this again so a newly recorded run or removed ref clears a report
// without requiring a daemon restart. Startup separately logs the detection.
func Inspect(ctx context.Context, d *db.DB, p *paths.Paths, repo *db.Repo) []Issue {
	issues, err := inspect(ctx, d, p, repo)
	if err != nil {
		issues = append(issues, Issue{Code: Failed, RepoID: repo.ID, Detail: err.Error()})
	}
	return issues
}

func inspect(ctx context.Context, d *db.DB, p *paths.Paths, repo *db.Repo) ([]Issue, error) {
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, fmt.Errorf("read run evidence: %w", err)
	}
	covered := make(map[string]bool)
	for _, run := range runs {
		ref := "refs/heads/" + run.Branch
		covered[ref+"\x00"+run.HeadSHA] = true
		if run.SubmittedHeadSHA != nil {
			covered[ref+"\x00"+*run.SubmittedHeadSHA] = true
		}
		if run.LastPushedSHA != nil {
			covered[ref+"\x00"+*run.LastPushedSHA] = true
		}
	}
	issues := notificationEvidence(p, repo, covered)
	output, err := git.RunBare(ctx, p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname)%09%(objectname)", "refs/heads/")
	if err != nil {
		return issues, fmt.Errorf("enumerate managed branch refs: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		ref, head, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(ref, "refs/heads/") || head == "" {
			return issues, fmt.Errorf("invalid managed ref enumeration")
		}
		if !covered[ref+"\x00"+head] {
			issues = append(issues, Issue{Code: OrphanedRef, RepoID: repo.ID, Ref: ref, Head: head,
				Detail: "no run or valid retained notification covers this head; receive options and ownership cannot be reconstructed safely; preserve the ref and explicitly submit a new validation with the intended options"})
		}
	}
	return issues, nil
}

// notificationEnvelope mirrors the retained-notification envelope without
// importing admission behavior. Raw params preserve checksummed owner/options
// fields and their encoding rather than reserializing them through an IPC type.
type notificationEnvelope struct {
	Version       int             `json:"version"`
	RepoID        string          `json:"repo_id"`
	WorkingPath   string          `json:"working_path"`
	GateIdentity  string          `json:"gate_identity"`
	PreviousRunID string          `json:"previous_run_id"`
	Checksum      string          `json:"checksum"`
	Params        json.RawMessage `json:"params"`
	ReceiveID     string          `json:"receive_id,omitempty"`
	Validated     bool            `json:"validated,omitempty"`
}

func digest(record notificationEnvelope) string {
	raw, _ := json.Marshal(record)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func notificationEvidence(p *paths.Paths, repo *db.Repo, covered map[string]bool) []Issue {
	dir, err := os.Open(filepath.Join(p.Root(), "push-notifications"))
	if os.IsNotExist(err) {
		return nil // A missing journal cannot prove that no receive occurred.
	}
	failure := func(detail string) Issue { return Issue{Code: Failed, RepoID: repo.ID, Detail: detail} }
	if err != nil {
		return []Issue{failure("cannot read retained notifications")}
	}
	defer dir.Close()
	var issues []Issue
	for {
		entries, readErr := dir.ReadDir(100)
		for _, entry := range entries {
			// A pre-rename temporary file is NOT a durable notification.
			if !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			record, err := readNotification(filepath.Join(dir.Name(), entry.Name()))
			if err != nil {
				issues = append(issues, failure("invalid or unreadable retained notification "+entry.Name()))
				continue
			}
			if record.RepoID != repo.ID {
				continue
			}
			var params struct{ Gate, Ref, New string }
			expectedGate := canonical(p.RepoDir(repo.ID))
			if json.Unmarshal(record.Params, &params) != nil || record.WorkingPath != repo.WorkingPath ||
				expectedGate == "" || canonical(record.GateIdentity) != expectedGate || canonical(params.Gate) != expectedGate {
				issues = append(issues, failure("retained notification repository context differs: "+entry.Name()))
				continue
			}
			covered[params.Ref+"\x00"+params.New] = true
		}
		if readErr == io.EOF {
			return issues
		}
		if readErr != nil {
			return append(issues, failure("cannot enumerate retained notifications"))
		}
	}
}

func canonical(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "" // Neither side is permitted to match an unavailable path.
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}

func readNotification(path string) (*notificationEnvelope, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, fmt.Errorf("invalid notification file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("notification changed while opening")
	}
	var record notificationEnvelope
	decoder := json.NewDecoder(io.LimitReader(f, (4<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("notification trailing data")
	}
	checksum := record.Checksum
	record.Checksum = ""
	if checksum != digest(record) || record.Version < 1 || record.Version > 3 {
		return nil, fmt.Errorf("notification checksum/version differs")
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != sha256.Size || id != strings.ToLower(id) {
		return nil, fmt.Errorf("invalid notification identity")
	}
	if record.Version == 3 {
		if record.ReceiveID != id {
			return nil, fmt.Errorf("notification receive identity differs")
		}
	} else {
		identity := record
		identity.Validated = false
		if identity.Version == 1 {
			identity.PreviousRunID = ""
		}
		// Legacy IDs canonicalize only params.gate. Preserve all other keys and
		// their order so owner/options added by #66 do not change the digest.
		params, err := canonicalGateParams(record.Params, record.GateIdentity)
		if err != nil {
			return nil, err
		}
		identity.Params = params
		if digest(identity) != id {
			return nil, fmt.Errorf("legacy notification identity differs")
		}
	}
	return &record, nil
}

func canonicalGateParams(raw json.RawMessage, gate string) (json.RawMessage, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid notification params")
	}
	var out strings.Builder
	out.WriteByte('{')
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		if key == "gate" {
			value, _ = json.Marshal(gate)
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		encoded, _ := json.Marshal(key)
		out.Write(encoded)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return json.RawMessage(out.String()), nil
}
