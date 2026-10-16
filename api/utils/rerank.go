package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Arkel-ai/nomos/api/types"
)

var rerankLog = NewLogger("utils.rerank")

// RerankHit is one scored document: Index refers to the caller's docs slice,
// so the caller keeps its own metadata and never has to match on text.
type RerankHit struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
}

type rerankResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Results []struct {
		Index int     `json:"index"`
		Score float64 `json:"relevance_score"`
	} `json:"results"`
	Usage *struct {
		SearchUnits int     `json:"search_units"`
		Cost        float64 `json:"cost"`
	} `json:"usage"`
}

// Rerank scores docs against query and returns the top N, best first.
//
// This is the second stage of hybrid retrieval (§6.2): RRF fuses the vector
// and FTS arms into ~40 candidates, the reranker orders them, 8–12 survive.
// What it does NOT do is establish precedential direction — a reranker rewards
// topical similarity, not whether a decision was overruled (§6.3). Callers
// must not present its order as legal authority.
func Rerank(ctx context.Context, model, query string, docs []string, topN int) ([]RerankHit, error) {
	g := llm()
	if g.apiKey == "" {
		return nil, ErrLLMNotConfigured
	}
	if model == "" {
		model = g.rerankModel
	}
	if len(docs) == 0 || query == "" {
		return nil, nil
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}

	ref := CallRefFrom(ctx)
	_, callID := beginLLMCall(model, types.LLMPurposeRerank, ref)

	raw, err := g.post(ctx, "/rerank", map[string]any{
		"model":     model,
		"query":     query,
		"documents": docs,
		"top_n":     topN,
		"provider":  g.providerRouting(nil),
	})
	if err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, types.LLMPurposeRerank)
		return nil, err
	}

	var resp rerankResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, types.LLMPurposeRerank)
		return nil, fmt.Errorf("decode rerank: %w", err)
	}

	// Rerank is billed per search unit, not per token: the usage record is
	// the provider's cost plus the unit count, and prompt/completion stay 0.
	usage := llmUsage{}
	var units []callAttribution
	if resp.Usage != nil {
		usage = llmUsage{CostUSD: resp.Usage.Cost, Known: true}
		units = append(units, callAttribution{UsageDetails: map[string]any{"search_units": resp.Usage.SearchUnits}})
	}
	finishLLMCall(callID, resp.ID, types.LLMCallStateSucceeded, usage, model, types.LLMPurposeRerank, units...)

	hits := make([]RerankHit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, fmt.Errorf("rerank: result index %d out of range", r.Index)
		}
		hits = append(hits, RerankHit{Index: r.Index, Score: r.Score})
	}
	// The API already returns them ordered, but the ranking is the whole
	// product of this call — sort rather than trust.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topN {
		hits = hits[:topN]
	}

	rerankLog.Infof("Reranked %d documents (model=%s kept=%d)", len(docs), model, len(hits))
	return hits, nil
}
