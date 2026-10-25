package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mergeFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mergeFixtureCommit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureGit(t, dir, "add", "-A")
	mergeFixtureGit(t, dir, "commit", "-q", "-m", file)
	return mergeFixtureGit(t, dir, "rev-parse", "HEAD")
}

// TestMergesCleanlyAndTargetAdvancedCleanly pins the two answers issue #237
// rests on, and that a command that cannot run is an error, never a verdict.
func TestMergesCleanlyAndTargetAdvancedCleanly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	mergeFixtureGit(t, dir, "init", "-q", "-b", "main")
	base := mergeFixtureCommit(t, dir, "shared.txt", "base\n")
	mergeFixtureGit(t, dir, "checkout", "-q", "-b", "feature")
	head := mergeFixtureCommit(t, dir, "feature.txt", "feature\n")
	mergeFixtureGit(t, dir, "checkout", "-q", "main")
	clean := mergeFixtureCommit(t, dir, "other.txt", "other\n")
	conflict := mergeFixtureCommit(t, dir, "feature.txt", "main\n")

	if ok, err := MergesCleanly(ctx, dir, clean, head); err != nil || !ok {
		t.Fatalf("clean merge = %v, %v", ok, err)
	}
	if ok, err := MergesCleanly(ctx, dir, conflict, head); err != nil || ok {
		t.Fatalf("conflicting merge = %v, %v", ok, err)
	}
	if _, err := MergesCleanly(ctx, dir, strings.Repeat("0", 39)+"1", head); err == nil {
		t.Fatal("an unresolvable commit must be an error, not a verdict")
	}

	if ok, err := TargetAdvancedCleanly(ctx, dir, base, clean, head, base); err != nil || !ok {
		t.Fatalf("pure advance = %v, %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, dir, base, conflict, head, base); err != nil || ok {
		t.Fatalf("conflicting advance = %v, %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, dir, clean, base, head, base); err != nil || ok {
		t.Fatalf("a target that moved backwards = %v, %v", ok, err)
	}
	if ok, err := TargetAdvancedCleanly(ctx, dir, base, base, head, base); err != nil || ok {
		t.Fatalf("an unmoved target is not an advance: %v, %v", ok, err)
	}
	// The target took the head's own commit, so the merge base moved and the
	// remaining diff changed: not a pure advance.
	mergeFixtureGit(t, dir, "checkout", "-q", "-b", "landed", base)
	mergeFixtureGit(t, dir, "merge", "-q", "--no-edit", head)
	landed := mergeFixtureCommit(t, dir, "later.txt", "later\n")
	if ok, err := TargetAdvancedCleanly(ctx, dir, base, landed, head, base); err != nil || ok {
		t.Fatalf("an advance that moved the merge base = %v, %v", ok, err)
	}
}
