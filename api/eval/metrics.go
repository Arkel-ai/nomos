// Package eval is the relevance evaluation harness for Nomos retrieval
// (docs/PLAN.md §6, §15 open decision 3).
//
// WHY THIS EXISTS. §6.2 fixes a retrieval design — two arms, reciprocal rank
// fusion, then a cross-encoder rerank — and §6.3 says out loud what that
// design does NOT establish. Neither statement is a measurement. Without a
// scored query set, "hybrid beats vector" is a claim, "the reranker is worth
// its money" is a hope, and "open-source-only retrieval is good enough" (§15,
// open decision 3) is unanswerable. This package is how every future retrieval
// change gets judged, and it scores the SAME code path the product uses —
// services.Retrieve — because a harness that scores a different path scores
// nothing.
//
// This file is the arithmetic, deliberately dependency-free and separately
// unit-tested against hand-computed values. A metric implementation that is
// only ever exercised by the pipeline it grades is not a measurement either.
package eval

import (
	"math"
	"sort"
)

// Gold relevance is GRADED, not binary, because "answers the question" and
// "you would also cite this" are different things to a lawyer and blending
// them hides exactly the failures worth catching:
//
//	2 — the article that ANSWERS the query. Missing it is a wrong answer.
//	1 — an article a lawyer would cite alongside it. Missing it is a thin answer.
//
// Recall, precision and MRR treat any grade ≥ 1 as relevant; nDCG is the only
// metric that uses the grades, which is why nDCG@10 is the regression gate.
const (
	GradeSupporting = 1
	GradePrimary    = 2
	maxGrade        = 3
)

// dedupe collapses a ranked list of article numbers to first occurrence.
// Several chunks of the same article may be retrieved; for relevance the unit
// is the ARTICLE, because that is what a citation names. Counting the same
// article three times would inflate precision for a system that returned one
// article three ways.
func dedupe(ranked []string) []string {
	seen := make(map[string]struct{}, len(ranked))
	out := make([]string, 0, len(ranked))
	for _, a := range ranked {
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// RecallAtK is the share of gold articles that appear in the top k.
//
// It is the metric that matters most here: a lawyer scanning ten results will
// find a cited article if it is on the page, but cannot use one that never
// came back. Returns 0 for an empty gold set — a caller must not average
// recall over negative controls.
func RecallAtK(ranked []string, gold map[string]int, k int) float64 {
	if len(gold) == 0 || k <= 0 {
		return 0
	}
	found := 0
	for _, a := range topK(dedupe(ranked), k) {
		if gold[a] >= GradeSupporting {
			found++
		}
	}
	return float64(found) / float64(len(gold))
}

// PrecisionAtK is the share of the top k that is relevant.
//
// The denominator is k, NOT len(ranked): a system that returns three results
// of which two are relevant has not earned precision@5 = 0.67, it has returned
// two useful results out of the five slots the UI shows.
func PrecisionAtK(ranked []string, gold map[string]int, k int) float64 {
	if k <= 0 {
		return 0
	}
	found := 0
	for _, a := range topK(dedupe(ranked), k) {
		if gold[a] >= GradeSupporting {
			found++
		}
	}
	return float64(found) / float64(k)
}

// ReciprocalRank is 1/rank of the FIRST relevant article, 0 if none.
// Averaged over a query set this is MRR: how far down the page the lawyer has
// to read before the first thing worth reading.
func ReciprocalRank(ranked []string, gold map[string]int) float64 {
	if len(gold) == 0 {
		return 0
	}
	for i, a := range dedupe(ranked) {
		if gold[a] >= GradeSupporting {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// NDCGAtK is discounted cumulative gain at k over the ideal ordering.
//
// Gain is 2^grade - 1, discount is 1/log2(rank+1) — the standard formulation,
// spelled out rather than imported so the numbers in README.md can be checked
// by hand. This is the only metric that distinguishes "the article that
// answers the question is 1st" from "it is 9th behind eight supporting ones",
// which is why it is the regression gate.
func NDCGAtK(ranked []string, gold map[string]int, k int) float64 {
	if len(gold) == 0 || k <= 0 {
		return 0
	}
	dcg := 0.0
	for i, a := range topK(dedupe(ranked), k) {
		dcg += gain(gold[a]) / math.Log2(float64(i)+2)
	}
	ideal := idealDCG(gold, k)
	if ideal == 0 {
		return 0
	}
	return dcg / ideal
}

func gain(grade int) float64 {
	if grade <= 0 {
		return 0
	}
	if grade > maxGrade {
		grade = maxGrade
	}
	return math.Exp2(float64(grade)) - 1
}

// idealDCG is the DCG of the best possible ordering: every gold article, most
// relevant first, truncated at k.
func idealDCG(gold map[string]int, k int) float64 {
	grades := make([]int, 0, len(gold))
	for _, g := range gold {
		grades = append(grades, g)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))
	out := 0.0
	for i, g := range topKInt(grades, k) {
		out += gain(g) / math.Log2(float64(i)+2)
	}
	return out
}

// Abstained reports whether a result set is correctly EMPTY of confident hits.
//
// This is the negative-control metric, and it is the one most retrieval
// evaluations skip. A system that always returns ten articles scores fine on
// recall and is dangerous in a legal product: for a question its corpus cannot
// answer, the honest output is nothing. `scores` is the final ordering score
// of each hit; abstention means the top score is below the threshold.
//
// The threshold is only meaningful when the score is CALIBRATED. A rerank
// score is (cohere/rerank-4-fast returns a 0–1 relevance estimate); an RRF score
// is a sum of 1/(k+rank) and carries no notion of "relevant at all", which is
// a real finding about the non-reranked strategies rather than a gap in this
// function — see README.md.
func Abstained(scores []float64, threshold float64) bool {
	for _, s := range scores {
		if s >= threshold {
			return false
		}
	}
	return true
}

// LeakedForbidden reports the forbidden articles that appeared in the top k.
//
// Used by the temporal cases: an article whose version is not in force at the
// as-of date must not be reachable at all (§6.4). Advising on the 2018 barème
// for a 2016 dismissal is not a ranking error, it is wrong law, so this is
// counted separately from recall and never traded off against it.
func LeakedForbidden(ranked []string, forbidden []string, k int) []string {
	if len(forbidden) == 0 {
		return nil
	}
	ban := make(map[string]struct{}, len(forbidden))
	for _, f := range forbidden {
		ban[f] = struct{}{}
	}
	var out []string
	for _, a := range topK(dedupe(ranked), k) {
		if _, bad := ban[a]; bad {
			out = append(out, a)
		}
	}
	return out
}

// RankAgreement is the share of the top k that two rankings share, in the same
// slots — 1.0 only for an identical prefix.
//
// The accent/casing variant pairs use it: "congés payés" and "conges payes"
// must reach the same articles, because unaccent is applied to the query
// exactly as it was applied to the text at write time (§6.2). A drop here is a
// normalisation bug, not a relevance opinion.
func RankAgreement(a, b []string, k int) float64 {
	x, y := topK(dedupe(a), k), topK(dedupe(b), k)
	n := max(len(x), len(y))
	if n == 0 {
		return 1
	}
	same := 0
	for i := 0; i < min(len(x), len(y)); i++ {
		if x[i] == y[i] {
			same++
		}
	}
	return float64(same) / float64(n)
}

// Mean is the macro-average used everywhere in the scoreboard: every query
// counts once, regardless of how many gold articles it has. A micro-average
// would let the five-gold multi-hop cases outvote the single-gold lookups.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func topK(xs []string, k int) []string {
	if k < len(xs) {
		return xs[:k]
	}
	return xs
}

func topKInt(xs []int, k int) []int {
	if k < len(xs) {
		return xs[:k]
	}
	return xs
}
