package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiPreservedRebaseOffersExecutableRecovery(t *testing.T) {
	f := newCLIRecoverFixture(t)
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	cliGit(t, pipeline, "checkout", "-b", "new-base", f.base)
	if err := os.WriteFile(filepath.Join(pipeline, "base-only.txt"), []byte("new base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "add", ".")
	cliGit(t, pipeline, "commit", "-m", "new base")
	base := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "checkout", "feature/recover")
	cliGit(t, pipeline, "rebase", "--onto", base, f.base)
	preserved := cliGit(t, pipeline, "rev-parse", "HEAD")
	anchor := "refs/no-mistakes/recover/" + f.runID
	cliGit(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover", "HEAD:"+anchor)
	cliGit(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
	cliGit(t, f.local, "push", f.remote, "HEAD:refs/heads/feature/recover")
	p, _ := paths.New()
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(f.runID, db.PushBinding{HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(f.runID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(f.runID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	database.Close()
	if err := os.WriteFile(filepath.Join(f.local, "local-only.txt"), []byte("local\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, f.local, "add", ".")
	cliGit(t, f.local, "commit", "-m", "local work")
	local := cliGit(t, f.local, "rev-parse", "HEAD")
	t.Logf("R=%s L=%s P=%s ref=%s", f.submitted, local, preserved, anchor)
	command := "no-mistakes axi sync --recover --keep-local --authoritative-head " + local
	for _, args := range [][]string{{"axi", "status"}, {"axi", "status", "--json"}, {"axi", "sync", "--check"}} {
		out, _ := executeCmd(args...)
		t.Logf("%v:\n%s", args, out)
		if !strings.Contains(out, command) || !strings.Contains(out, preserved) || !strings.Contains(out, anchor) {
			t.Fatalf("missing recovery evidence/command:\n%s", out)
		}
	}
	if out, err := executeCmd("axi", "sync", "--recover"); err == nil {
		t.Fatalf("plain recovery accepted:\n%s", out)
	}
	// Execute exactly the offered command through the real command handler.
	out, err := executeCmd(strings.Fields(command)[1:]...)
	t.Logf("%s:\n%s", command, out)
	if err != nil {
		t.Fatalf("offered command failed: %v\n%s", err, out)
	}
	for _, dir := range []string{f.local, f.gate} {
		if cliGit(t, dir, "rev-parse", anchor) != preserved || cliGit(t, dir, "rev-parse", "refs/heads/feature/recover") != local {
			t.Fatal("recovery lost history")
		}
	}
	database, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(f.runID)
	if err != nil || run.CustodyReturnedAt == nil {
		t.Fatal("custody not returned")
	}
	t.Logf("readback: L=%s on both branches, P=%s pinned by %s in both repos; custody returned", local, preserved, anchor)
}
