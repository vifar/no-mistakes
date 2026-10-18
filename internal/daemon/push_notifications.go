package daemon

import (
	"bytes"
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
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

type pushNotification struct {
	Version       int                    `json:"version"`
	RepoID        string                 `json:"repo_id"`
	WorkingPath   string                 `json:"working_path"`
	GateIdentity  string                 `json:"gate_identity"`
	PreviousRunID string                 `json:"previous_run_id"`
	Checksum      string                 `json:"checksum"`
	Params        ipc.PushReceivedParams `json:"params"`
}

type retainedPush struct {
	ID     string
	Record *pushNotification
}
type pushNotificationKey struct{}

func (m *RunManager) notificationPath(id string) string {
	return filepath.Join(m.paths.Root(), "push-notifications", id+".json")
}

func notificationID(record *pushNotification) string {
	copyRecord := *record
	copyRecord.PreviousRunID = ""
	copyRecord.Checksum = ""
	copyRecord.Params.Gate = copyRecord.GateIdentity
	raw, _ := json.Marshal(copyRecord)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func notificationChecksum(record *pushNotification) string {
	copyRecord := *record
	copyRecord.Checksum = ""
	raw, _ := json.Marshal(copyRecord)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func syncNotificationDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (m *RunManager) retainPush(params *ipc.PushReceivedParams, repo *db.Repo) (*retainedPush, error) {
	m.notificationMu.Lock()
	defer m.notificationMu.Unlock()
	record := &pushNotification{Version: 1, RepoID: repo.ID, WorkingPath: repo.WorkingPath, GateIdentity: canonicalRoot(params.Gate), Params: *params}
	id := notificationID(record)
	path := m.notificationPath(id)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := syncNotificationDir(m.paths.Root()); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		existing, err := m.readNotification(id)
		if err != nil {
			return nil, err
		}
		if err := syncNotificationDir(dir); err != nil {
			return nil, err
		}
		return &retainedPush{ID: id, Record: existing}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	rows, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, err
	}
	for _, run := range rows {
		if run.Branch == branchFromRef(params.Ref) {
			record.PreviousRunID = run.ID
			break
		}
	}
	record.Checksum = notificationChecksum(record)
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".notification-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return nil, err
	}
	if err = syncNotificationDir(dir); err != nil {
		return nil, err
	}
	return &retainedPush{ID: id, Record: record}, nil
}

func (m *RunManager) readNotification(id string) (*pushNotification, error) {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != sha256.Size || id != strings.ToLower(id) {
		return nil, fmt.Errorf("invalid notification ID")
	}
	path := m.notificationPath(id)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, fmt.Errorf("invalid notification file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record pushNotification
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("corrupt notification: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("corrupt notification: trailing data")
	}
	if notificationID(&record) != id || record.Version != 1 || record.Checksum != notificationChecksum(&record) {
		return nil, fmt.Errorf("corrupt notification: identity/version mismatch")
	}
	return &record, nil
}

func (m *RunManager) validateNotification(ctx context.Context, retained *retainedPush) error {
	record := retained.Record
	p := &record.Params
	repoID, err := ownedGateRepoID(m.paths, p.Gate)
	if err != nil {
		return err
	}
	if repoID != record.RepoID || canonicalRoot(p.Gate) != record.GateIdentity {
		return fmt.Errorf("notification gate context changed")
	}
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return err
	}
	if repo == nil || repo.WorkingPath != record.WorkingPath {
		return fmt.Errorf("notification repository context changed")
	}
	if !strings.HasPrefix(p.Ref, "refs/heads/") {
		return fmt.Errorf("notification is not a branch update")
	}
	if _, err := git.Run(ctx, p.Gate, "check-ref-format", p.Ref); err != nil {
		return err
	}
	head, err := git.Run(ctx, p.Gate, "rev-parse", "--verify", p.Ref+"^{commit}")
	if err != nil || head != p.New {
		return fmt.Errorf("stale notification: current ref/head differs from accepted receive")
	}
	admitted, diagnostic, err := m.db.PushNotificationRuns(retained.ID)
	if err != nil {
		return err
	}
	if admitted != nil {
		return nil
	}
	rows, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return err
	}
	latest := ""
	for _, run := range rows {
		if diagnostic != nil && run.ID == diagnostic.ID {
			continue
		}
		if run.Branch == branchFromRef(p.Ref) {
			latest = run.ID
			break
		}
	}
	if latest != record.PreviousRunID {
		return fmt.Errorf("stale notification: branch run context changed")
	}
	return nil
}

func (m *RunManager) HandlePushReceived(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	if git.IsZeroSHA(params.New) {
		return "", fmt.Errorf("ref deletion push, no pipeline to run")
	}
	repoID, err := ownedGateRepoID(m.paths, params.Gate)
	if err != nil {
		return "", err
	}
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo for gate %s", params.Gate)
	}
	retained, err := m.retainPush(params, repo)
	if err != nil {
		return "", fmt.Errorf("retain post-receive notification failed (accepted Git ref may lack a run): %w", err)
	}
	if m.afterPushRetained != nil {
		m.afterPushRetained()
	}
	admitted, _, err := m.db.PushNotificationRuns(retained.ID)
	if err == nil && admitted != nil {
		return admitted.ID, nil
	}
	if err == nil {
		ctx = context.WithValue(ctx, pushNotificationKey{}, retained)
		var id string
		id, err = m.handleRetainedPush(ctx, params)
		if err == nil {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w; retained notification %s at %s; resolve with NM_HOME=%q no-mistakes daemon notifications replay %s", err, retained.ID, m.notificationPath(retained.ID), m.paths.Root(), retained.ID)
}

func (m *RunManager) InspectPushNotification(id string) (*ipc.PushNotificationInfo, error) {
	record, err := m.readNotification(id)
	if err != nil {
		return nil, err
	}
	admitted, diagnostic, err := m.db.PushNotificationRuns(id)
	if err != nil {
		return nil, err
	}
	info := &ipc.PushNotificationInfo{ID: id, Path: m.notificationPath(id), Pending: admitted == nil, Params: &record.Params}
	if admitted != nil {
		info.RunID = admitted.ID
	}
	if diagnostic != nil {
		info.DiagnosticRunID = diagnostic.ID
	}
	return info, nil
}

func (m *RunManager) ListPushNotifications() ([]ipc.PushNotificationInfo, error) {
	entries, err := os.ReadDir(filepath.Join(m.paths.Root(), "push-notifications"))
	if os.IsNotExist(err) {
		return []ipc.PushNotificationInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []ipc.PushNotificationInfo{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		info, err := m.InspectPushNotification(id)
		if err != nil {
			result = append(result, ipc.PushNotificationInfo{ID: id, Path: m.notificationPath(id), Pending: true, Error: err.Error()})
			continue
		}
		result = append(result, *info)
	}
	return result, nil
}

func (m *RunManager) ReplayPushNotification(ctx context.Context, id string) (string, error) {
	record, err := m.readNotification(id)
	if err != nil {
		return "", err
	}
	retained := &retainedPush{ID: id, Record: record}
	if err := m.validateNotification(ctx, retained); err != nil {
		return "", err
	}
	return m.HandlePushReceived(ctx, &record.Params)
}
