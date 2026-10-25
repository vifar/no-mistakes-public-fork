package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShippingMergedProofAfterTargetAdvance(t *testing.T) {
	for _, kind := range []string{"unchanged", "clean", "conflict", "merge-base-moved", "identity", "head", "always", "immutable"} {
		t.Run(kind, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			mode := types.ShippingFull
			if kind == "immutable" {
				mode = types.ShippingImmutableSource
			}
			run, err := c.DB.InsertRunWithShipping(c.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			c.Run = run
			comparison, err := c.DB.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base, HeadSHA: head, MergeBaseSHA: base, DiffDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
			if err != nil {
				t.Fatal(err)
			}
			sr, err := c.DB.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DB.CompleteReviewStep(sr.ID, run.ID, head, 0, 1, "", comparison.Generation); err != nil {
				t.Fatal(err)
			}
			c.Repo.UpstreamURL = "https://github.com/test/repo.git"
			tip := base
			if kind != "unchanged" {
				gitCmd(t, dir, "switch", "main")
				file := "target-only"
				if kind == "conflict" {
					file = "feature.txt"
				}
				if err := os.WriteFile(filepath.Join(dir, file), []byte("target advance\n"), 0644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", file)
				gitCmd(t, dir, "commit", "-m", "advance target")
				tip = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "switch", "feature")
			}
			if kind == "merge-base-moved" {
				tip = head
			}
			if kind == "always" {
				c.Config.Rebase.Integrate = config.RebaseIntegrateAlways
			}
			url := "https://github.com/test/repo/pull/7"
			facts := scm.PRFacts{State: scm.PRStateMerged, URL: url, SourceRepo: "test/repo", SourceBranch: "feature", HeadSHA: head, TargetRepo: "test/repo", TargetBranch: "main", TargetSHA: tip}
			if kind == "identity" {
				facts.SourceBranch = "other"
			}
			if kind == "head" {
				facts.HeadSHA = tip
			}
			err = verifyShippingMergedProof(c, &mergedFactsHost{facts: facts}, &scm.PR{URL: url})
			want := kind == "clean" || kind == "unchanged"
			if (err == nil) != want {
				t.Fatalf("merge proof error = %v; want accepted=%v", err, want)
			}
			after, err := c.DB.GetReviewComparison(run.ID)
			if err != nil || after.TargetSHA != base || after.Generation != comparison.Generation {
				t.Fatalf("merge proof changed approved comparison: %+v %v", after, err)
			}
		})
	}
}
