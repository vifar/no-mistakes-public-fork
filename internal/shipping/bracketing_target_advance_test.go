package shipping

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExportBracketingTargetAdvance(t *testing.T) {
	for _, kind := range []string{"clean", "clean-forge", "clean-remote", "conflict", "merge-base-moved", "identity", "head", "draft", "rollback", "always", "immutable"} {
		for _, phase := range []string{"review", "ci-ready"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				mode := types.ShippingFull
				if kind == "immutable" {
					mode = types.ShippingImmutableSource
				}
				trusted := ""
				if kind == "always" {
					trusted = "rebase:\n  integrate: always\n"
				}
				f := newExportFixtureWithPolicy(t, &types.ShippingOptions{Mode: mode}, trusted, "")
				before, err := f.database.GetReviewComparison(f.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				file := "target-only"
				if kind == "conflict" {
					file = "source.txt"
				}
				tip := advanceExportTarget(t, f, file, false)
				if kind == "merge-base-moved" {
					tip = f.run.HeadSHA
				}
				if kind == "rollback" {
					f.forge.facts.TargetSHA = tip
				}
				f.forge.checks = []scm.Check{{Name: "approval", Bucket: scm.CheckBucketPass}}
				if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
					t.Fatal(err)
				}
				reads := 0
				f.forge.observe = func() {
					reads++
					if reads != 2 {
						return
					}
					switch kind {
					case "rollback":
						f.forge.facts.TargetSHA = before.TargetSHA
					case "identity":
						f.forge.facts.SourceBranch = "other"
					case "head":
						f.forge.facts.HeadSHA = tip
					case "clean-remote":
					case "draft":
						f.forge.facts.Draft = !f.forge.facts.Draft
					default:
						f.forge.facts.TargetSHA = tip
					}
					if kind == "clean" || kind == "clean-remote" {
						receiptGit(t, f.dir, "push", f.repo.UpstreamURL, "main")
					}
				}
				raw, err := Export(context.Background(), f.database, f.repo, f.run.ID, phase, f.forge, f.global)
				if kind != "clean" && kind != "clean-forge" && kind != "clean-remote" {
					if err == nil {
						t.Fatal("invalid bracketing observation accepted")
					}
					return
				}
				if err != nil {
					t.Fatalf("clean advance during export failed: %v", err)
				}
				var p Payload
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
				if p.TargetSHA != before.TargetSHA || p.MergeBaseSHA != before.MergeBaseSHA || p.DiffDigest != before.DiffDigest || p.LocalHeadSHA != before.HeadSHA || p.Generation != before.Generation {
					t.Fatalf("receipt restamped the approved change: %+v", p)
				}
			})
		}
	}
}

func TestCaptureBracketingTargetAdvance(t *testing.T) {
	for _, kind := range []string{"unchanged", "clean", "conflict", "merge-base-moved", "always", "immutable", "rollback"} {
		t.Run(kind, func(t *testing.T) {
			mode := types.ShippingFull
			if kind == "immutable" {
				mode = types.ShippingImmutableSource
			}
			f := newExportFixtureWithShipping(t, &types.ShippingOptions{Mode: mode})
			before, err := f.database.GetReviewComparison(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			file := "target-only"
			if kind == "conflict" {
				file = "source.txt"
			}
			tip := advanceExportTarget(t, f, file, false)
			reads := 0
			read := func(ctx context.Context, dir, remote, branch string) (string, error) {
				reads++
				if reads == 1 && kind == "rollback" {
					return tip, nil
				}
				if reads == 2 {
					if kind == "merge-base-moved" {
						return f.run.HeadSHA, nil
					}
					if kind != "unchanged" && kind != "rollback" {
						receiptGit(t, f.dir, "push", f.repo.UpstreamURL, "main")
					}
				}
				return RemoteHead(ctx, dir, remote, branch)
			}
			got, err := capture(context.Background(), f.database, f.repo, f.run, f.dir, "main", kind != "always", read)
			want := kind == "clean" || kind == "unchanged"
			if (err == nil) != want {
				t.Fatalf("capture error = %v, want accepted=%v", err, want)
			}
			if want && (got.TargetSHA != before.TargetSHA || got.HeadSHA != before.HeadSHA || got.MergeBaseSHA != before.MergeBaseSHA || got.DiffDigest != before.DiffDigest) {
				t.Fatalf("capture moved the comparison: %+v", got)
			}
			if !want {
				after, err := f.database.GetReviewComparison(f.run.ID)
				if err != nil || *after != *before {
					t.Fatalf("failed capture changed authority: %+v %v", after, err)
				}
			}
		})
	}
}
