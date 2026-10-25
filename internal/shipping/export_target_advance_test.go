package shipping

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExportAfterTargetAdvance(t *testing.T) {
	for _, phase := range []string{"review", "ci-ready"} {
		for _, kind := range []string{"clean", "forge-conflict", "remote-conflict", "merge-base-moved", "identity", "immutable"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				mode := types.ShippingFull
				if kind == "immutable" {
					mode = types.ShippingImmutableSource
				}
				f := newExportFixtureWithShipping(t, &types.ShippingOptions{Mode: mode})
				before, err := f.database.GetReviewComparison(f.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				receiptGit(t, f.dir, "switch", "main")
				file := "target-only"
				if kind == "forge-conflict" || kind == "remote-conflict" {
					file = "source.txt"
				}
				if err := os.WriteFile(filepath.Join(f.dir, file), []byte("target advance\n"), 0644); err != nil {
					t.Fatal(err)
				}
				receiptGit(t, f.dir, "add", file)
				receiptGit(t, f.dir, "commit", "-m", "advance target")
				if kind == "merge-base-moved" {
					receiptGit(t, f.dir, "merge", "--no-ff", "feature", "-m", "land feature")
				}
				tip := receiptGit(t, f.dir, "rev-parse", "HEAD")
				if kind != "forge-conflict" {
					receiptGit(t, f.dir, "push", f.repo.UpstreamURL, "main")
				}
				receiptGit(t, f.dir, "switch", "feature")
				if kind != "remote-conflict" {
					f.forge.facts.TargetSHA = tip
				}
				if kind == "identity" {
					f.forge.facts.SourceBranch = "other"
				}
				f.forge.checks = []scm.Check{{Name: "approval", Bucket: scm.CheckBucketPass}}
				if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
					t.Fatal(err)
				}
				raw, err := Export(context.Background(), f.database, f.repo, f.run.ID, phase, f.forge)
				if kind != "clean" {
					if err == nil {
						t.Fatal("invalid target or identity accepted")
					}
					return
				}
				if err != nil {
					t.Fatalf("clean base advance blocked receipt: %v", err)
				}
				var p Payload
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
				if p.TargetSHA != before.TargetSHA || p.MergeBaseSHA != before.MergeBaseSHA || p.DiffDigest != before.DiffDigest || p.Generation != before.Generation || p.LocalHeadSHA != before.HeadSHA {
					t.Fatalf("receipt restamped the approved comparison: %+v", p)
				}
				after, err := f.database.GetReviewComparison(f.run.ID)
				if err != nil || *after != *before {
					t.Fatalf("export changed review authority: %+v %v", after, err)
				}
			})
		}
	}
}
