package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testCommit = "0123456789abcdef0123456789abcdef01234567"
	testConfig = "cfg-fingerprint"
)

// passingReport is a sealed report whose every trial passed, for a tier.
func passingReport(tier string) Report {
	w := Workloads[tier]
	r := Report{Version: ReportVersion, Tier: tier, BudgetUSD: 100,
		Fingerprint: Fingerprint{Commit: testCommit, ConfigFingerprint: testConfig},
		Flags:       RunFlags{RequireModels: true, RequireDependencies: true, NoDotenv: true}}
	for i := 0; i < w.Total(); i++ {
		r.Trials = append(r.Trials, TrialResult{ID: fmt.Sprintf("fr_v1/c%03d#1", i), Status: StatusPassed,
			OutputSHA256: fmt.Sprintf("%064d", i), Outcome: &Outcome{Outcome: "completed", LatencyMS: int64(i)}})
	}
	r.Aggregate()
	r.Seal()
	return r
}

func approvingReview(r Report) Review {
	rv := Review{Version: ReviewVersion, Reviewer: Reviewer{Name: "Me Exemple", Role: "avocat", Bar: "Barreau d'Amiens"},
		ReviewedAt: time.Now().UTC().Format(time.RFC3339), Commit: r.Fingerprint.Commit,
		ConfigFingerprint: r.Fingerprint.ConfigFingerprint, ReportSHA256: r.ReportSHA256}
	for _, t := range r.Trials {
		rv.Outputs = append(rv.Outputs, ReviewedOutput{TrialID: t.ID, OutputSHA256: t.OutputSHA256, Verdict: VerdictApproved})
	}
	return rv
}

func current() Fingerprint { return Fingerprint{Commit: testCommit, ConfigFingerprint: testConfig} }

func mustRefuse(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("expected a refusal mentioning %q, got %v", want, problems)
}

func TestGateAllowsOnlyAReviewedFullReport(t *testing.T) {
	r := passingReport(TierFull)
	rv := approvingReview(r)
	if problems := Gate(r, &rv, current()); len(problems) > 0 {
		t.Fatalf("a passing, reviewed full report was refused: %v", problems)
	}
}

func TestGateRefusesFastOnlySuccess(t *testing.T) {
	r := passingReport(TierFast)
	rv := approvingReview(r)
	if r.Status != StatusPassed {
		t.Fatalf("the fast report itself is %s", r.Status)
	}
	mustRefuse(t, Gate(r, &rv, current()), "full tier")
}

func TestGateRefusesAnotherCommitOrConfig(t *testing.T) {
	r := passingReport(TierFull)
	rv := approvingReview(r)
	other := current()
	other.Commit = "fedcba9876543210fedcba9876543210fedcba98"
	mustRefuse(t, Gate(r, &rv, other), "the release is")
	other = current()
	other.ConfigFingerprint = "another"
	mustRefuse(t, Gate(r, &rv, other), "configuration fingerprint")
}

func TestGateRefusesMissingOrMismatchedReview(t *testing.T) {
	r := passingReport(TierFull)
	mustRefuse(t, Gate(r, nil, current()), "no lawyer review")

	// The output changed after the lawyer read it: the hash no longer matches.
	rv := approvingReview(r)
	rv.Outputs[7].OutputSHA256 = strings.Repeat("f", 64)
	mustRefuse(t, CheckReview(r, rv), "hash mismatch")
	mustRefuse(t, Gate(r, &rv, current()), "hash mismatch")

	rv = approvingReview(r)
	rv.Outputs = rv.Outputs[1:]
	mustRefuse(t, Gate(r, &rv, current()), "never reviewed")

	rv = approvingReview(r)
	rv.Outputs[0].Verdict = VerdictRejected
	mustRefuse(t, Gate(r, &rv, current()), "rejected")

	rv = approvingReview(r)
	rv.Reviewer = Reviewer{}
	mustRefuse(t, Gate(r, &rv, current()), "names no reviewer")

	// A review of another report (re-run, same commit) does not carry over.
	rv = approvingReview(r)
	rv.ReportSHA256 = "other"
	mustRefuse(t, Gate(r, &rv, current()), "another report")
}

func TestGateRefusesATamperedReport(t *testing.T) {
	r := passingReport(TierFull)
	rv := approvingReview(r)
	r.Trials[3].Failures = nil
	r.Trials[3].OutputSHA256 = strings.Repeat("e", 64) // edited after sealing
	mustRefuse(t, Gate(r, &rv, current()), "edited after it was written")
}

// No majority vote, no denominator reduction: one failed trial, one trial
// not run, one unknown usage — each keeps the report from passing.
func TestReportStatusNeverShrinksTheDenominator(t *testing.T) {
	fail := passingReport(TierFull)
	fail.Trials[0].Status = StatusFailed
	fail.Aggregate()
	if fail.Status != StatusFailed || fail.Counts.Planned != 396 {
		t.Fatalf("one failed trial: status %s planned %d", fail.Status, fail.Counts.Planned)
	}

	budget := passingReport(TierFull)
	for i := 200; i < len(budget.Trials); i++ {
		budget.Trials[i] = TrialResult{ID: budget.Trials[i].ID, Status: StatusNotRun}
	}
	budget.StoppedBy = StatusIncompleteBudget
	budget.Aggregate()
	if budget.Status != StatusIncompleteBudget || budget.Counts.Planned != 396 || budget.Counts.NotRun != 196 {
		t.Fatalf("budget stop: status %s counts %+v", budget.Status, budget.Counts)
	}
	budget.Seal()
	rv := approvingReview(budget)
	mustRefuse(t, Gate(budget, &rv, current()), "incomplete_budget")

	unknown := passingReport(TierFull)
	unknown.Usage.UnknownUsage = 1
	unknown.Aggregate()
	if unknown.Status != StatusUnknownUsage {
		t.Fatalf("unknown usage: status %s", unknown.Status)
	}
	unknown.Seal()
	rv = approvingReview(unknown)
	mustRefuse(t, Gate(unknown, &rv, current()), "unknown usage")

	timeout := passingReport(TierFull)
	timeout.Trials[5].Status = StatusIncompleteTimeout
	timeout.Aggregate()
	if timeout.Status != StatusIncompleteTimeout {
		t.Fatalf("timeout: status %s", timeout.Status)
	}

	harness := passingReport(TierFull)
	harness.Flags.RequireDependencies = false
	harness.Seal()
	rv = approvingReview(harness)
	mustRefuse(t, Gate(harness, &rv, current()), "require-dependencies")
}

// A run that may have read a .env file cannot authorise release.
func TestGateRequiresANoDotenvRun(t *testing.T) {
	r := passingReport(TierFull)
	r.Flags.NoDotenv = false
	r.Seal()
	rv := approvingReview(r)
	mustRefuse(t, Gate(r, &rv, current()), ".env")
}

// The gold lists and compétences the oracles rest on are drafts until a
// lawyer signs them: a report graded against drafts authorises nothing,
// whether the drafts were unsigned when it ran or are unsigned now.
func TestGateRefusesUnreviewedEvaluationMaterial(t *testing.T) {
	m, fr, ag := loadAll(t)
	drafts := UnreviewedMaterial([]Suite{fr, ag}, m)
	for _, want := range []string{"gold list vasseur_chronologie", "compétence requete-cph-resiliation-judiciaire"} {
		if !slices.Contains(drafts, want) {
			t.Fatalf("unreviewed material %v does not name %q", drafts, want)
		}
	}
	signed := m
	signed.Gold = map[string]GoldList{}
	for k, g := range m.Gold {
		g.Status = "reviewed by Me Exemple, 2026-10-20"
		signed.Gold[k] = g
	}
	signed.Competences = map[string]CompetenceFixture{}
	for k, c := range m.Competences {
		c.Status = "Reviewed by Me Exemple, 2026-10-20"
		signed.Competences[k] = c
	}
	if left := UnreviewedMaterial([]Suite{fr, ag}, signed); len(left) > 0 {
		t.Fatalf("signed material still listed: %v", left)
	}

	r := passingReport(TierFull)
	rv := approvingReview(r)
	now := current()
	now.Unreviewed = drafts
	mustRefuse(t, Gate(r, &rv, now), "not reviewed by a lawyer")

	r.Fingerprint.Unreviewed = []string{"gold list vasseur_chronologie"}
	r.Seal()
	rv = approvingReview(r)
	mustRefuse(t, Gate(r, &rv, current()), "gold list vasseur_chronologie")
}
