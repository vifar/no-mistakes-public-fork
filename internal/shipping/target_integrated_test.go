package shipping

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTargetIntegratedAndAdvancedCleanly pins strict full shipping's two
// #237 rules and that immutable-source and rebase.integrate: always keep the
// pinned, up-to-date behaviour.
func TestTargetIntegratedAndAdvancedCleanly(t *testing.T) {
	f := newExportFixture(t)
	ctx := context.Background()
	reviewed, err := f.database.GetReviewComparison(f.run.ID)
	if err != nil || reviewed == nil {
		t.Fatalf("comparison: %+v %v", reviewed, err)
	}
	advance := func(file string) string {
		receiptGit(t, f.dir, "switch", "main")
		if err := os.WriteFile(filepath.Join(f.dir, file), []byte("advance "+file+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		receiptGit(t, f.dir, "add", file)
		receiptGit(t, f.dir, "commit", "-m", "advance "+file)
		sha := receiptGit(t, f.dir, "rev-parse", "HEAD")
		receiptGit(t, f.dir, "switch", "feature")
		return sha
	}
	clean := advance("target-only")
	conflict := advance("source.txt")
	head := f.run.HeadSHA

	for _, tc := range []struct {
		name              string
		target, mergeBase string
		keepClean, want   bool
	}{
		{"contains target", reviewed.TargetSHA, reviewed.TargetSHA, false, true},
		{"clean unintegrated", clean, reviewed.TargetSHA, true, true},
		{"clean under integrate always", clean, reviewed.TargetSHA, false, false},
		{"conflicting", conflict, reviewed.TargetSHA, true, false},
	} {
		got, err := TargetIntegrated(ctx, f.dir, tc.target, tc.mergeBase, head, tc.keepClean)
		if err != nil || got != tc.want {
			t.Fatalf("%s: TargetIntegrated = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}

	full := &db.Run{Shipping: &types.ShippingOptions{Mode: types.ShippingFull}}
	immutable := &db.Run{Shipping: &types.ShippingOptions{Mode: types.ShippingImmutableSource}}
	if ok, err := TargetAdvancedCleanly(ctx, f.dir, f.repo.UpstreamURL, full, reviewed, receiptGit(t, f.dir, "rev-parse", clean), true); err != nil || !ok {
		t.Fatalf("a pure advance did not keep the approved comparison: %v %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, f.dir, f.repo.UpstreamURL, full, reviewed, conflict, true); err != nil || ok {
		t.Fatalf("a conflicting advance kept the approved comparison: %v %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, f.dir, f.repo.UpstreamURL, full, reviewed, clean, false); err != nil || ok {
		t.Fatalf("integrate always accepted a moved target: %v %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, f.dir, f.repo.UpstreamURL, immutable, reviewed, clean, true); err != nil || ok {
		t.Fatalf("an immutable source accepted a moved target: %v %v", ok, err)
	}
}
