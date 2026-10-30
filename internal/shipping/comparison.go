// Package shipping owns strict shipping comparison and receipt validation.
package shipping

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func Digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

type remoteObservationError struct{ cause error }

func (*remoteObservationError) Error() string {
	return "observe shipping ref: remote observation failed"
}

func (e *remoteObservationError) Unwrap() error { return e.cause }

// AddsMeaning reports that this error says more than the cause it wraps: the
// remote observation did not produce a result. That is a failure in its own
// right, even when the cause is a command the cancellation stopped, so the
// pipeline records this run's own outcome rather than the cancellation's.
//
// The interface lives in the pipeline package, which owns the attribution.
func (*remoteObservationError) AddsMeaning() bool { return true }

func RemoteHead(ctx context.Context, dir, remote, branch string) (string, error) {
	out, err := git.RunNetwork(ctx, dir, "ls-remote", "--exit-code", remote, "refs/heads/"+branch)
	if err != nil {
		return "", &remoteObservationError{cause: err}
	}
	rows := strings.Fields(out)
	if len(rows) != 2 || rows[1] != "refs/heads/"+branch || len(rows[0]) != 40 {
		return "", fmt.Errorf("observe shipping ref: ambiguous ref")
	}
	return rows[0], nil
}

func Compare(ctx context.Context, dir, targetSHA, head string) (string, string, error) {
	base, err := git.Run(ctx, dir, "merge-base", targetSHA, head)
	if err != nil {
		return "", "", fmt.Errorf("resolve shipping merge base: %w", err)
	}
	raw, err := git.RunRaw(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", targetSHA+"..."+head)
	if err != nil {
		return "", "", fmt.Errorf("resolve shipping diff: %w", err)
	}
	return base, Digest([]byte(raw)), nil
}

// TargetIntegrated reports whether a strict full comparison has integrated its
// target: the target is the merge base, or - when keepClean (rebase.integrate:
// on-conflict) - the head merges into it without a conflict. A pull request
// merges from its own head, so a head that merges cleanly is shipped as it is
// rather than rebased (issue #237).
func TargetIntegrated(ctx context.Context, dir, targetSHA, mergeBase, head string, keepClean bool) (bool, error) {
	if mergeBase == targetSHA {
		return true, nil
	}
	if !keepClean {
		return false, nil
	}
	return git.MergesCleanly(ctx, dir, targetSHA, head)
}

// TargetAdvancedCleanly reports whether the live target tip is a pure advance
// of the approved comparison c: a fast-forward of c's target that leaves the
// approved head's merge base, and so its diff, unchanged, and that the head
// still merges cleanly into. Only full shipping integrates a moving target;
// an immutable source keeps its pinned target. The tip's objects are fetched
// when missing (issue #237).
func TargetAdvancedCleanly(ctx context.Context, dir, upstreamURL string, run *db.Run, c *db.ReviewComparison, tip string, keepClean bool) (bool, error) {
	if !keepClean || run == nil || run.Shipping == nil || run.Shipping.Mode != types.ShippingFull || c == nil || tip == "" || tip == c.TargetSHA {
		return false, nil
	}
	if _, err := git.Run(ctx, dir, "cat-file", "-e", tip+"^{commit}"); err != nil {
		if _, err := git.RunNetwork(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", upstreamURL, tip); err != nil {
			return false, fmt.Errorf("fetch advanced shipping target: %w", err)
		}
	}
	return git.TargetAdvancedCleanly(ctx, dir, c.TargetSHA, tip, c.HeadSHA, c.MergeBaseSHA)
}

func Capture(ctx context.Context, database *db.DB, repo *db.Repo, run *db.Run, dir, target string, keepClean bool) (*db.ReviewComparison, error) {
	return capture(ctx, database, repo, run, dir, target, keepClean, RemoteHead)
}

func capture(ctx context.Context, database *db.DB, repo *db.Repo, run *db.Run, dir, target string, keepClean bool, remoteHead func(context.Context, string, string, string) (string, error)) (*db.ReviewComparison, error) {
	upstreamURL := git.ResolveUpstreamURL(ctx, dir, repo.UpstreamURL, repo.URLsVerified)
	tip, err := remoteHead(ctx, dir, upstreamURL, target)
	if err != nil {
		return nil, err
	}
	if _, err := git.RunNetwork(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", upstreamURL, tip); err != nil {
		return nil, fmt.Errorf("fetch shipping comparison objects: %w", err)
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return nil, err
	}
	base, digest, err := Compare(ctx, dir, tip, head)
	if err != nil {
		return nil, err
	}
	comparison := db.ReviewComparison{SourceRepo: github.RepoSlug(repo.PushURL()), SourceBranch: strings.TrimPrefix(run.Branch, "refs/heads/"), TargetBranch: target, TargetSHA: tip, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest}
	after, err := remoteHead(ctx, dir, upstreamURL, target)
	if err != nil {
		return nil, err
	}
	if after != tip {
		advanced, err := TargetAdvancedCleanly(ctx, dir, upstreamURL, run, &comparison, after, keepClean)
		if err != nil {
			return nil, err
		}
		if !advanced {
			return nil, fmt.Errorf("shipping target moved during review capture")
		}
	}
	if run.Shipping.Mode == types.ShippingImmutableSource {
		if err := CheckImmutable(ctx, run, repo, dir); err != nil {
			return nil, err
		}
	}
	return database.CaptureReviewComparison(run.ID, comparison)
}

func CheckImmutableSource(ctx context.Context, run *db.Run, repo *db.Repo, dir string) error {
	if run == nil || run.Shipping == nil || run.Shipping.Mode != types.ShippingImmutableSource {
		return nil
	}
	if run.SubmittedHeadSHA == nil {
		return fmt.Errorf("immutable source lacks submitted head")
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return err
	}
	status, err := git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if head != *run.SubmittedHeadSHA || strings.TrimSpace(status) != "" {
		return fmt.Errorf("immutable source changed; source repairs and publication are refused")
	}
	pushURL := git.ResolvePushURL(ctx, dir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
	remote, err := RemoteHead(ctx, dir, pushURL, strings.TrimPrefix(run.Branch, "refs/heads/"))
	if err != nil {
		return err
	}
	if remote != head {
		return fmt.Errorf("immutable published source moved; start a new run")
	}
	return nil
}

func CheckImmutable(ctx context.Context, run *db.Run, repo *db.Repo, dir string) error {
	if err := CheckImmutableSource(ctx, run, repo, dir); err != nil {
		return err
	}
	return CheckImmutableTarget(ctx, run, repo, dir, RemoteHead)
}

func CheckImmutableTarget(ctx context.Context, run *db.Run, repo *db.Repo, dir string, remoteHead func(context.Context, string, string, string) (string, error)) error {
	if run == nil || run.Shipping == nil || run.Shipping.Mode != types.ShippingImmutableSource {
		return nil
	}
	if run.Shipping.TargetSHA != "" {
		upstreamURL := git.ResolveUpstreamURL(ctx, dir, repo.UpstreamURL, repo.URLsVerified)
		target, err := remoteHead(ctx, dir, upstreamURL, run.Shipping.TargetBranch)
		if err != nil {
			return err
		}
		if target != run.Shipping.TargetSHA {
			return fmt.Errorf("immutable target moved; start a new run")
		}
	}
	return nil
}
