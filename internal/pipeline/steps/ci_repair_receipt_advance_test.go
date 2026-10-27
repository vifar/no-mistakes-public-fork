package steps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCIMonitorAdvancesReceiptForItsOwnPublishedRepair covers issue #87 as
// measured on controller runs 01M5TWW4 (#9270) and 01M5V01T (#9266):
//
//  1. a non-strict run's CI step repairs a red check and PUBLISHES the repair
//     (publishRunHead, ci.revalidate_repairs: false),
//  2. the checks re-run green on the repair head while the PR stays open,
//  3. the readiness check finds the receipt still naming the pre-repair head.
//
// Nothing in the monitor re-pinned the receipt, so every otherwise-ready poll
// reported "lacks a current PR comparison receipt" until the 10m budget failed
// the run with ci-readiness-receipt-missing. The durable publication record
// proves the advance was the run's own, exactly as it does on the terminal
// path (advanceReceiptForPipelineOwnedHead), so readiness must follow it - and
// every advance the record does not prove must still withhold readiness.
func TestCIMonitorAdvancesReceiptForItsOwnPublishedRepair(t *testing.T) {
	for _, tc := range []struct {
		name          string
		publish       bool
		stalePublish  bool
		forgeLags     bool
		foreignHead   bool
		divergentHead bool
		retarget      bool
		wantReady     bool
	}{
		{name: "published repair with the forge on the repair head", publish: true, wantReady: true},
		{name: "repair never published", publish: false},
		{name: "only an older publication record", publish: true, stalePublish: true},
		{name: "forge has not reported the repair head yet", publish: true, forgeLags: true},
		{name: "another publisher moved the forge head", publish: true, foreignHead: true},
		{name: "published head on a different lineage", publish: true, divergentHead: true},
		{name: "PR retargeted to another branch", publish: true, retarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			if sctx.Run.Shipping != nil {
				t.Fatal("fixture must be a non-strict run")
			}
			prURL := "https://github.com/test/repo/pull/42"
			selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
				t.Fatal(err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			sctx.PRContext = receipt
			if tc.wantReady {
				// Review approved the pre-repair head with a pending CI claim.
				// Readiness on the repair head must not let the repair settle it.
				review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{pendingReviewCIClaim("build-42", head)}})
				if err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.SetStepFindings(review.ID, encoded); err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.CompleteReviewStep(review.ID, sctx.Run.ID, head, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			sctx.Run.PRURL = &prURL

			// The CI step commits its repair on top of the receipt's head.
			if tc.divergentHead {
				gitCmd(t, dir, "reset", "--hard", base)
			}
			if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("fixed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "repair.txt")
			gitCmd(t, dir, "commit", "-m", "no-mistakes(ci): repair")
			repairHead := gitCmd(t, dir, "rev-parse", "HEAD")

			// publishRunHead records the run head and the publication binding.
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, repairHead); err != nil {
				t.Fatal(err)
			}
			if tc.publish {
				publishedHead := repairHead
				if tc.stalePublish {
					publishedHead = receipt.LocalHeadSHA
				}
				if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
					HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
				}); err != nil {
					t.Fatal(err)
				}
				if tc.stalePublish {
					if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, repairHead); err != nil {
						t.Fatal(err)
					}
				}
			}
			sctx.Run.HeadSHA = repairHead

			forgeHead := repairHead
			switch {
			case tc.forgeLags:
				forgeHead = receipt.LocalHeadSHA
			case tc.foreignHead:
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "another publisher")
				forgeHead = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "reset", "--hard", repairHead)
			}
			targetBranch := "main"
			if tc.retarget {
				targetBranch = "release"
				gitCmd(t, dir, "branch", targetBranch, "main")
			}
			facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"open","merged":false,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":%q}}`, prURL, forgeHead, targetBranch)
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE":            "ci-gh-reconcile",
				"FAKE_CLI_PR_FACTS_JSON":   facts,
				"FAKE_CLI_PROTECTION_JSON": `{"strict":false}`,
				"FAKE_CLI_RULES_JSON":      `[]`,
			})
			host, reason := buildHost(sctx, resolvedProvider(sctx))
			if host == nil {
				t.Fatalf("build host: %s", reason)
			}
			checks := []scm.Check{{Name: "build", ProviderID: "build-42", HeadSHA: forgeHead, Bucket: scm.CheckBucketPass, State: "SUCCESS"}}

			_, err = logVerifiedCIMonitorStatus(sctx, host, checks, ciChecksPassedMsg, "")
			after, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !tc.wantReady {
				if err == nil {
					t.Fatalf("readiness accepted an unproven head advance; receipt %+v", after)
				}
				if after.PRContextCandidate != receipt.PRContextCandidate {
					t.Fatalf("unproven advance replaced the receipt:\nbefore %+v\nafter  %+v", receipt, after)
				}
				if sctx.PRContext != receipt {
					t.Fatal("unproven advance replaced the active step's comparison")
				}
				return
			}
			if err != nil {
				t.Fatalf("readiness refused the run's own published CI repair: %v", err)
			}
			// The proof serves readiness only. The stored receipt keeps naming
			// the reviewed head, so step boundaries still see the repair as a
			// head advance and handle it exactly as before.
			if after.PRContextCandidate != receipt.PRContextCandidate || after.Generation != receipt.Generation {
				t.Fatalf("readiness persisted the advance:\nbefore %+v\nafter  %+v", receipt, after)
			}
			proven, err := pipelineOwnedHeadComparison(sctx, after, mustGetRun(t, sctx), pipeline.PRTargetSelection{
				PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: repairHead, TargetBranch: "main"})
			if err != nil || proven == nil {
				t.Fatalf("published repair not proven: %+v, %v", proven, err)
			}
			if proven.LocalHeadSHA != repairHead || proven.ForgeHeadSHA != repairHead || proven.PRURL != receipt.PRURL ||
				proven.TargetBranch != receipt.TargetBranch || proven.TargetSHA != receipt.TargetSHA || proven.MergeBaseSHA != receipt.MergeBaseSHA ||
				proven.DiffDigest == receipt.DiffDigest {
				t.Fatalf("proven comparison does not describe the published repair at the same target:\nreceipt %+v\nproven  %+v", receipt, proven)
			}
			// The oracle is the Review support contract: Review approved the
			// pre-repair head, so a green check on the repair head must not
			// resolve that claim. The repaired code returns to Review, even
			// though readiness accepted the published repair.
			sctx.Fixing = true
			ownerHost := &supportCheckHost{Host: host, facts: scm.PRFacts{
				PR: scm.PR{URL: prURL}, State: scm.PRStateOpen, SourceRepository: "test/repo",
				SourceBranch: "feature", HeadSHA: repairHead, BaseBranch: "main",
			}, checks: checks}
			results, err := resolveCIReviewSupport(sctx, ownerHost, &scm.PR{URL: prURL, Number: "42"})
			if !errors.Is(err, errReviewSupportHeadAdvanced) || len(results) != 0 {
				t.Fatalf("a published repair resolved the Review claim of the code it replaced instead of restarting Review: %+v, %v", results, err)
			}
		})
	}
}

func mustGetRun(t *testing.T, sctx *pipeline.StepContext) *db.Run {
	t.Helper()
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run == nil {
		t.Fatalf("read run: %+v, %v", run, err)
	}
	return run
}
