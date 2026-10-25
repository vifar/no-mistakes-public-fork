package shipping

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func advanceExportTarget(t *testing.T, f *exportFixture, file string, publish bool) string {
	t.Helper()
	receiptGit(t, f.dir, "switch", "main")
	if err := os.WriteFile(filepath.Join(f.dir, file), []byte("target advance\n"), 0644); err != nil {
		t.Fatal(err)
	}
	receiptGit(t, f.dir, "add", file)
	receiptGit(t, f.dir, "commit", "-m", "advance target")
	tip := receiptGit(t, f.dir, "rev-parse", "HEAD")
	if publish {
		receiptGit(t, f.dir, "push", f.repo.UpstreamURL, "main")
	}
	receiptGit(t, f.dir, "switch", "feature")
	return tip
}

func TestExportIntegrationPolicy(t *testing.T) {
	const always = "rebase:\n  integrate: always\n"
	const onConflict = "rebase:\n  integrate: on-conflict\n"
	for _, kind := range []string{"trusted-always", "global-always", "trusted-on-conflict", "pushed-always", "pushed-on-conflict", "current-unintegrated"} {
		for _, observation := range []string{"forge", "remote", "both"} {
			t.Run(kind+"/"+observation, func(t *testing.T) {
				trusted, pushed := "", ""
				if kind == "trusted-always" || kind == "pushed-on-conflict" || kind == "current-unintegrated" {
					trusted = always
				}
				if kind == "trusted-on-conflict" {
					trusted = onConflict
				}
				if kind == "pushed-always" {
					pushed = always
				}
				if kind == "pushed-on-conflict" {
					pushed = onConflict
				}
				f := newExportFixtureWithPolicy(t, &types.ShippingOptions{Mode: types.ShippingFull}, trusted, pushed)
				if kind == "global-always" || kind == "trusted-on-conflict" {
					f.global.Rebase.Integrate = config.RebaseIntegrateAlways
				}
				if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge, f.global); err != nil {
					t.Fatalf("unchanged integrated comparison was refused: %v", err)
				}
				old, err := f.database.GetReviewComparison(f.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				tip := advanceExportTarget(t, f, "target-only", observation != "forge")
				if observation != "remote" {
					f.forge.facts.TargetSHA = tip
				}
				if kind == "current-unintegrated" {
					old.TargetSHA = tip
					captured, err := f.database.CaptureReviewComparison(f.run.ID, *old)
					if err != nil {
						t.Fatal(err)
					}
					rows, err := f.database.GetStepsByRun(f.run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.database.CompleteReviewStep(rows[2].ID, f.run.ID, f.run.HeadSHA, 0, 1, "", captured.Generation); err != nil {
						t.Fatal(err)
					}
					f.forge.facts.TargetSHA = tip
				}
				_, err = Export(context.Background(), f.database, f.repo, f.run.ID, "review", f.forge, f.global)
				want := kind == "trusted-on-conflict" || kind == "pushed-always"
				if (err == nil) != want {
					t.Fatalf("receipt error = %v, want accepted=%v", err, want)
				}
			})
		}
	}
}
