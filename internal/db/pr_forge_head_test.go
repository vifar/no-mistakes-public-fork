package db

import (
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRefreshPRForgeHeadKeepsEvidenceAndRejectsStaleReceipt(t *testing.T) {
	d := openTestDB(t)
	id := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	candidate.PRURL = "https://github.com/acme/repo/pull/1"
	candidate.ForgeHeadSHA = strings.Repeat("e", 40)
	if _, err := d.BindRunPRContext(id, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	before, err := d.GetRunPRContext(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateRunReviewApprovedHeadSHA(id, candidate.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err = d.sql.Exec(`UPDATE run_pr_contexts SET ci_qualified=1 WHERE run_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = d.RefreshRunPRForgeHead(id, *before, candidate.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	after, err := d.GetRunPRContext(id)
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation || !after.CIQualified || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != candidate.LocalHeadSHA {
		t.Fatalf("authority lost: %+v %+v", after, run)
	}
	if err = d.RefreshRunPRForgeHead(id, *before, strings.Repeat("f", 40)); err == nil {
		t.Fatal("stale same-generation receipt replaced refreshed observation")
	}
	unchanged, _ := d.GetRunPRContext(id)
	if unchanged.PRContextCandidate != after.PRContextCandidate {
		t.Fatal("failed CAS wrote a receipt")
	}
}

func TestRefreshPRForgeHeadConcurrentWritersHaveOneWinner(t *testing.T) {
	d := openTestDB(t)
	id := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	candidate.PRURL = "https://github.com/acme/repo/pull/1"
	candidate.ForgeHeadSHA = strings.Repeat("e", 40)
	if _, err := d.BindRunPRContext(id, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	before, err := d.GetRunPRContext(id)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, head := range []string{strings.Repeat("f", 40), candidate.LocalHeadSHA} {
		wg.Add(1)
		go func(head string) { defer wg.Done(); <-start; errs <- d.RefreshRunPRForgeHead(id, *before, head) }(head)
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		}
	}
	after, err := d.GetRunPRContext(id)
	if err != nil {
		t.Fatal(err)
	}
	if success != 1 || after.Generation != before.Generation {
		t.Fatalf("success=%d receipt=%+v", success, after)
	}
}

func TestRefreshPRForgeHeadMatchesPendingIdentityInsteadOfCreationGeneration(t *testing.T) {
	for _, drift := range []string{"none", "local", "target", "forge", "unattached"} {
		t.Run(drift, func(t *testing.T) {
			d, id, candidate, req := targetRewindDBFixture(t)
			candidate.PRURL = "https://github.com/acme/repo/pull/1"
			candidate.ForgeHeadSHA = candidate.LocalHeadSHA
			if _, err := d.BindRunPRContext(id, candidate, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			previous, err := d.GetRunPRContext(id)
			if err != nil {
				t.Fatal(err)
			}
			candidate.TargetSHA = strings.Repeat("f", 40)
			req.ExpectedGeneration, req.HasPR = previous.Generation, true
			if _, err := d.RewindRunPRContext(id, candidate, req); err != nil {
				t.Fatal(err)
			}
			pending, err := d.GetPendingTargetRewind(id)
			if err != nil || pending == nil {
				t.Fatalf("pending=%+v %v", pending, err)
			}
			oldHead := candidate.LocalHeadSHA
			candidate.LocalHeadSHA = strings.Repeat("1", 40)
			if _, err := d.AdvanceRunPRContext(id, candidate); err != nil {
				t.Fatal(err)
			}
			if err := d.AdvancePendingTargetRewindHead(id, oldHead, candidate.LocalHeadSHA, true); err != nil {
				t.Fatal(err)
			}
			before, err := d.GetRunPRContext(id)
			if err != nil || before.Generation != pending.Generation+1 {
				t.Fatalf("receipt=%+v %v", before, err)
			}
			var mutationErr error
			switch drift {
			case "local":
				_, mutationErr = d.sql.Exec(`UPDATE run_target_rewinds SET local_head_sha=? WHERE id=?`, oldHead, pending.ID)
			case "target":
				_, mutationErr = d.sql.Exec(`UPDATE run_target_rewinds SET target_sha=? WHERE id=?`, previous.TargetSHA, pending.ID)
			case "forge":
				_, mutationErr = d.sql.Exec(`UPDATE run_target_rewinds SET forge_head_sha=? WHERE id=?`, strings.Repeat("2", 40), pending.ID)
			case "unattached":
				_, mutationErr = d.sql.Exec(`UPDATE run_target_rewinds SET forge_head_sha=NULL WHERE id=?`, pending.ID)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			err = d.RefreshRunPRForgeHead(id, *before, candidate.LocalHeadSHA)
			if (err != nil) != (drift != "none") {
				t.Fatalf("refresh with %s drift: %v", drift, err)
			}
			after, readErr := d.GetRunPRContext(id)
			if readErr != nil || after.Generation != before.Generation {
				t.Fatalf("receipt=%+v %v", after, readErr)
			}
			if drift != "none" {
				if after.PRContextCandidate != before.PRContextCandidate {
					t.Fatal("pending identity refusal did not roll back receipt update")
				}
				return
			}
			updated, readErr := d.GetPendingTargetRewind(id)
			if readErr != nil || updated == nil || updated.Generation != pending.Generation || updated.ForgeHeadSHA == nil || *updated.ForgeHeadSHA != candidate.LocalHeadSHA || after.ForgeHeadSHA != candidate.LocalHeadSHA {
				t.Fatalf("pending observation=%+v receipt=%+v %v", updated, after, readErr)
			}
		})
	}
}
