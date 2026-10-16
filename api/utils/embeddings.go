package utils

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Arkel-ai/nomos/api/types"
)

var embedLog = NewLogger("utils.embeddings")

// embeddingResponse is the OpenAI-compatible /embeddings shape plus
// OpenRouter's usage trailer.
type embeddingResponse struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
}

// Embed returns one vector per input, in input order.
//
// The dimension is a real commitment, not a runtime detail: it is baked into
// the halfvec(N) column type, so a model that answers with a different width
// would silently poison the corpus. openai/text-embedding-3-small returns 1536
// dims (verified); anything else is rejected here rather than at INSERT time
// (§9.1(2)).
//
// Inputs are batched, and each batch is its own recorded gateway call — a
// half-delivered batch must still be billable and auditable.
func Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	g := llm()
	if g.apiKey == "" {
		return nil, ErrLLMNotConfigured
	}
	if model == "" {
		model = g.embeddingModel
	}
	if len(inputs) == 0 {
		return nil, nil
	}

	out := make([][]float32, len(inputs))
	for start := 0; start < len(inputs); start += embedBatchSize {
		end := min(start+embedBatchSize, len(inputs))
		if err := embedBatch(ctx, g, model, inputs[start:end], out[start:end]); err != nil {
			return nil, err
		}
	}
	embedLog.Infof("Embedded %d inputs (model=%s dims=%d batches=%d)",
		len(inputs), model, len(out[0]), (len(inputs)+embedBatchSize-1)/embedBatchSize)
	return out, nil
}

func embedBatch(ctx context.Context, g *gateway, model string, batch []string, out [][]float32) error {
	ref := CallRefFrom(ctx)
	_, callID := beginLLMCall(model, types.LLMPurposeEmbed, ref)

	raw, err := g.post(ctx, "/embeddings", map[string]any{
		"model":    model,
		"input":    batch,
		"provider": g.providerRouting(nil),
		"usage":    map[string]any{"include": true},
	})
	if err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, types.LLMPurposeEmbed)
		return err
	}

	var resp embeddingResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, types.LLMPurposeEmbed)
		return fmt.Errorf("decode embeddings: %w", err)
	}
	finishLLMCall(callID, resp.ID, types.LLMCallStateSucceeded, usageFrom(resp.Usage),
		model, types.LLMPurposeEmbed)

	if len(resp.Data) != len(batch) {
		return fmt.Errorf("embeddings: expected %d vectors, got %d", len(batch), len(resp.Data))
	}
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return fmt.Errorf("embeddings: vector index %d out of range", d.Index)
		}
		if g.embeddingDims > 0 && len(d.Embedding) != g.embeddingDims {
			return fmt.Errorf("embeddings: model %s returned %d dimensions, the corpus column is halfvec(%d)",
				model, len(d.Embedding), g.embeddingDims)
		}
		out[d.Index] = d.Embedding
	}
	return nil
}
