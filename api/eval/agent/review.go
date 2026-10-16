package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// The lawyer's review: production-model outputs block
// release until a practising lawyer has read them. The review is bound to
// output HASHES, so an output that changed after it was read is simply not
// reviewed — an old approval can never cover a new text.

// ReviewVersion pins the review file's shape.
const ReviewVersion = 1

// Verdicts.
const (
	VerdictApproved = "approved"
	VerdictRejected = "rejected"
)

// Review is the reviewer's signed-off file.
type Review struct {
	Version           int              `json:"version"`
	Reviewer          Reviewer         `json:"reviewer"`
	ReviewedAt        string           `json:"reviewed_at"` // RFC 3339
	Commit            string           `json:"commit"`
	ConfigFingerprint string           `json:"config_fingerprint"`
	ReportSHA256      string           `json:"report_sha256"`
	Outputs           []ReviewedOutput `json:"outputs"`
}

// Reviewer identifies the lawyer.
type Reviewer struct {
	Name string `json:"name"`
	Role string `json:"role"` // e.g. "avocat"
	Bar  string `json:"bar"`  // e.g. "Barreau d'Amiens"
}

// ReviewedOutput is one trial output the lawyer read, by hash.
type ReviewedOutput struct {
	TrialID      string `json:"trial_id"`
	OutputSHA256 string `json:"output_sha256"`
	Verdict      string `json:"verdict"`
	Notes        string `json:"notes,omitempty"`
}

// ReadReview reads a review file strictly.
func ReadReview(path string) (Review, error) {
	var rv Review
	raw, err := os.ReadFile(path)
	if err != nil {
		return rv, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rv); err != nil {
		return rv, fmt.Errorf("review %s: %w", path, err)
	}
	return rv, nil
}

// VerifyReport checks a report's own integrity: the seal, and totals that
// agree with the trials they summarise. It says nothing about passing.
func VerifyReport(r Report) []string {
	var out []string
	if r.Version != ReportVersion {
		out = append(out, fmt.Sprintf("report version %d, this tool reads %d", r.Version, ReportVersion))
	}
	if !r.Sealed() {
		out = append(out, "the report's sha256 does not match its content: it was edited after it was written")
	}
	again := r
	again.Trials = append([]TrialResult(nil), r.Trials...)
	again.Aggregate()
	if again.Counts != r.Counts || again.Status != r.Status {
		out = append(out, fmt.Sprintf("the report's totals (%s, %+v) do not follow from its trials (%s, %+v)", r.Status, r.Counts, again.Status, again.Counts))
	}
	if w, ok := Workloads[r.Tier]; ok && r.Counts.Planned != w.Total() {
		out = append(out, fmt.Sprintf("tier %s plans %d trials, the report lists %d", r.Tier, w.Total(), r.Counts.Planned))
	}
	ids := map[string]bool{}
	for _, t := range r.Trials {
		if ids[t.ID] {
			out = append(out, "trial listed twice: "+t.ID)
		}
		ids[t.ID] = true
		if t.Status == StatusPassed && t.OutputSHA256 == "" {
			out = append(out, "a passed trial has no output hash: "+t.ID)
		}
	}
	return out
}

// CheckReview matches a review against the report it claims to cover: same
// commit, same configuration fingerprint, same sealed report, a named
// reviewer, and every trial output approved at its exact hash.
func CheckReview(r Report, rv Review) []string {
	var out []string
	if rv.Version != ReviewVersion {
		out = append(out, fmt.Sprintf("review version %d, this tool reads %d", rv.Version, ReviewVersion))
	}
	if strings.TrimSpace(rv.Reviewer.Name) == "" || strings.TrimSpace(rv.Reviewer.Role) == "" {
		out = append(out, "the review names no reviewer (name and role)")
	}
	if _, err := time.Parse(time.RFC3339, rv.ReviewedAt); err != nil {
		out = append(out, "reviewed_at is not an RFC 3339 time")
	}
	if rv.Commit != r.Fingerprint.Commit {
		out = append(out, fmt.Sprintf("the review is for commit %s, the report for %s", short(rv.Commit), short(r.Fingerprint.Commit)))
	}
	if rv.ConfigFingerprint != r.Fingerprint.ConfigFingerprint {
		out = append(out, "the review is for another configuration fingerprint")
	}
	if rv.ReportSHA256 != r.ReportSHA256 {
		out = append(out, "the review is for another report (report_sha256 differs)")
	}
	reviewed := map[string]ReviewedOutput{}
	for _, o := range rv.Outputs {
		if _, dup := reviewed[o.TrialID]; dup {
			out = append(out, "output reviewed twice: "+o.TrialID)
		}
		reviewed[o.TrialID] = o
	}
	var unreviewed, mismatched, rejected []string
	for _, t := range r.Trials {
		if t.OutputSHA256 == "" {
			continue
		}
		o, ok := reviewed[t.ID]
		switch {
		case !ok:
			unreviewed = append(unreviewed, t.ID)
		case o.OutputSHA256 != t.OutputSHA256:
			mismatched = append(mismatched, t.ID)
		case o.Verdict == VerdictRejected:
			rejected = append(rejected, t.ID)
		case o.Verdict != VerdictApproved:
			out = append(out, fmt.Sprintf("%s: verdict %q is neither approved nor rejected", t.ID, o.Verdict))
		}
		delete(reviewed, t.ID)
	}
	for id := range reviewed {
		out = append(out, "the review covers an output the report does not have: "+id)
	}
	if len(mismatched) > 0 {
		sort.Strings(mismatched)
		out = append(out, fmt.Sprintf("%d output(s) changed since they were reviewed (hash mismatch): %s", len(mismatched), strings.Join(mismatched, ", ")))
	}
	if len(unreviewed) > 0 {
		sort.Strings(unreviewed)
		out = append(out, fmt.Sprintf("%d output(s) were never reviewed: %s", len(unreviewed), strings.Join(firstN(unreviewed, 10), ", ")))
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		out = append(out, fmt.Sprintf("the reviewer rejected %d output(s): %s", len(rejected), strings.Join(rejected, ", ")))
	}
	sort.Strings(out)
	return out
}

// Gate decides release: a FULL-tier report that passed every trial,
// under -require-models and -require-dependencies with no .env file read,
// sealed, for exactly the commit being deployed and the configuration
// fingerprint recomputed from this checkout, graded against gold lists and
// compétences a lawyer signed, with a matching lawyer review. Fast-only
// success, another commit's report, a missing review, a timeout, unknown cost
// or missing dependencies cannot authorise release. It returns every reason
// it refuses.
func Gate(r Report, rv *Review, current Fingerprint) []string {
	out := VerifyReport(r)
	if r.Tier != TierFull {
		out = append(out, fmt.Sprintf("a %s-tier report cannot authorise release: the gate needs the full tier", r.Tier))
	}
	if r.Status != StatusPassed {
		out = append(out, fmt.Sprintf("the report's status is %s", r.Status))
	}
	if w := Workloads[TierFull]; r.Counts.Passed != w.Total() || r.Counts.Planned != w.Total() {
		out = append(out, fmt.Sprintf("%d/%d full-tier trials passed, every one of the %d must", r.Counts.Passed, r.Counts.Planned, w.Total()))
	}
	if !r.Flags.RequireModels || !r.Flags.RequireDependencies {
		out = append(out, "the run was not made under both -require-models and -require-dependencies")
	}
	if !r.Flags.NoDotenv {
		out = append(out, "the run may have read a .env file: only the exported environment can configure a gate run")
	}
	// The oracles' gold lists and the compétences are drafts until a lawyer
	// signs them; a report graded against drafts authorises nothing.
	if unsigned := append(slices.Clone(r.Fingerprint.Unreviewed), current.Unreviewed...); len(unsigned) > 0 {
		slices.Sort(unsigned)
		out = append(out, "evaluation material not reviewed by a lawyer: "+strings.Join(slices.Compact(unsigned), ", "))
	}
	if r.Usage.UnknownUsage > 0 || r.IngestionUsage.UnknownUsage > 0 {
		out = append(out, fmt.Sprintf("%d call(s) have unknown usage", r.Usage.UnknownUsage))
	}
	if r.ModelIssues > 0 {
		out = append(out, fmt.Sprintf("%d unapproved model substitution(s) or unknown model(s)", r.ModelIssues))
	}
	if len(r.Prompts) > 1 {
		out = append(out, "the trials ran with more than one system prompt")
	}
	if r.Fingerprint.Dirty {
		out = append(out, "the report was produced from a tree with uncommitted changes")
	}
	if current.Dirty {
		out = append(out, "the checkout being released has uncommitted changes")
	}
	if r.Fingerprint.Commit != current.Commit {
		out = append(out, fmt.Sprintf("the report is for commit %s, the release is %s", short(r.Fingerprint.Commit), short(current.Commit)))
	}
	if r.Fingerprint.ConfigFingerprint != current.ConfigFingerprint {
		out = append(out, "the report's configuration fingerprint differs from this checkout's and release config's")
	}
	if rv == nil {
		out = append(out, "no lawyer review: production outputs block release until reviewed")
	} else {
		out = append(out, CheckReview(r, *rv)...)
	}
	return out
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("… %d more", len(s)-n))
	}
	return s
}
