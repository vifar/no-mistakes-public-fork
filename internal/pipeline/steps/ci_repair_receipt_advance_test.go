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
			checks := []scm.Check{{Name: "build", HeadSHA: forgeHead, Bucket: scm.CheckBucketPass, State: "SUCCESS"}}

			err = verifyCIMonitorComparison(sctx, host, checks)
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
				return
			}
			if err != nil {
				t.Fatalf("readiness refused the run's own published CI repair: %v", err)
			}
			if errors.Is(err, errCIReadinessReceiptMissing) {
				t.Fatal("readiness still reports a missing receipt")
			}
			if after.LocalHeadSHA != repairHead || after.ForgeHeadSHA != repairHead {
				t.Fatalf("receipt heads = local %s forge %s, want the published repair %s", after.LocalHeadSHA, after.ForgeHeadSHA, repairHead)
			}
			if after.PRURL != receipt.PRURL || after.SourceRepo != receipt.SourceRepo || after.SourceBranch != receipt.SourceBranch ||
				after.TargetBranch != receipt.TargetBranch || after.TargetSHA != receipt.TargetSHA || after.MergeBaseSHA != receipt.MergeBaseSHA {
				t.Fatalf("advance changed the PR identity or target comparison:\nbefore %+v\nafter  %+v", receipt, after)
			}
			if after.DiffDigest == receipt.DiffDigest {
				t.Fatal("advance kept the pre-repair diff digest")
			}
		})
	}
}
