package branchsync

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// This file is the DIAGNOSTIC half of the blocked_recover_diverged refusal.
// It reports what the refusal compared and which tests it ran, so "diverged"
// stops reading as "your work was lost" when the branch's commit was REWRITTEN
// by a target advance rather than dropped.
//
// It NEVER decides anything.
//
// The containment proof stays exactly where it was: preservedContainsLocalWork
// still answers "does the preserved head carry every local change" with an
// executable three-way merge, and nothing in this file can change that answer.
// A patch identity is reported here because it helps a human understand a
// refusal, not because it proves containment. The codebase's own rule (see
// preservedContainsLocalWork) is that patch IDs discard hunk locations and
// whitespace, so they cannot tell a genuine replay from a same-shaped edit to
// a different identical block. A containment claim built on them is not a
// proof, and this path exists only to protect people's unlanded code.
//
// Every function here is read-only. Nothing writes a ref, a file, or a row, and
// no function here is consulted before the refusal decision is already made.

// divergencePatchScanLimit bounds how many preserved-side commits the patch
// identity diagnostic reads.
//
// The search is a diagnostic, so a bounded miss is safe: it reports nothing
// rather than guessing. The bound keeps a long preserved history from turning
// one refusal into an unbounded diff stream.
const divergencePatchScanLimit = 200

// divergenceDiagnostic is the read-only evidence behind one diverged refusal.
//
// It exists so the refusal can name the two heads it compared and the tests it
// ran. It decides nothing: the containment proof is preservedContainsLocalWork,
// and no field here feeds it.
type divergenceDiagnostic struct {
	// LocalHead is the local branch head the refusal compared.
	LocalHead string
	// PreservedHead is the other head the refusal compared: the preserved
	// pipeline head in custody recovery, the pipeline-pushed head in the
	// strict fast-forward refusal.
	PreservedHead string
	// PreservedLabel names the other head the way the operator-facing text
	// does.
	PreservedLabel string
	// MergeBase is the common ancestor the three-way merge proof used.
	MergeBase string
	// AncestryHeld reports whether the local head is an ancestor of the
	// other head.
	AncestryHeld bool
	// MergeProofHeld reports whether the executable three-way merge proof
	// showed the other head carries every local change.
	MergeProofHeld bool
	// ConflictPaths names the paths whose content conflicted in the
	// three-way merge, when the merge conflicted.
	ConflictPaths []string
	// PatchIdentityMatch names a commit in the other head's history that
	// carries the same patch identity as the local head's own change. It is
	// a DIAGNOSTIC ONLY: it is never a containment claim and never enables
	// an adoption path.
	PatchIdentityMatch string
	// PatchIdentityChecked reports whether the patch identity comparison
	// could be made at all. It is false when the local head has no own
	// change to measure.
	PatchIdentityChecked bool
	// PatchIdentityBounded reports that the search covered only the most
	// recent commits the other head adds, so a match further back was not
	// looked for.
	PatchIdentityBounded bool
}

// divergenceDescription renders the operator-facing explanation of a diverged
// refusal. It names the two commits that were compared, states which tests
// were used, and reports a patch identity match as a diagnostic.
//
// The wording is deliberately careful about what the refusal does and does not
// claim. "Diverged" says the two heads are not interchangeable; it does not
// say the operator's work was lost, and the diagnostic never says so either.
func (d divergenceDiagnostic) divergenceDescription() string {
	preservedLabel := d.PreservedLabel
	if preservedLabel == "" {
		preservedLabel = "preserved pipeline head"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the refusal compared the local head %s with the %s %s", d.LocalHead, preservedLabel, d.PreservedHead)
	b.WriteString("; the only tests it ran were Git ancestry (git merge-base --is-ancestor) and the executable three-way merge proof (git merge-tree --write-tree --merge-base)")
	if d.AncestryHeld {
		b.WriteString("; ancestry holds, the local head is an ancestor of the other head")
	} else {
		b.WriteString("; ancestry does not hold, the local head is not an ancestor of the other head")
	}
	if d.MergeProofHeld {
		b.WriteString("; the three-way merge proof holds, the other head carries every local change")
	} else {
		b.WriteString("; the three-way merge proof does not hold, the other head is not proven to carry every local change")
	}
	if len(d.ConflictPaths) > 0 {
		fmt.Fprintf(&b, "; the three-way merge conflicts in %s", strings.Join(d.ConflictPaths, ", "))
	}
	switch {
	case d.PatchIdentityMatch != "":
		fmt.Fprintf(&b, "; a commit in the %s carries the same patch identity as the local head's own change (%s), the same patch under a different commit identity - that is a diagnostic for the operator and not a containment proof, so it was not one of the tests that decided this and nothing was adopted", preservedLabel, d.PatchIdentityMatch)
	case d.PatchIdentityChecked && d.PatchIdentityBounded:
		fmt.Fprintf(&b, "; no commit in the most recent %d commits the %s adds carries the same patch identity as the local head's own change, and the search was bounded, so a match further back was not looked for - that is a diagnostic, not a containment proof, and it was not one of the tests that decided this", divergencePatchScanLimit, preservedLabel)
	case d.PatchIdentityChecked:
		fmt.Fprintf(&b, "; no commit in the %s carries the same patch identity as the local head's own change - that is a diagnostic, not a containment proof, and it was not one of the tests that decided this", preservedLabel)
	}
	b.WriteString("; divergence here means the two heads are not interchangeable, not that the local work was lost")
	return b.String()
}

// collectDivergenceDiagnostic gathers the read-only evidence behind a diverged
// refusal. It runs the same ancestry and three-way merge tests the refusal
// decision used, and adds a patch identity diagnostic.
//
// The function never returns a decision. A caller that wants the containment
// answer must ask preservedContainsLocalWork.
func collectDivergenceDiagnostic(ctx context.Context, dir, local, preserved, preservedLabel string) divergenceDiagnostic {
	diagnostic := divergenceDiagnostic{
		LocalHead:      local,
		PreservedHead:  preserved,
		PreservedLabel: preservedLabel,
	}
	diagnostic.AncestryHeld = isAncestor(ctx, dir, local, preserved)
	base, err := git.Run(ctx, dir, "merge-base", local, preserved)
	if err == nil && base != "" {
		diagnostic.MergeBase = base
		proof := runContainmentProof(ctx, dir, base, local, preserved)
		diagnostic.MergeProofHeld = proof.held
		diagnostic.ConflictPaths = proof.conflictPaths
	}
	diagnostic.PatchIdentityMatch, diagnostic.PatchIdentityChecked, diagnostic.PatchIdentityBounded = patchIdentityDiagnostic(ctx, dir, local, preserved)
	return diagnostic
}

// parseMergeConflictPaths extracts the conflicted path names from
// `git merge-tree --write-tree` output.
//
// The output starts with the written tree OID, then one line per conflicted
// file, then a blank line, then the informational messages. A conflicted file
// line is either the bare path or the stage form "<mode> <object> <stage>\t<path>".
// Only the lines between the tree OID and the first blank line are paths.
func parseMergeConflictPaths(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil
	}
	if !isObjectID(lines[0]) {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		path := line
		if tab := strings.IndexByte(line, '\t'); tab >= 0 {
			path = line[tab+1:]
		}
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// isObjectID reports whether s is a full Git object ID, in either the SHA-1 or
// the SHA-256 form.
func isObjectID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// patchIdentityDiagnostic reports the commit in the preserved head's history
// that carries the same patch identity as the local head's own change.
//
// The result is a DIAGNOSTIC ONLY. It explains a refusal to a human; it is
// never a containment proof and it never enables an adoption path. A patch
// identity discards hunk locations and whitespace, so an equal identity can
// also come from a same-shaped edit to a different identical block, and the
// commit that carries it can even have been reverted later. That is exactly
// why this must not decide anything.
//
// The second result reports whether the comparison could be made at all, and
// the third reports that the search was bounded. The empty commit means no
// match was found.
func patchIdentityDiagnostic(ctx context.Context, dir, local, preserved string) (string, bool, bool) {
	localIdentity, ok := commitPatchIdentity(ctx, dir, local)
	if !ok {
		return "", false, false
	}
	// The change may be in the preserved head's history under a different
	// commit identity, which is the case a rewritten commit makes. The
	// search covers the commits the preserved head has that the local head
	// does not, so the local head itself is never a candidate.
	stream, err := git.Run(ctx, dir, "log", "--format=commit %H", "-p", "--no-merges", "--no-ext-diff",
		"--max-count="+strconv.Itoa(divergencePatchScanLimit), preserved, "^"+local)
	if err != nil {
		return "", true, false
	}
	identities, err := git.StablePatchIdentities(ctx, dir, stream)
	if err != nil {
		return "", true, false
	}
	for _, identity := range identities {
		if identity.PatchID != localIdentity || identity.Commit == "" || identity.Commit == local {
			continue
		}
		return shortCommit(ctx, dir, identity.Commit), true, false
	}
	count, err := git.Run(ctx, dir, "rev-list", "--no-merges", "--count", preserved, "^"+local)
	if err != nil {
		return "", true, false
	}
	bounded, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return "", true, false
	}
	return "", true, bounded > divergencePatchScanLimit
}

// shortCommit returns the abbreviated form of a commit, falling back to the
// full SHA when the abbreviation cannot be read.
func shortCommit(ctx context.Context, dir, commit string) string {
	short, err := git.Run(ctx, dir, "rev-parse", "--short", commit)
	if err != nil || short == "" {
		return commit
	}
	return short
}

// commitPatchIdentity computes the stable patch identity of one commit's own
// change: the diff against its first parent.
//
// It is the whole-commit form of the identity a human measures with
// `git show <sha> | git patch-id --stable`. The second result is false when
// the commit has no own change to measure, or when the diff cannot be read, in
// which case the caller has no identity to compare.
func commitPatchIdentity(ctx context.Context, dir, commit string) (string, bool) {
	if strings.TrimSpace(commit) == "" {
		return "", false
	}
	diff, err := git.Run(ctx, dir, "diff-tree", "-p", "--root", "--no-commit-id", "--no-ext-diff", "--no-textconv", commit)
	if err != nil || strings.TrimSpace(diff) == "" {
		return "", false
	}
	patchID, err := git.StablePatchIDFromDiff(ctx, dir, diff)
	if err != nil || patchID == "" {
		return "", false
	}
	return patchID, true
}
