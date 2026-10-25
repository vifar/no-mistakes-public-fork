package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// holdPureTargetAdvance re-expresses a live comparison against the target the
// run recorded, when the target moving is the only thing that changed
// (issue #237).
//
// A target that fast-forwards under a head whose merge base it does not move
// leaves the pull request's own change byte-for-byte identical: the canonical
// diff `target...head` is `merge-base..head`, and neither end moved. If that
// head still merges cleanly into the new target, nothing the run reviewed,
// tested, or published was invalidated - the base merely moved, and a pull
// request merges from its own head. The comparison is therefore held at the
// recorded target instead of being treated as drift: no rewind, no restart,
// no new head, and the receipt and its generation stay exactly as they were.
//
// It returns the live comparison recomputed at recordedTarget, keeping live's
// head and identity, and whether the advance is pure. Anything else - a
// target that did not fast-forward, a merge base the advance moved (part of
// the change already landed on the target), or a conflict - reports false and
// leaves the caller's existing rewind, gate, or refusal in charge, as does
// rebase.integrate: always.
// holdReceiptTargetAdvance is holdPureTargetAdvance against a receipt. A
// target already retained for that receipt stays the floor: every later
// observation must equal or descend from it, so a rollback or sibling of the
// retained target is never held even when it still descends from the
// receipt's own target.
func holdReceiptTargetAdvance(c *pipeline.StepContext, receipt *db.PRContext, live db.PRContextCandidate) (db.PRContextCandidate, bool, error) {
	if c == nil || c.DB == nil || receipt == nil {
		return live, false, nil
	}
	retained, err := c.DB.GetRetainedPRTarget(c.Run.ID, receipt)
	if err != nil {
		return live, false, err
	}
	if retained != "" && retained != live.TargetSHA {
		if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", retained, live.TargetSHA); err != nil {
			return live, false, nil
		}
	}
	return holdPureTargetAdvance(c, receipt.TargetSHA, live)
}

func holdPureTargetAdvance(c *pipeline.StepContext, recordedTarget string, live db.PRContextCandidate) (db.PRContextCandidate, bool, error) {
	if c == nil || !keepsCleanHeads(c) || recordedTarget == "" || live.TargetSHA == "" || live.TargetSHA == recordedTarget || live.LocalHeadSHA == "" {
		return live, false, nil
	}
	held, err := prComparisonAtTarget(c, live.TargetBranch, recordedTarget, live.LocalHeadSHA)
	if err != nil {
		return live, false, err
	}
	pure, err := git.TargetAdvancedCleanly(c.Ctx, c.WorkDir, recordedTarget, live.TargetSHA, live.LocalHeadSHA, held.MergeBaseSHA)
	if err != nil || !pure {
		return live, false, err
	}
	held.PRURL, held.SourceRepo, held.SourceBranch, held.ForgeHeadSHA = live.PRURL, live.SourceRepo, live.SourceBranch, live.ForgeHeadSHA
	if c.Log != nil {
		c.Log(fmt.Sprintf("target %s advanced from %s to %s; head %s still merges cleanly with an unchanged diff, so the comparison is kept", live.TargetBranch, shortSHA(recordedTarget), shortSHA(live.TargetSHA), shortSHA(live.LocalHeadSHA)))
	}
	return held, true, nil
}
