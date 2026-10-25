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
