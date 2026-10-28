//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is #224's receipt contract: only an accounted observation may
// change, and a forge-only refresh leaves the comparison and its evidence
// intact. These journeys use the real CLI, daemon, Git, and SQLite; only the
// agent and forge are stand-ins. Forge lag is independent of the Git source.
func TestForgeReceiptValidationLive(t *testing.T) {
	for _, kind := range []string{"review", "test", "document", "lint", "push", "pre-push", "pre-push-foreign", "foreign", "base", "source", "local", "new-rewind", "pending", "pending-foreign", "rewritten-target", "owner-claim"} {
		t.Run(kind, func(t *testing.T) {
			j := newForgeReceiptJourney(t, kind)
			h := j.h
			if kind == "pending" || kind == "pending-foreign" {
				j.start()
				parked := waitForStepStatus(t, h, j.branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
				before := j.receipt(parked.ID)
				pending, err := j.database.GetPendingTargetRewind(parked.ID)
				if err != nil || pending == nil || before.ForgeHeadSHA != j.forge {
					t.Fatalf("missing original pending receipt: %+v %+v %v", before, pending, err)
				}
				j.inject()
				h.Respond(parked.ID, types.StepRebase, types.ActionFix)
				if kind == "pending-foreign" {
					run := h.WaitForRun(j.branch, 90*time.Second)
					j.assertRefused(run, before, "unaccounted pull request head")
					j.assertRewindObservation(pending, j.forge)
					return
				}
				run := j.published()
				j.assertPublished(run)
				after := j.receipt(run.ID)
				j.assertRewindObservation(pending, j.head)
				if after.ForgeHeadSHA != j.head || after.TargetSHA != j.target {
					t.Fatalf("pending catch-up lost its observation: %+v", after)
				}
				if stored, _ := j.database.GetRun(run.ID); stored.TargetAdvanceRewinds != 1 || stored.PendingTargetRewindID != nil {
					t.Fatalf("pending refresh reclassified the rewind: %+v", stored)
				}
				t.Logf("pending catch-up accepted: generation=%d->%d target=%s forge=%s", before.Generation, after.Generation, after.TargetSHA, after.ForgeHeadSHA)
				return
			}
			j.start()
			if cause := map[string]string{"foreign": "unaccounted pull request head", "pre-push-foreign": "unaccounted pull request head", "base": "PR comparison changed", "source": "differs", "local": "PR comparison changed", "rewritten-target": "target"}[kind]; cause != "" {
				run := h.WaitForRun(j.branch, 120*time.Second)
				j.assertRefused(run, nil, cause)
				return
			}
			run := j.published()
			j.assertPublished(run)
			after := j.receipt(run.ID)
			if after.ForgeHeadSHA != j.head || after.LocalHeadSHA != run.HeadSHA {
				t.Fatalf("accounted observation was not bound: %+v", after)
			}
			if kind == "new-rewind" {
				stored, _ := j.database.GetRun(run.ID)
				if stored.TargetAdvanceRewinds != 1 || stored.PendingTargetRewindID != nil || after.TargetSHA != j.target {
					t.Fatalf("catch-up did not integrate exactly one rewind: %+v %+v", stored, after)
				}
			} else if after.Generation != 2 || after.TargetSHA != j.base || after.LocalHeadSHA != j.head {
				t.Fatalf("forge-only catch-up changed the comparison: %+v", after)
			}
			if kind == "owner-claim" {
				j.assertOwnerClaim(run.ID, after)
			}
			if kind == "pre-push" {
				body, err := os.ReadFile(j.script + ".attested")
				if err != nil {
					t.Fatal(err)
				}
				assertReceiptAttestation(t, string(body), j.head)
			}
			t.Logf("%s accepted: forge=%s local=%s target=%s generation=%d", kind, after.ForgeHeadSHA, after.LocalHeadSHA, after.TargetSHA, after.Generation)
		})
	}
}

func TestForgeReceiptReviewFixLive(t *testing.T) {
	j := newForgeReceiptJourney(t, "review-fix")
	j.start()
	run := j.published()
	j.assertPublished(run)
	receipt := j.receipt(run.ID)
	if run.HeadSHA != j.head || run.HeadSHA == j.forge || receipt.ForgeHeadSHA != run.HeadSHA || receipt.LocalHeadSHA != run.HeadSHA || receipt.Generation != 2 ||
		gitIn(t, j.h, j.h.UpstreamDir, "show", run.HeadSHA+":feature.txt") != "submitted feature" ||
		!isAncestorIn(t, j.h, j.h.UpstreamDir, j.forge, run.HeadSHA) {
		t.Fatalf("simultaneous valid Review fix and forge catch-up failed: %+v", receipt)
	}
	t.Logf("simultaneous Review fix and forge catch-up accepted at %s, generation=%d", run.HeadSHA, receipt.Generation)
}

type forgeReceiptJourney struct {
	t                                                       *testing.T
	h                                                       *Harness
	database                                                *db.DB
	branch, base, forge, head, target, script, snapshot, state string
	ownerBefore                                             *db.StepResult
}

func newForgeReceiptJourney(t *testing.T, kind string) *forgeReceiptJourney {
	t.Helper()
	scenario := cleanReviewScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "provider_plugins:\n  receipt:\n    command: nm-fake-provider-plugin\n    hosts: [receipt.example.test]\n    timeout: 30s\n"})
	j := &forgeReceiptJourney{t: t, h: h, branch: "feature/forge-receipt"}
	root := filepath.Dir(h.AgentLog)
	j.script, j.snapshot, j.state = filepath.Join(root, "inject.sh"), filepath.Join(root, "next.json"), filepath.Join(root, "forge.json")
	// State-only forge observations model eventual consistency without moving
	// the actual source ref during a pending integration.
	t.Setenv(fakeplugin.EnvUpstream, "")
	t.Setenv(fakeplugin.EnvState, j.state)
	t.Setenv(fakeplugin.EnvLog, filepath.Join(root, "provider.log"))
	const remote = "https://receipt.example.test/team/repo.git"
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	gitIn(t, h, h.WorkDir, "remote", "set-url", "origin", remote)
	command := "sh " + shellQuote(j.script)
	testCommand, lintCommand, formatCommand := "true", "true", "true"
	switch kind {
	case "lint":
		lintCommand = command
	case "push":
		formatCommand = command
	case "pre-push", "pre-push-foreign":
		formatCommand = "touch " + shellQuote(j.script+".armed")
	case "review", "document", "review-fix", "owner-claim":
	default:
		testCommand = command
	}
	commitTrustedRepoConfig(t, h, fmt.Sprintf("no_ci: true\ncommands:\n  test: %q\n  lint: %q\n  format: %q\nauto_fix:\n  review: 1\n  rebase: 0\n", testCommand, lintCommand, formatCommand))
	j.base = h.UpstreamBranchSHA("main")
	if strings.Contains(kind, "rewind") || strings.HasPrefix(kind, "pending") || kind == "rewritten-target" {
		if strings.HasPrefix(kind, "pending") {
			h.CommitChange("main", "feature.txt", "landed target\n", "prepare conflict")
		}
		j.target = h.CommitChange("main", "landed.txt", "landed target\n", "prepare target")
		if kind == "rewritten-target" {
			gitIn(t, h, h.WorkDir, "reset", "--hard", j.base+"^")
			j.target = h.CommitChange("main", "replacement.txt", "rewritten target\n", "rewrite target")
		}
		gitIn(t, h, h.WorkDir, "push", "origin", j.target+":refs/heads/prepared-target")
		gitIn(t, h, h.WorkDir, "reset", "--hard", j.base)
	}
	j.forge = h.CommitChange(j.branch, "feature.txt", "submitted feature\n", "published feature")
	gitIn(t, h, h.WorkDir, "push", "origin", j.branch)
	j.head = h.CommitChange(j.branch, "unpublished.txt", "local addition\n", "extend submitted head")
	// Keep all objects available to the disposable upstream while its source
	// ref stays fixed throughout the journey.
	gitIn(t, h, h.WorkDir, "push", "origin", j.head+":refs/heads/prepared-source")
	body, _ := steps.BuildPipelineSummary([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, nil, j.forge)
	state := fakeplugin.State{MaxPRBodyChars: 65536, PRs: []fakeplugin.PR{{Number: "42", URL: "https://receipt.example.test/team/repo/pulls/42", HeadBranch: j.branch, BaseBranch: "main", HeadSHA: j.forge, Title: "Receipt validation", Body: body, State: "open"}}}
	if err := fakeplugin.Save(j.state, state); err != nil {
		t.Fatal(err)
	}
	state.PRs[0].HeadSHA = j.head
	if kind == "foreign" || kind == "pending-foreign" || kind == "pre-push-foreign" {
		// The base is accounted ancestry; use a sibling instead.
		gitIn(t, h, h.WorkDir, "checkout", "-b", "foreign", j.base)
		gitIn(t, h, h.WorkDir, "commit", "--allow-empty", "-m", "foreign publisher")
		state.PRs[0].HeadSHA = gitIn(t, h, h.WorkDir, "rev-parse", "HEAD")
		h.Checkout(j.branch)
	}
	if kind == "base" {
		state.PRs[0].BaseBranch = "other"
		gitIn(t, h, h.UpstreamDir, "update-ref", "refs/heads/other", j.base)
	}
	if kind == "source" {
		state.PRs[0].HeadBranch = "other"
	}
	if err := fakeplugin.Save(j.snapshot, state); err != nil {
		t.Fatal(err)
	}
	injection := "#!/bin/sh\nset -eu\nif test -f \"$0.done\"; then exit 0; fi\n"
	if kind == "owner-claim" {
		injection += "touch \"$0.waiting\"\nn=0\nwhile ! test -f \"$0.release\"; do n=$((n+1)); test \"$n\" -lt 600; sleep 0.1; done\n"
	}
	if kind == "local" {
		injection += "git commit --allow-empty -m 'unaccounted local commit'\n"
	}
	if j.target != "" {
		injection += "git --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/main " + j.target + "\n"
	}
	if kind != "pending" && kind != "pending-foreign" {
		injection += "cp " + shellQuote(j.snapshot) + " " + shellQuote(j.state) + "\n"
	}
	injection += "touch \"$0.done\"\n"
	if err := os.WriteFile(j.script, []byte(injection), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	actions := "actions:\n"
	gitAction := fmt.Sprintf("    git:\n      - [-c, %q, inject]\n", "alias.inject=!"+command)
	if kind == "review" || kind == "document" || kind == "owner-claim" {
		match := "Fix-round provenance:"
		if kind == "document" || kind == "owner-claim" {
			match = "Keep the project documentation accurate for this change."
		}
		actions += fmt.Sprintf("  - match: %q\n%s    structured:\n      findings: []\n      summary: 'receipt changed'\n      risk_level: low\n      risk_rationale: 'fixture'\n      risk_scope: source-or-external\n", match, gitAction)
	}
	if strings.HasPrefix(kind, "pending") {
		actions += "  - match: 'Resolve git rebase conflicts.'\n    edits:\n      - path: feature.txt\n        new: \"submitted feature\\nlanded target\\n\"\n    git:\n      - [add, feature.txt]\n      - [-c, core.editor=true, rebase, --continue]\n    structured:\n      summary: 'resolved fixture conflict'\n"
	}
	if kind == "owner-claim" {
		actions += fmt.Sprintf("  - match: 'Fix-round provenance:'\n    structured:\n      findings:\n        - id: configured-test-claim\n          severity: error\n          file: unpublished.txt\n          line: 1\n          description: 'The configured test command fails'\n          action: ask-user\n          support:\n            claim_type: test\n            test:\n              command: %q\n      summary: 'Test must check this claim'\n      risk_level: low\n      risk_rationale: 'fixture claim'\n      risk_scope: source-or-external\n", testCommand)
	}
	fixGit := fmt.Sprintf("    git:\n      - [fetch, --no-tags, %q, %s]\n      - [reset, --hard, %s]\n", h.UpstreamDir, j.head, j.head)
	if strings.HasPrefix(kind, "pending") || kind == "new-rewind" {
		fixGit += fmt.Sprintf("      - [push, %q, HEAD:refs/heads/%s]\n", h.UpstreamDir, j.branch)
	}
	if kind == "review-fix" {
		fixGit += fmt.Sprintf("      - [-c, %q, inject]\n", "alias.inject=!"+command)
	}
	// Every journey first accepts an ordinary Review fix from F to A while
	// the PR is already attached. A later F -> A forge observation is then
	// strictly a receipt refresh, rather than initial PR attachment.
	if kind != "review" && kind != "owner-claim" {
		actions += "  - match: 'Fix-round provenance:'\n    structured:\n      findings: []\n      summary: 'fix verified'\n      risk_level: low\n      risk_rationale: 'fixture repaired'\n      risk_scope: source-or-external\n"
	}
	// Later fresh reviews after a rewind must not prescribe the setup fix
	// again. Only the initial review emits it.
	followup := filepath.Join(root, "post-fix.yaml")
	if err := os.WriteFile(followup, []byte(strings.Replace(string(old), "actions:\n", actions, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	fixGit += fmt.Sprintf("      - [-c, %q, next-scenario]\n", "alias.next-scenario=!cp "+shellQuote(followup)+" "+shellQuote(scenario))
	actions += fmt.Sprintf("  - match: 'Investigate previous review findings'\n%s    structured:\n      findings: []\n      summary: 'fixed'\n      verification: passed\n  - match: 'Review the code changes and return structured findings'\n    structured:\n      findings:\n        - id: fix-feature\n          severity: warning\n          file: feature.txt\n          line: 1\n          description: 'Add the local addition'\n          action: auto-fix\n          support:\n            claim_type: source\n            source: {path: feature.txt, line: 1, quote: 'submitted feature'}\n      summary: 'needs fix'\n      risk_level: low\n      risk_rationale: 'fixture repair'\n      risk_scope: source-or-external\n", fixGit)

	if err := os.WriteFile(scenario, []byte(strings.Replace(string(old), "actions:\n", actions, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if kind == "pre-push" || kind == "pre-push-foreign" {
		delegateDir := filepath.Join(root, "delegate")
		if err := os.MkdirAll(delegateDir, 0700); err != nil {
			t.Fatal(err)
		}
		delegate := filepath.Join(delegateDir, fakeplugin.ExecutableName)
		if err := os.Symlink(h.FakeAgent, delegate); err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(h.BinDir, fakeplugin.ExecutableName)
		if err := os.Remove(wrapper); err != nil {
			t.Fatal(err)
		}
		body := "#!/bin/sh\nset -eu\nif test \"${1-} ${2-}\" = 'pr view' && test -f " + shellQuote(j.script+".armed") + "; then " + command + "; fi\nexec " + shellQuote(delegate) + " \"$@\"\n"
		capture := "if test \"${1-} ${2-}\" = 'pr update' && ! test -f " + shellQuote(j.script+".attested") + "; then\n" +
			"  test \"$(git --git-dir=" + shellQuote(h.UpstreamDir) + " rev-parse refs/heads/" + j.branch + ")\" = " + shellQuote(j.forge) + " || exit 71\n" +
			"  for arg in \"$@\"; do case \"$arg\" in --body-file=*) cp \"${arg#--body-file=}\" " + shellQuote(j.script+".attested") + ";; esac; done\nfi\n"
		body = strings.Replace(body, "exec ", capture+"exec ", 1)
		if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return j
}

func (j *forgeReceiptJourney) start() {
	j.t.Helper()
	j.h.Checkout(j.branch)
	gitIn(j.t, j.h, j.h.WorkDir, "reset", "--hard", j.forge)
	initFromOwnWorktree(j.t, j.h, "init-forge-receipt")
	j.h.Checkout(j.branch)
	j.h.PushToGate(j.branch)
	var err error
	j.database, err = db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
	if err != nil {
		j.t.Fatal(err)
	}
	j.t.Cleanup(func() { _ = j.database.Close() })
}

func (j *forgeReceiptJourney) receipt(runID string) *db.PRContext {
	j.t.Helper()
	receipt, err := j.database.GetRunPRContext(runID)
	if err != nil || receipt == nil {
		j.t.Fatalf("read receipt: %+v %v", receipt, err)
	}
	return receipt
}

func (j *forgeReceiptJourney) inject() {
	j.t.Helper()
	data, err := os.ReadFile(j.snapshot)
	if err != nil {
		j.t.Fatal(err)
	}
	if err := os.WriteFile(j.state, data, 0600); err != nil {
		j.t.Fatal(err)
	}
}

func (j *forgeReceiptJourney) assertRefused(run *ipc.RunInfo, before *db.PRContext, cause string) {
	j.t.Helper()
	after := j.receipt(run.ID)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, cause) || after.ForgeHeadSHA != j.forge ||
		(before != nil && !reflect.DeepEqual(before, after)) {
		j.t.Fatalf("unsafe observation accepted: status=%s error=%s before=%+v after=%+v", run.Status, deref(run.Error), before, after)
	}
	stored, err := j.database.GetRun(run.ID)
	if err != nil || stored == nil || stored.LastPushedSHA != nil {
		j.t.Fatalf("refusal published: %+v %v", stored, err)
	}
	expectedRemote := j.forge
	if before != nil {
		expectedRemote = j.head
	} else if after.Generation != 2 || after.LocalHeadSHA != j.head || after.TargetSHA != j.base {
		j.t.Fatalf("refusal changed the comparison: %+v", after)
	}
	if got := j.h.UpstreamBranchSHA(j.branch); got != expectedRemote {
		j.t.Fatalf("refusal changed the source ref: %s, want %s", got, expectedRemote)
	}
	j.t.Logf("refused without accepting forge observation: %s; receipt=%+v", *run.Error, after)
}

func (j *forgeReceiptJourney) assertRewindObservation(before *db.TargetAdvanceRewind, forge string) {
	j.t.Helper()
	// The persisted rewind row survives completion. Check that only its
	// forge observation changed, including when integration consumes it.
	control := openControlDB(j.t, filepath.Join(j.h.NMHome, "state.sqlite"))
	var observed, source, local, target string
	var generation int64
	err := control.QueryRow(`SELECT forge_head_sha,source_sha,local_head_sha,target_sha,generation FROM run_target_rewinds WHERE id=?`, before.ID).Scan(&observed, &source, &local, &target, &generation)
	if err != nil || before.SourceSHA == nil || observed != forge || source != *before.SourceSHA || local != before.LocalHeadSHA || target != before.TargetSHA || generation != before.Generation {
		j.t.Fatalf("rewind observation was not atomic: forge=%s source=%s local=%s target=%s generation=%d before=%+v err=%v", observed, source, local, target, generation, before, err)
	}
}

func (j *forgeReceiptJourney) assertPublished(run *ipc.RunInfo) {
	j.t.Helper()
	for _, name := range []types.StepName{types.StepPush, types.StepPR} {
		step, ok := findStep(run.Steps, name)
		if !ok || step.Status != types.StepStatusCompleted {
			j.t.Fatalf("publication did not complete %s: %+v", name, step)
		}
	}
	published := j.h.UpstreamBranchSHA(j.branch)
	if published != run.HeadSHA || gitIn(j.t, j.h, j.h.UpstreamDir, "show", published+":unpublished.txt") != "local addition" {
		j.t.Fatal("publication lost the accounted local addition")
	}
	state, err := fakeplugin.Load(j.state)
	if err != nil || len(state.PRs) != 1 {
		j.t.Fatalf("read published PR: %+v %v", state, err)
	}
	assertReceiptAttestation(j.t, state.PRs[0].Body, published)
}

func assertReceiptAttestation(t *testing.T, body, head string) {
	t.Helper()
	const prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
	_, tail, ok := strings.Cut(body, prefix)
	payload, _, closed := strings.Cut(tail, " -->")
	var attestation struct {
		HeadSHA string `json:"head_sha"`
	}
	if !ok || !closed || json.Unmarshal([]byte(payload), &attestation) != nil || attestation.HeadSHA != head {
		t.Fatalf("missing exact-head public attestation for %s: %s", head, body)
	}
}

func (j *forgeReceiptJourney) published() *ipc.RunInfo {
	j.t.Helper()
	deadline := pollDeadline(j.t, 90*time.Second)
	for time.Now().Before(deadline) {
		for _, run := range j.h.Runs() {
			if run.Branch != j.branch {
				continue
			}
			if _, err := os.Stat(j.script + ".waiting"); err == nil && j.ownerBefore == nil {
				before := j.receipt(run.ID)
				rows, err := j.database.GetStepsByRun(run.ID)
				if err != nil {
					j.t.Fatal(err)
				}
				for _, row := range rows {
					if row.StepName == types.StepTest && row.Status == types.StepStatusCompleted {
						j.ownerBefore = row
					}
				}
				if j.ownerBefore == nil || before.Generation != 2 || before.ForgeHeadSHA != j.forge || before.LocalHeadSHA != j.head {
					j.t.Fatalf("missing pre-refresh validation: receipt=%+v Test=%+v", before, j.ownerBefore)
				}
				if err := os.WriteFile(j.script+".release", nil, 0600); err != nil {
					j.t.Fatal(err)
				}
			}
			push, pushed := findStep(run.Steps, types.StepPush)
			pr, attached := findStep(run.Steps, types.StepPR)
			if pushed && attached && push.Status == types.StepStatusCompleted && pr.Status == types.StepStatusCompleted {
				return &run
			}
			if run.Status.Terminal() {
				j.t.Fatalf("run ended before publication: %s %s", run.Status, deref(run.Error))
			}
			for _, step := range run.Steps {
				if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview {
					j.t.Fatalf("unexpected gate at %s: %s", step.StepName, deref(step.FindingsJSON))
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	j.t.Fatal("run never completed publication")
	return nil
}

func (j *forgeReceiptJourney) assertOwnerClaim(runID string, receipt *db.PRContext) {
	j.t.Helper()
	if j.ownerBefore == nil {
		j.t.Fatal("Test evidence was not captured before the forge refresh")
	}
	after, err := j.database.GetStepResult(j.ownerBefore.ID)
	if err != nil || !reflect.DeepEqual(j.ownerBefore, after) {
		j.t.Fatalf("forge-only refresh changed completed Test evidence: before=%+v after=%+v err=%v", j.ownerBefore, after, err)
	}
	rows, err := j.database.GetStepsByRun(runID)
	if err != nil {
		j.t.Fatal(err)
	}
	var claim, resolved types.Finding
	for _, row := range rows {
		if row.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*row.FindingsJSON)
		if err != nil {
			j.t.Fatal(err)
		}
		for _, item := range findings.Items {
			if row.StepName == types.StepReview && item.ID == "configured-test-claim" {
				claim = item
			}
			if row.StepName == types.StepTest && item.Category == types.FindingCategoryReviewSupportResolved {
				resolved = item
			}
		}
	}
	if claim.Support == nil || resolved.Support == nil || resolved.Support.OwnerResult == nil {
		j.t.Fatalf("missing owner evidence: claim=%+v resolved=%+v", claim, resolved)
	}
	owner := resolved.Support.OwnerResult
	if owner.ReviewFindingID != claim.ID || resolved.ID != types.ReviewSupportClaimID(claim) || owner.HeadSHA != receipt.LocalHeadSHA ||
		owner.TargetSHA != receipt.TargetSHA || owner.DiffDigest != receipt.DiffDigest || owner.Generation != receipt.Generation ||
		owner.Disposition != types.FindingSupportDispositionDisproven || owner.ExitCode == nil || *owner.ExitCode != 0 {
		j.t.Fatalf("forge refresh invalidated owner claim: claim=%+v result=%+v receipt=%+v", claim, resolved, receipt)
	}
	j.t.Logf("Test-owner evidence and claim identity remain valid: claim=%s generation=%d head=%s", resolved.ID, owner.Generation, owner.HeadSHA)
}
