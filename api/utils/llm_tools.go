package utils

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Arkel-ai/nomos/api/types"
)

var llmToolsLog = NewLogger("utils.llm_tools")

// Native tool calling on the durable gateway.
//
// ChatTools is the agent's ONE way to reach a model, and it differs from Chat
// on purpose:
//
//   - It makes exactly ONE provider attempt. Every hidden retry would be a
//     controller dispatch the agent's 40-dispatch ceiling never saw; a
//     transport failure goes back to the durable loop, which reserves a new
//     dispatch for the next try.
//   - Accounting is strict. The llm_calls row is written before the request
//     leaves (or was already written by the caller's dispatch reservation),
//     and if it cannot be written nothing is sent. A reconciliation failure
//     is returned, never logged away: an output whose cost is not recorded
//     is not "fully recorded".
//   - A completion made only of tool calls, with no text, is a normal
//     result. A refusal and a truncation stay explicit unusable outputs.
//
// Provider routing, cost accounting and the usage trailer are the same as
// Chat's; legacy callers are untouched.

// ErrAccounting means the durable llm_calls record could not be written or
// reconciled. Before dispatch it means nothing was sent.
var ErrAccounting = errors.New("llm call accounting failed")

// Transcript roles.
const (
	ToolRoleSystem    = "system"
	ToolRoleUser      = "user"
	ToolRoleAssistant = "assistant"
	ToolRoleTool      = "tool"
)

// FunctionSpec is one native function offered to the model (not to be
// confused with exec.go's ToolSpec, an external command).
type FunctionSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCallWire is one function call as the provider returned it. Arguments
// is the raw JSON text of the arguments, exactly as received.
type ToolCallWire struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolMessage is one message of a native tool-calling transcript. Assistant
// tool calls must precede the tool messages that answer them, each carrying
// the call's provider id in ToolCallID.
type ToolMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCallWire
	ToolCallID string
	// ReasoningDetails is the provider's opaque reasoning continuation of an
	// assistant message, replayed exactly as returned. Private: never shown.
	ReasoningDetails json.RawMessage
}

// ReservedCall is an llm_calls row the caller already committed — in the same
// transaction that reserved the dispatch — so ChatTools only reconciles it.
type ReservedCall struct {
	ID          string
	ExecutionID string
}

// ToolChatOpts is one native tool-calling completion.
type ToolChatOpts struct {
	Model    string
	Messages []ToolMessage
	Tools    []FunctionSpec

	// MaxTokens caps the completion, reasoning included. Zero leaves the
	// provider default.
	MaxTokens       int
	ReasoningEffort string

	// Purpose is a short label for llm_calls.purpose and the cost record.
	Purpose string

	TenantID      string
	RunID         string
	RunStepID     string
	StepAttemptID string
	// ActorID is the user the spend is attributed to.
	ActorID string
	// AgentDispatch is the controller dispatch number; zero for other calls.
	AgentDispatch int
	// InputManifestSHA256 identifies the exact input sent.
	InputManifestSHA256 string

	Providers []string
	Reserved  *ReservedCall
}

// ToolChatUsage is the usage trailer, with the counters agent accounting
// needs. Known is false when the provider returned none.
type ToolChatUsage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	ReasoningTokens  int
	CostUSD          float64
	ServerToolUse    map[string]any
	Known            bool
}

// ToolChatResult is one completed provider attempt.
type ToolChatResult struct {
	LLMCallID         string
	ExecutionID       string
	ProviderRequestID string
	// Model is the model the provider says it ran, which may differ from the
	// one requested.
	Model            string
	FinishReason     string
	Content          string
	Refusal          string
	ToolCalls        []ToolCallWire
	ReasoningDetails json.RawMessage
	Usage            ToolChatUsage
	UsageStatus      string
}

// NewLLMCallRecord builds the dispatched llm_calls row for opts, with fresh
// row and execution ids. A caller that reserves a dispatch inserts it in the
// reservation transaction and hands its ids to ChatTools as Reserved.
func NewLLMCallRecord(opts ToolChatOpts) types.LLMCall {
	return newLLMCallRow(opts.Model, opts.purpose(), opts.ref(), opts.attribution())
}

func (o ToolChatOpts) purpose() string {
	if o.Purpose == "" {
		return types.LLMPurposeChat
	}
	return o.Purpose
}

func (o ToolChatOpts) ref() CallRef {
	return CallRef{TenantID: o.TenantID, RunID: o.RunID, RunStepID: o.RunStepID, StepAttemptID: o.StepAttemptID}
}

func (o ToolChatOpts) attribution() callAttribution {
	return callAttribution{ActorID: o.ActorID, AgentDispatch: o.AgentDispatch, InputManifestSHA256: o.InputManifestSHA256}
}

// ToolRequestSHA256 hashes exactly what is sent: the model, the transcript
// and the offered tools. Two dispatches with the same hash saw the same input.
func ToolRequestSHA256(model string, messages []ToolMessage, tools []FunctionSpec) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": wireToolMessages(messages),
		"tools":    wireTools(tools),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateToolArguments checks a call's raw arguments against its strict
// schema (type, required, properties, additionalProperties, items, enum): the
// same server-owned validation ChatJSON applies to structured output.
func ValidateToolArguments(args json.RawMessage, schema map[string]any) error {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		return errors.New("arguments are empty")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	if dec.More() {
		return errors.New("arguments carry trailing content after the JSON object")
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("arguments must be a JSON object, got %s", jsonKind(value))
	}
	// A schema written in Go carries []string required lists and enums, which
	// the validator only reads as []any: a JSON round trip gives it the shape
	// a provider would see.
	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("tool schema is not encodable: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("tool schema is not an object: %v", err)
	}
	return validateNode("$", value, doc)
}

// toolChatResponse is the OpenAI-compatible completion with native tool
// calls, plus OpenRouter's usage details.
type toolChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string `json:"content"`
			Refusal   string `json:"refusal"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ReasoningDetails json.RawMessage `json:"reasoning_details"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		ServerToolUse map[string]any `json:"server_tool_use"`
	} `json:"usage"`
}

// ChatTools makes ONE native tool-calling completion attempt. It returns
// ErrRefused or ErrTruncated (with the result) for an unusable completion, a
// *GatewayError or a context error for a failed attempt, and ErrAccounting
// when the durable record could not be written or reconciled.
func ChatTools(ctx context.Context, opts ToolChatOpts) (ToolChatResult, error) {
	g := llm()
	if g.apiKey == "" {
		return ToolChatResult{}, ErrLLMNotConfigured
	}
	if opts.Model == "" {
		opts.Model = g.extractionModel
	}
	purpose := opts.purpose()

	body := map[string]any{
		"model":    opts.Model,
		"messages": wireToolMessages(opts.Messages),
		"provider": g.providerRouting(opts.Providers),
		"usage":    map[string]any{"include": true},
	}
	if len(opts.Tools) > 0 {
		body["tools"] = wireTools(opts.Tools)
		body["tool_choice"] = "auto"
	}
	if opts.MaxTokens > 0 {
		body["max_tokens"] = opts.MaxTokens
	}
	if opts.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": opts.ReasoningEffort}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ToolChatResult{}, fmt.Errorf("marshal request: %w", err)
	}

	// The record exists BEFORE the request leaves, or the request does not
	// leave at all: beginLLMCall's empty id means nothing was recorded.
	ref, attribution := opts.ref(), opts.attribution()
	reserved := opts.Reserved
	if reserved == nil {
		executionID, callID := beginLLMCall(opts.Model, purpose, ref, attribution)
		if callID == "" {
			llmToolsLog.Errorf("Dispatch refused: llm_call could not be recorded (model=%s purpose=%s)", opts.Model, purpose)
			return ToolChatResult{}, fmt.Errorf("%w: the dispatch could not be recorded", ErrAccounting)
		}
		reserved = &ReservedCall{ID: callID, ExecutionID: executionID}
	}
	result := ToolChatResult{LLMCallID: reserved.ID, ExecutionID: reserved.ExecutionID}

	llmToolsLog.Debugf("Tool dispatch (model=%s purpose=%s messages=%d tools=%d dispatch=%d)",
		opts.Model, purpose, len(opts.Messages), len(opts.Tools), opts.AgentDispatch)
	raw, callErr := g.attempt(ctx, "/chat/completions", payload)
	if callErr != nil {
		outcome := "failed"
		var ge *GatewayError
		if !errors.As(callErr, &ge) || ge.Status == 0 {
			// No status: the provider may have accepted the request. The
			// spend is unknown and stays recorded as such.
			outcome = "unknown"
		}
		attribution.UsageDetails = map[string]any{"outcome": outcome}
		if err := finishLLMCall(reserved.ID, "", types.LLMCallStateFailed, llmUsage{}, opts.Model, purpose,
			attribution); err != nil {
			llmToolsLog.Errorf("Could not reconcile failed llm_call (model=%s): %v", opts.Model, err)
		}
		llmToolsLog.Warnf("Tool dispatch failed (model=%s purpose=%s dispatch=%d outcome=%s): %v",
			opts.Model, purpose, opts.AgentDispatch, outcome, callErr)
		return result, callErr
	}

	var resp toolChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		attribution.ReturnedModel = resp.Model
		attribution.UsageDetails = map[string]any{"outcome": "undecodable"}
		if rerr := finishLLMCall(reserved.ID, resp.ID, types.LLMCallStateFailed, llmUsage{}, opts.Model, purpose,
			attribution); rerr != nil {
			llmToolsLog.Errorf("Could not reconcile undecodable llm_call (model=%s): %v", opts.Model, rerr)
		}
		return result, &GatewayError{Status: 200, Message: "completion could not be decoded", Retryable: true}
	}

	result.ProviderRequestID = resp.ID
	result.Model = resp.Model
	choice := resp.Choices[0]
	result.FinishReason = choice.FinishReason
	result.Content = choice.Message.Content
	result.Refusal = choice.Message.Refusal
	if rd := bytes.TrimSpace(choice.Message.ReasoningDetails); len(rd) > 0 && string(rd) != "null" {
		result.ReasoningDetails = rd
	}
	for _, tc := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCallWire{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: rawArguments(tc.Function.Arguments),
		})
	}
	if u := resp.Usage; u != nil {
		result.Usage = ToolChatUsage{
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			CostUSD:          u.Cost,
			ServerToolUse:    u.ServerToolUse,
			Known:            true,
		}
		if u.PromptTokensDetails != nil {
			result.Usage.CachedTokens = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			result.Usage.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
		}
	}
	result.UsageStatus = types.UsageStatusUnknown
	if result.Usage.Known {
		result.UsageStatus = types.UsageStatusKnown
	}

	// A refusal or a truncation is a completed, billable call: recorded as
	// succeeded (the money was spent), returned as unusable.
	usage, details := toolCallUsage(result.Usage)
	attribution.ReturnedModel, attribution.UsageDetails = resp.Model, details
	if err := finishLLMCall(reserved.ID, resp.ID, types.LLMCallStateSucceeded, usage, opts.Model, purpose,
		attribution); err != nil {
		llmToolsLog.Errorf("llm_call reconciliation failed (model=%s purpose=%s dispatch=%d): %v",
			opts.Model, purpose, opts.AgentDispatch, err)
		return result, fmt.Errorf("%w: %v", ErrAccounting, err)
	}
	llmToolsLog.Infof("Tool dispatch completed (model=%s returned=%s purpose=%s dispatch=%d finish=%s calls=%d content_chars=%d prompt_tokens=%d cached=%d completion_tokens=%d reasoning=%d usage=%s)",
		opts.Model, result.Model, purpose, opts.AgentDispatch, result.FinishReason, len(result.ToolCalls),
		len(result.Content), result.Usage.PromptTokens, result.Usage.CachedTokens,
		result.Usage.CompletionTokens, result.Usage.ReasoningTokens, result.UsageStatus)

	switch {
	case result.Refusal != "" || result.FinishReason == "content_filter":
		return result, ErrRefused
	case result.FinishReason == "length":
		return result, ErrTruncated
	}
	return result, nil
}

// rawArguments returns a call's arguments as JSON text. The OpenAI shape is a
// string holding JSON; a provider that sends the object itself is accepted
// as that object's text.
func rawArguments(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	return string(trimmed)
}

// toolCallUsage turns a completion's usage into the gateway's usage record;
// the counters land in llm_calls.usage_details, next to the acting user and
// the dispatch the row already carries.
// Unknown usage stays unknown: nothing is estimated and nothing is billed.
func toolCallUsage(usage ToolChatUsage) (llmUsage, map[string]any) {
	if !usage.Known {
		return llmUsage{}, nil
	}
	details := map[string]any{
		"cached_tokens":    usage.CachedTokens,
		"reasoning_tokens": usage.ReasoningTokens,
		"cost_usd":         usage.CostUSD,
	}
	if len(usage.ServerToolUse) > 0 {
		details["server_tool_use"] = usage.ServerToolUse
	}
	return llmUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CostUSD:          usage.CostUSD,
		Known:            true,
	}, details
}

// wireToolMessages serialises a transcript into the OpenAI chat shape.
func wireToolMessages(msgs []ToolMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case ToolRoleAssistant:
			w := map[string]any{"role": ToolRoleAssistant}
			if m.Content != "" {
				w["content"] = m.Content
			} else {
				w["content"] = nil
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					calls = append(calls, map[string]any{
						"id":   c.ID,
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": c.Arguments,
						},
					})
				}
				w["tool_calls"] = calls
			}
			if len(m.ReasoningDetails) > 0 {
				w["reasoning_details"] = m.ReasoningDetails
			}
			out = append(out, w)
		case ToolRoleTool:
			out = append(out, map[string]any{
				"role":         ToolRoleTool,
				"tool_call_id": m.ToolCallID,
				"content":      m.Content,
			})
		default:
			out = append(out, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	return out
}

func wireTools(tools []FunctionSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}
