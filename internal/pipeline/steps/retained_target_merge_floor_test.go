package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAdvancedMergeProofPreservesLatestRetainedFloor(t *testing.T) {
	for _, kind := range []string{"consecutive", "rollback", "sibling", "older-retained-sibling", "moved-merge-base", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			c, selection, receipt := retainedTargetFixture(t)
			gitCmd(t, c.WorkDir, "checkout", "main")
			gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "first retained target")
			first := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			gitCmd(t, c.WorkDir, "checkout", "feature")
			if _, err := guardPRContextWithSelection(c, types.StepCI, selection, false); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, c.WorkDir, "checkout", "main")
			gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "latest retained target")
			floor := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
			gitCmd(t, c.WorkDir, "checkout", "feature")
			if _, err := guardPRContextWithSelection(c, types.StepCI, selection, false); err != nil {
				t.Fatal(err)
			}
			if got, err := c.DB.GetRetainedPRTarget(c.Run.ID, receipt); err != nil || got != floor {
				t.Fatalf("latest retained floor = %q, want %q: %v", got, floor, err)
			}
			c.Config.Rebase.Integrate = config.RebaseIntegrateOnConflict
			targetTree := gitCmd(t, c.WorkDir, "rev-parse", floor+"^{tree}")
			parent := gitCmd(t, c.WorkDir, "commit-tree", targetTree, "-p", floor, "-m", "pure advance")
			switch kind {
			case "rollback":
				parent = receipt.TargetSHA
			case "sibling":
				parent = gitCmd(t, c.WorkDir, "commit-tree", targetTree, "-p", receipt.TargetSHA, "-m", "sibling of retained targets")
			case "older-retained-sibling":
				parent = gitCmd(t, c.WorkDir, "commit-tree", targetTree, "-p", first, "-m", "sibling of latest retained target")
			case "conflict":
				gitCmd(t, c.WorkDir, "checkout", "--detach", floor)
				if err := os.WriteFile(filepath.Join(c.WorkDir, "feature.txt"), []byte("conflicting target change\n"), 0644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", "feature.txt")
				gitCmd(t, c.WorkDir, "commit", "-m", "conflicting target advance")
				parent = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "checkout", "feature")
			case "moved-merge-base":
				headTree := gitCmd(t, c.WorkDir, "rev-parse", receipt.LocalHeadSHA+"^{tree}")
				parent = gitCmd(t, c.WorkDir, "commit-tree", headTree, "-p", floor, "-p", receipt.LocalHeadSHA, "-m", "already landed head")
			}
			tree := gitCmd(t, c.WorkDir, "rev-parse", receipt.LocalHeadSHA+"^{tree}")
			if kind != "conflict" {
				tree = gitCmd(t, c.WorkDir, "merge-tree", "--write-tree", parent, receipt.LocalHeadSHA)
			}
			merged := gitCmd(t, c.WorkDir, "commit-tree", tree, "-p", parent, "-m", "squash reviewed change")
			proven, err := mergedOntoAdvancedTarget(c, receipt, merged, tree)
			want := kind == "consecutive"
			if err != nil || proven != want {
				t.Fatalf("first-parent proof = %v, %v; want %v above latest retained floor", proven, err, want)
			}
			if got, err := c.DB.GetRetainedPRTarget(c.Run.ID, receipt); err != nil || got != floor {
				t.Fatalf("terminal proof changed retained floor: %q, %v", got, err)
			}
			if after, err := c.DB.GetRunPRContext(c.Run.ID); err != nil || *after != *receipt {
				t.Fatalf("terminal proof changed comparison: %+v, %v", after, err)
			}
		})
	}
}
