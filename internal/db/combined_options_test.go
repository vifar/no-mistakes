package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCombinedOptionsAdmissionAndLaunchReceipt(t *testing.T) {
	t.Run("shipping and external owner are mutually exclusive without insert", func(t *testing.T) {
		d := openTestDB(t)
		repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.test/repo.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.InsertRunWithAllOptions(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, false, types.ExternalCIOwnerControllerShipPR, nil, nil, &types.ShippingOptions{Mode: types.ShippingFull})
		if err == nil {
			t.Fatal("shipping plus external owner was accepted")
		}
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM runs WHERE repo_id=?`, repo.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("runs after rejected insert = %d, %v; want 0", count, err)
		}
	})

	t.Run("mismatched owner or strict profile does not consume first observer", func(t *testing.T) {
		d := openTestDB(t)
		repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.test/repo.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
		run, err := d.InsertRunWithAllOptions(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "release", true, false, types.ExternalCIOwnerControllerShipPR, nil, pin, nil)
		if err != nil {
			t.Fatal(err)
		}
		claim := func(owner string, request *agentcfg.PiProfile) (*Run, bool, error) {
			return d.ClaimLaunchReceiptWithAllOptions(repo.ID, "feature", "nonce", "head", "generation", "digest", "release", false, nil, owner, request)
		}
		wrongOwner, claimed, err := claim("", pin)
		if err != nil || claimed || wrongOwner == nil || wrongOwner.ID != run.ID {
			t.Fatalf("mismatched owner claim = %#v, %v, %v", wrongOwner, claimed, err)
		}
		wrongPin := &agentcfg.PiProfile{Model: pin.Model, Effort: agentcfg.EffortLow}
		wrongProfile, claimed, err := claim(types.ExternalCIOwnerControllerShipPR, wrongPin)
		if err != nil || claimed || wrongProfile == nil || wrongProfile.ID != run.ID {
			t.Fatalf("mismatched strict profile claim = %#v, %v, %v", wrongProfile, claimed, err)
		}
		var observed any
		if err := d.sql.QueryRow(`SELECT launch_receipt_claimed_at FROM runs WHERE id=?`, run.ID).Scan(&observed); err != nil || observed != nil {
			t.Fatalf("first observer after rejected claims = %#v, %v; want NULL", observed, err)
		}
		accepted, claimed, err := claim(types.ExternalCIOwnerControllerShipPR, pin)
		if err != nil || !claimed || accepted == nil || accepted.ID != run.ID {
			t.Fatalf("matching claim = %#v, %v, %v", accepted, claimed, err)
		}
		if accepted.ExternalCIOwner != types.ExternalCIOwnerControllerShipPR || accepted.PiProfile == nil || *accepted.PiProfile != *pin || accepted.PRBaseBranch == nil || *accepted.PRBaseBranch != "release" || !accepted.PRBaseBranchRequested {
			t.Fatalf("accepted union fields = %#v", accepted)
		}
	})

	t.Run("ordinary receipt accepts and returns ordinary union fields", func(t *testing.T) {
		d := openTestDB(t)
		repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.test/repo.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.InsertRunWithAllOptions(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, false, "", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, claimed, err := d.ClaimLaunchReceiptWithAllOptions(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, nil, "", nil)
		if err != nil || !claimed || got == nil || got.ID != run.ID {
			t.Fatalf("ordinary claim = %#v, %v, %v", got, claimed, err)
		}
		if got.ExternalCIOwner != "" || got.Shipping != nil || got.PiProfile != nil || got.PRBaseBranch != nil || got.PRBaseBranchRequested {
			t.Fatalf("ordinary union fields = %#v", got)
		}
	})
}
