package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// conflictRewindTarget advances main with a change to the file the feature
// branch adds, so the head no longer merges cleanly into the target.
func conflictRewindTarget(t *testing.T, c *pipeline.StepContext) string {
	t.Helper()
	gitCmd(t, c.WorkDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("main's own feature line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", "feature.txt")
	gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target change")
	sha := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, c.WorkDir, "push", "origin", "main")
	gitCmd(t, c.WorkDir, "checkout", "feature")
	return sha
}

func assertReceiptUnchanged(t *testing.T, c *pipeline.StepContext, before *db.PRContext) {
	t.Helper()
	after, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil || after.PRContextCandidate != before.PRContextCandidate || after.Generation != before.Generation {
		t.Fatalf("receipt moved on a pure target advance:\nbefore %+v\nafter  %+v", before, after)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil || run.TargetAdvanceRewinds != 0 {
		t.Fatalf("a pure target advance was charged as a rewind: %+v %v", run, err)
	}
	if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending != nil {
		t.Fatalf("a pure target advance left a pending rewind: %+v %v", pending, err)
	}
}

// A target that only moved, under a head that still merges cleanly with an
// unchanged diff, is not drift at any boundary (#237): no rewind, no restart,
// and the receipt and its generation stay where Review and Test bound them.
func TestGuardHoldsPureTargetAdvanceAtEveryValidationBoundary(t *testing.T) {
	for _, step := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
		t.Run(string(step), func(t *testing.T) {
			c, selected := rewindFixture(t)
			c.Config.Rebase.Integrate = ""
			before, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			advanceRewindTarget(t, c)
			decision, err := guardPRContextWithSelection(c, step, selected, false)
			if err != nil {
				t.Fatalf("a pure target advance failed the %s boundary: %v", step, err)
			}
			if decision.RestartFrom != "" {
				t.Fatalf("a pure target advance restarted from %q at %s", decision.RestartFrom, step)
			}
			assertReceiptUnchanged(t, c, before)
		})
	}
}

// A published or strict run has no rewind to fall back on, so before #237 a
// moved target failed it outright ("start a new run"). A pure advance now
// keeps it going.
func TestGuardHoldsPureTargetAdvanceForPublishedAndStrictRuns(t *testing.T) {
	for _, kind := range []string{"published", "strict"} {
		t.Run(kind, func(t *testing.T) {
			c, selected := rewindFixture(t)
			c.Config.Rebase.Integrate = ""
			switch kind {
			case "published":
				if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
					t.Fatal(err)
				}
			case "strict":
				c.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"}
			}
			before, err := c.DB.GetRunPRContext(c.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			advanceRewindTarget(t, c)
			decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
			if err != nil {
				t.Fatalf("a pure target advance failed a %s run: %v", kind, err)
			}
			if decision.RestartFrom != "" {
				t.Fatalf("a pure target advance restarted a %s run from %q", kind, decision.RestartFrom)
			}
			assertReceiptUnchanged(t, c, before)
		})
	}
}

// A conflict is the one target change that still counts: an unpublished run
// rewinds to Rebase exactly as before, and a published one is still refused.
func TestGuardConflictingTargetAdvanceStillRewindsOrRefuses(t *testing.T) {
	t.Run("unpublished rewinds", func(t *testing.T) {
		c, selected := rewindFixture(t)
		c.Config.Rebase.Integrate = ""
		conflictRewindTarget(t, c)
		decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
		if err != nil || decision.RestartFrom != types.StepRebase {
			t.Fatalf("a conflicting target advance must rewind: %+v, %v", decision, err)
		}
		if pending, err := c.DB.GetPendingTargetRewind(c.Run.ID); err != nil || pending == nil {
			t.Fatalf("a conflicting advance left no pending integration: %+v %v", pending, err)
		}
	})
	t.Run("published refuses", func(t *testing.T) {
		c, selected := rewindFixture(t)
		c.Config.Rebase.Integrate = ""
		if err := c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: c.Run.HeadSHA}); err != nil {
			t.Fatal(err)
		}
		conflictRewindTarget(t, c)
		if _, err := guardPRContextWithSelection(c, types.StepTest, selected, false); err == nil {
			t.Fatal("a published run accepted a target its head conflicts with")
		}
	})
}

// rebase.integrate: always keeps every target move a comparison change.
func TestGuardIntegrateAlwaysStillRewindsAPureTargetAdvance(t *testing.T) {
	c, selected := rewindFixture(t)
	c.Config.Rebase.Integrate = config.RebaseIntegrateAlways
	advanceRewindTarget(t, c)
	decision, err := guardPRContextWithSelection(c, types.StepTest, selected, false)
	if err != nil || decision.RestartFrom != types.StepRebase {
		t.Fatalf("rebase.integrate: always must rewind a moved target: %+v, %v", decision, err)
	}
}

// Push re-reads the live target before it publishes. A pure advance is not a
// reason to rewind; a conflicting one still is.
func TestPushTargetAdvanceDetectionRewindsOnlyOnConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance func(*testing.T, *pipeline.StepContext) string
		rewind  bool
	}{
		{"pure advance publishes", advanceRewindTarget, false},
		{"conflicting advance rewinds", conflictRewindTarget, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := publicationFixture(t)
			c.Config.Rebase.Integrate = ""
			tc.advance(t, c)
			rewind, err := detectTargetAdvanceAtPublication(c, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			if rewind != tc.rewind {
				t.Fatalf("rewind = %v, want %v", rewind, tc.rewind)
			}
		})
	}
}

// The pre-push attestation compares the live target with the receipt. A pure
// advance passes; a conflicting one is still the target-advance condition.
func TestAttestationComparisonHoldsOnlyAPureTargetAdvance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mainFile string
		wantErr  bool
	}{
		{"pure advance", "advance.md", false},
		{"conflicting advance", "feature.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, host, pr := mutationComparisonFixture(t)
			gitCmd(t, sctx.WorkDir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(sctx.WorkDir, tc.mainFile), []byte("target moved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, sctx.WorkDir, "add", tc.mainFile)
			gitCmd(t, sctx.WorkDir, "commit", "-m", "advance target")
			gitCmd(t, sctx.WorkDir, "checkout", "feature")
			err := verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA, false)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a pure target advance failed the attestation: %v", err)
				}
				return
			}
			if !errors.Is(err, errTargetComparisonAdvanced) {
				t.Fatalf("a conflicting target advance did not name the advance: %v", err)
			}
		})
	}
}

// #161: other merges land between a run's last comparison and its own, so the
// forge merges the reviewed head onto a newer target. The merge commit's first
// parent proves it, but only when merging the reviewed head onto that parent
// reproduces the merge commit's tree exactly.
func TestMergedOntoAdvancedTargetProvesOnlyTheReviewedChange(t *testing.T) {
	c, _ := rewindFixture(t)
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil || receipt == nil {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	advanced := advanceRewindTarget(t, c)
	squash := func(t *testing.T, parent, tree string) (string, string) {
		t.Helper()
		sha := gitCmd(t, c.WorkDir, "commit-tree", tree, "-p", parent, "-m", "squash")
		return sha, gitCmd(t, c.WorkDir, "rev-parse", sha+"^{tree}")
	}
	reviewedTree := gitCmd(t, c.WorkDir, "merge-tree", "--write-tree", advanced, receipt.LocalHeadSHA)

	t.Run("reviewed head on the advanced target", func(t *testing.T) {
		sha, tree := squash(t, advanced, reviewedTree)
		proven, err := mergedOntoAdvancedTarget(c, receipt, sha, tree)
		if err != nil || !proven {
			t.Fatalf("the reviewed change on an advanced target was not proven: %v %v", proven, err)
		}
	})
	t.Run("extra content in the merge commit", func(t *testing.T) {
		gitCmd(t, c.WorkDir, "checkout", "--detach", advanced)
		if err := os.WriteFile(filepath.Join(c.WorkDir, "smuggled.txt"), []byte("not reviewed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, c.WorkDir, "add", "smuggled.txt")
		gitCmd(t, c.WorkDir, "checkout", receipt.LocalHeadSHA, "--", "feature.txt")
		tree := gitCmd(t, c.WorkDir, "write-tree")
		gitCmd(t, c.WorkDir, "reset", "--hard", "-q")
		gitCmd(t, c.WorkDir, "checkout", "-q", "feature")
		sha, actual := squash(t, advanced, tree)
		if proven, err := mergedOntoAdvancedTarget(c, receipt, sha, actual); err != nil || proven {
			t.Fatalf("a merge commit carrying unreviewed content was proven: %v %v", proven, err)
		}
	})
	t.Run("parent outside the reviewed target's history", func(t *testing.T) {
		orphanTree := gitCmd(t, c.WorkDir, "rev-parse", advanced+"^{tree}")
		orphan := gitCmd(t, c.WorkDir, "commit-tree", orphanTree, "-m", "unrelated base")
		sha, actual := squash(t, orphan, reviewedTree)
		if proven, err := mergedOntoAdvancedTarget(c, receipt, sha, actual); err != nil || proven {
			t.Fatalf("a merge onto an unrelated base was proven: %v %v", proven, err)
		}
	})
	t.Run("last commit of a rebase merge", func(t *testing.T) {
		// Two replayed commits: the merge commit's parent is the first
		// replay, so its own change is only part of the reviewed change.
		gitCmd(t, c.WorkDir, "checkout", "-q", "feature")
		if err := os.WriteFile(filepath.Join(c.WorkDir, "second.txt"), []byte("second\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, c.WorkDir, "add", "second.txt")
		gitCmd(t, c.WorkDir, "commit", "-q", "-m", "second")
		head := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
		two := *receipt
		two.LocalHeadSHA = head
		gitCmd(t, c.WorkDir, "checkout", "-q", "--detach", advanced)
		gitCmd(t, c.WorkDir, "cherry-pick", head+"~1")
		replayed := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
		gitCmd(t, c.WorkDir, "cherry-pick", head)
		last := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
		gitCmd(t, c.WorkDir, "checkout", "-q", "feature")
		if replayed == last {
			t.Fatal("rebase merge fixture did not replay two commits")
		}
		tree := gitCmd(t, c.WorkDir, "rev-parse", last+"^{tree}")
		if proven, err := mergedOntoAdvancedTarget(c, &two, last, tree); err != nil || proven {
			t.Fatalf("a rebase merge's last commit was proven from its replayed parent: %v %v", proven, err)
		}
	})
	if strings.TrimSpace(reviewedTree) == "" {
		t.Fatal("empty reviewed tree")
	}
}
