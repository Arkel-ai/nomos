package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
)

var llmLog = NewLogger("utils.llm")

// The LLM gateway: one OpenAI-compatible client for every model Nomos uses
// (docs/PLAN.md §4.6 and §12). Chat completions live here; embeddings and
// rerank live in embeddings.go / rerank.go and share this file's transport,
// retry policy and cost path.
//
// Three rules shape the whole file:
//
//  1. Every call carries an explicit context and runs on an explicit
//     http.Client with a timeout — never http.DefaultClient, so a hung
//     provider can never pin a goroutine forever.
//  2. Every request denies data collection, because these prompts carry
//     privileged client material (§13). An OpenAI-compatible URL is not a
//     compliance boundary: when a route allowlist is configured, provider
//     fallback is disabled too, so the prompt cannot silently land at an
//     unapproved processor.
//  3. Every call is recorded BEFORE dispatch and reconciled after, with the
//     provider-reported cost (§4.6). A fire-and-forget record loses exactly
//     the expensive runs.
const (
	// llmRequestTimeout bounds a single HTTP attempt. A drafting completion
	// takes 60–180 s (§2.1), so this is generous on purpose; the caller's
	// context is what normally ends a call early.
	llmRequestTimeout = 5 * time.Minute

	// llmMaxAttempts counts the initial dispatch plus retries. Retries are
	// only ever issued for the transient classes in classifyStatus.
	llmMaxAttempts = 3

	llmBackoffBase = 500 * time.Millisecond
	llmBackoffCap  = 8 * time.Second

	// embedBatchSize bounds one /embeddings request. Used by embeddings.go.
	embedBatchSize = 64
)

// ErrLLMNotConfigured is returned when OPENROUTER_API_KEY is unset. Same
// optional-integration contract as Postgres / S3: the caller turns it into a
// 503 "LLM gateway not configured".
var ErrLLMNotConfigured = errors.New("LLM gateway not configured")

// ErrSchemaInvalid is returned by ChatJSON when the model's reply still fails
// the server-owned schema after the single repair round-trip. It is a
// deterministic parse failure: retrying the identical input would only spend
// money to fail again (§4.3).
var ErrSchemaInvalid = errors.New("model output failed schema validation")

// ErrTruncated is returned when the provider stopped on the token limit. The
// fix is a larger MaxTokens or a smaller input, never a retry.
var ErrTruncated = errors.New("completion truncated at max_tokens")

// ErrRefused is returned on a policy refusal. Never retried.
var ErrRefused = errors.New("model refused the request")

// ErrEmptyCompletion is returned when the call finished normally but the
// answer is empty — every token went to reasoning. It is NOT a refusal: a
// vision model shown a blank page does exactly this. Never retried.
var ErrEmptyCompletion = errors.New("empty completion")

// GatewayError is a non-2xx response from the gateway. Retryable marks the
// transient classes — timeouts, 429 and 5xx (§4.3). Schema-invalid input,
// policy refusal and context-too-large are deliberately NOT retryable.
type GatewayError struct {
	Status    int
	Message   string
	Retryable bool
}

func (e *GatewayError) Error() string {
	return fmt.Sprintf("llm gateway: status %d: %s", e.Status, e.Message)
}

// IsRetryable reports whether err is worth another attempt. Callers in the
// runner use it to decide between requeueing a step and failing it.
func IsRetryable(err error) bool {
	var ge *GatewayError
	if errors.As(err, &ge) {
		return ge.Retryable
	}
	// A context the caller cancelled is not retryable here; a deadline the
	// transport hit is.
	return errors.Is(err, context.DeadlineExceeded)
}

// ContentPart is an optional non-text part of the user message: a page image
// for vision OCR, or a PDF handed to a model that accepts file input
// (google/gemini-3.7-flash does — §12). Data must already be a data: URI or a
// URL the provider can fetch; this package never reads the filesystem.
type ContentPart struct {
	// Kind is "image" or "file".
	Kind string
	// URL is the data: URI (or https URL) of an image part.
	URL string
	// Filename and FileData carry a file part.
	Filename string
	FileData string
}

// ChatOpts is one chat completion. Purpose lands in llm_calls.purpose, and
// the Tenant/Run/Step ids are what make the cost attributable to a mission.
type ChatOpts struct {
	// Model defaults to the configured extraction model — the cheap one — so
	// a forgotten field can never silently route to the expensive drafter.
	Model string
	// System is the STATIC identity prompt. Variable data belongs in User:
	// a system prompt that carries the dossier is a prompt
	// that cannot be cached, reviewed or reasoned about.
	System string
	User   string
	Parts  []ContentPart

	MaxTokens int
	// Temperature is applied only when non-nil, so the provider default
	// stands for callers that do not care.
	Temperature *float64
	// ReasoningEffort ("low", "medium", "high") caps the model's hidden
	// reasoning. Empty leaves the provider default. Transcription needs
	// none: on a scan it is most of the latency and the cost.
	ReasoningEffort string

	// Purpose is a short constant from types.LLMPurpose*.
	Purpose string

	TenantID      string
	RunID         string
	RunStepID     string
	StepAttemptID string

	// Providers is a per-call upstream allowlist. Non-empty also disables
	// provider fallback (§13).
	Providers []string
}

// ChatResult is one completed call, including everything the audit trail
// needs. UsageStatus is types.UsageStatusKnown or types.UsageStatusUnknown —
// never an estimate, because a character count is not auditable billing.
type ChatResult struct {
	Text              string
	Model             string
	ExecutionID       string
	ProviderRequestID string
	FinishReason      string
	PromptTokens      int
	CompletionTokens  int
	CostUSD           float64
	UsageStatus       string
	// Citations are the web pages an :online model actually consulted. Empty
	// for every other call. They are PROVENANCE, not citable sources: nothing
	// here is stored, hashed or resolvable, so they may be shown to the
	// lawyer and steer further searching, never cited in a note (§6).
	Citations []WebCitation
}

// WebCitation is one url_citation annotation from an :online completion.
type WebCitation struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// ---------------------------------------------------------------------------
// Gateway handle
// ---------------------------------------------------------------------------

type gateway struct {
	baseURL         string
	apiKey          string
	http            *http.Client
	providers       []string
	extractionModel string
	embeddingModel  string
	rerankModel     string
	embeddingDims   int
}

var (
	gwOnce sync.Once
	gw     *gateway
)

// llm returns the lazily-built gateway. It reads configuration on first use
// (after main's godotenv.Load), which keeps utils.InitClients — owned by the
// template — free of Nomos-specific wiring.
func llm() *gateway {
	gwOnce.Do(func() {
		cfg := config.Load()
		gw = &gateway{
			baseURL: strings.TrimRight(cfg.OpenRouterBaseURL, "/"),
			apiKey:  cfg.OpenRouterAPIKey,
			// One client, one timeout, connection reuse across the pool.
			http:            &http.Client{Timeout: llmRequestTimeout},
			providers:       splitList(os.Getenv("OPENROUTER_PROVIDER_ALLOWLIST")),
			extractionModel: cfg.ExtractionModel,
			embeddingModel:  cfg.EmbeddingModel,
			rerankModel:     cfg.RerankModel,
			embeddingDims:   cfg.EmbeddingDims,
		}
		if gw.apiKey == "" {
			llmLog.Info("OpenRouter not configured — LLM gateway calls will return 503")
		} else {
			llmLog.Infof("LLM gateway ready (providers_allowlisted=%d)", len(gw.providers))
		}
	})
	return gw
}

// UseGateway points the gateway at another OpenAI-compatible endpoint and
// returns the function that restores it. Tests use it to stand a fake
// OpenRouter in front of the real client.
func UseGateway(baseURL, apiKey string) (restore func()) {
	g := llm()
	prev := *g
	g.baseURL, g.apiKey = strings.TrimRight(baseURL, "/"), apiKey
	return func() { *g = prev }
}

// LLMConfigured reports whether the gateway has credentials. Services use it
// for the 503 guard before doing any work.
func LLMConfigured() bool { return llm().apiKey != "" }

// providerRouting is the block every request body carries. data_collection
// "deny" is unconditional: these prompts carry material covered by the secret
// professionnel. An allowlist additionally pins the upstream and switches
// fallback off, so an approved-route deployment cannot drift (§13).
func (g *gateway) providerRouting(override []string) map[string]any {
	routing := map[string]any{"data_collection": "deny"}
	allow := override
	if len(allow) == 0 {
		allow = g.providers
	}
	if len(allow) > 0 {
		routing["only"] = allow
		routing["allow_fallbacks"] = false
	}
	return routing
}

// post sends one JSON request with retry. It returns the raw response body so
// each endpoint can decode its own shape; the body is marshalled once and
// replayed per attempt.
func (g *gateway) post(ctx context.Context, path string, body any) ([]byte, error) {
	if g.apiKey == "" {
		return nil, ErrLLMNotConfigured
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= llmMaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt - 1)):
			}
		}

		raw, err := g.attempt(ctx, path, payload)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if !IsRetryable(err) || ctx.Err() != nil {
			return nil, err
		}
		llmLog.Warnf("Gateway attempt %d/%d failed (path=%s): %v", attempt, llmMaxAttempts, path, err)
	}
	return nil, lastErr
}

func (g *gateway) attempt(ctx context.Context, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.http.Do(req)
	if err != nil {
		// Connection errors and transport timeouts are transient by
		// definition — unless the caller's own context ended the call.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &GatewayError{Status: 0, Message: "connection error", Retryable: true}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &GatewayError{Status: resp.StatusCode, Message: "truncated response body", Retryable: true}
	}
	if resp.StatusCode >= 400 {
		return nil, &GatewayError{
			Status:    resp.StatusCode,
			Message:   providerMessage(raw),
			Retryable: retryableStatus(resp.StatusCode),
		}
	}
	return raw, nil
}

// retryableStatus implements the retry policy of §4.3. 400/422 are
// schema-invalid input, 403 is a policy refusal, 413 is context-too-large,
// 401/404 are configuration: none of them changes with another identical attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// providerMessage extracts the provider's error text without ever echoing the
// request back into a log line.
func providerMessage(raw []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	return "provider error"
}

// backoff is capped exponential with ±25 % jitter, so a provider recovering
// from an outage does not take a synchronised thundering herd (§4.3).
func backoff(n int) time.Duration {
	d := float64(llmBackoffBase) * math.Pow(2, float64(n))
	if d > float64(llmBackoffCap) {
		d = float64(llmBackoffCap)
	}
	return time.Duration(d * (0.75 + 0.5*rand.Float64()))
}

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Chat completions
// ---------------------------------------------------------------------------

// chatResponse is the OpenAI-compatible completion shape, plus OpenRouter's
// `usage.cost` (returned because every request asks for usage:{include:true}).
// Provider-reported cost is what makes the llm_calls record real billing rather
// than a local guess.
type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
			// Annotations carry the url_citation entries an :online model
			// returns. Absent on every ordinary completion.
			Annotations []struct {
				Type        string `json:"type"`
				URLCitation struct {
					URL     string `json:"url"`
					Title   string `json:"title"`
					Content string `json:"content"`
				} `json:"url_citation"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
}

// Chat runs one non-streaming completion.
//
// Non-streaming is deliberate for mission work: a token stream severed by a
// pod restart is not resumable and its usage trailer may never arrive (§4.5).
// The conversational surface streams from the durable message log instead.
func Chat(ctx context.Context, opts ChatOpts) (ChatResult, error) {
	return chatComplete(ctx, opts, opts.messages(), nil)
}

// ChatJSON runs a completion constrained by a SERVER-OWNED JSON schema and
// decodes it into out.
//
// schema is the JSON Schema document (any value that marshals to one). It is
// sent to the provider as response_format, and — because a provider honouring
// it is a courtesy, not a guarantee — the reply is validated against it here
// as well, then strictly decoded into out with unknown fields rejected.
//
// On failure the validator's message is fed back to the model for exactly ONE
// repair round-trip. There is no regex for "the first JSON-looking blob": that
// is the template's demo extractor, and §4.3 rejects it for production because
// it turns a malformed answer into a plausible-looking wrong one.
func ChatJSON(ctx context.Context, opts ChatOpts, schema any, out any) error {
	format, err := jsonSchemaFormat(schema)
	if err != nil {
		return err
	}

	messages := opts.messages()
	res, err := chatComplete(ctx, opts, messages, format)
	if err != nil {
		return err
	}

	validationErr := decodeStrict([]byte(res.Text), schema, out)
	if validationErr == nil {
		return nil
	}
	llmLog.Warnf("Schema validation failed (purpose=%s model=%s) — one repair round-trip", opts.Purpose, res.Model)

	// The repair turn carries the validator's complaint, not a restatement of
	// the task: the model already has the task, what it lacks is the defect.
	messages = append(messages,
		map[string]any{"role": "assistant", "content": res.Text},
		map[string]any{"role": "user", "content": "<error>" + validationErr.Error() + "</error>\n" +
			"<tasks>Corrige la réponse précédente. Respecte exactement le schéma imposé.</tasks>\n" +
			"<output>Un unique document JSON valide, sans texte autour.</output>"},
	)

	repaired, err := chatComplete(ctx, opts, messages, format)
	if err != nil {
		return err
	}
	if err := decodeStrict([]byte(repaired.Text), schema, out); err != nil {
		llmLog.Errorf("Schema validation failed after repair (purpose=%s model=%s): %v", opts.Purpose, repaired.Model, err)
		return fmt.Errorf("%w: %v", ErrSchemaInvalid, err)
	}
	return nil
}

// messages builds the wire messages. The system message is static identity;
// everything variable is in the user turn.
func (o ChatOpts) messages() []map[string]any {
	msgs := make([]map[string]any, 0, 2)
	if o.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": o.System})
	}
	if len(o.Parts) == 0 {
		msgs = append(msgs, map[string]any{"role": "user", "content": o.User})
		return msgs
	}

	content := []map[string]any{{"type": "text", "text": o.User}}
	for _, p := range o.Parts {
		switch p.Kind {
		case "image":
			content = append(content, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": p.URL},
			})
		case "file":
			content = append(content, map[string]any{
				"type": "file",
				"file": map[string]any{"filename": p.Filename, "file_data": p.FileData},
			})
		}
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": content})
	return msgs
}

func chatComplete(ctx context.Context, opts ChatOpts, messages []map[string]any, format map[string]any) (ChatResult, error) {
	g := llm()
	model := opts.Model
	if model == "" {
		model = g.extractionModel
	}

	body := map[string]any{
		"model":    model,
		"messages": messages,
		"provider": g.providerRouting(opts.Providers),
		// Ask for the usage trailer explicitly: without it there is no
		// provider-reported cost and every recorded cost would be a guess.
		"usage": map[string]any{"include": true},
	}
	if opts.MaxTokens > 0 {
		body["max_tokens"] = opts.MaxTokens
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if opts.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": opts.ReasoningEffort}
	}
	if format != nil {
		body["response_format"] = format
	}

	purpose := opts.Purpose
	if purpose == "" {
		purpose = types.LLMPurposeChat
	}
	ref := CallRef{TenantID: opts.TenantID, RunID: opts.RunID, RunStepID: opts.RunStepID, StepAttemptID: opts.StepAttemptID}
	if (ref == CallRef{}) {
		ref = CallRefFrom(ctx)
	}

	// The execution id is minted BEFORE dispatch and the row is written
	// first, so a call the provider accepted is never invisible to the audit
	// trail — even if this process dies before the response arrives (§4.6).
	executionID, callID := beginLLMCall(model, purpose, ref)

	llmLog.Debugf("Chat dispatch (model=%s purpose=%s messages=%d parts=%d)", model, purpose, len(messages), len(opts.Parts))
	raw, err := g.post(ctx, "/chat/completions", body)
	if err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, purpose)
		return ChatResult{ExecutionID: executionID}, err
	}

	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		finishLLMCall(callID, "", types.LLMCallStateFailed, llmUsage{}, model, purpose)
		return ChatResult{ExecutionID: executionID}, fmt.Errorf("decode completion: %w", err)
	}

	usage := usageFrom(resp.Usage)
	result := ChatResult{
		Model:             resp.Model,
		ExecutionID:       executionID,
		ProviderRequestID: resp.ID,
		PromptTokens:      usage.PromptTokens,
		CompletionTokens:  usage.CompletionTokens,
		CostUSD:           usage.CostUSD,
		UsageStatus:       usage.status(),
	}
	if len(resp.Choices) > 0 {
		result.Text = resp.Choices[0].Message.Content
		result.FinishReason = resp.Choices[0].FinishReason
		for _, a := range resp.Choices[0].Message.Annotations {
			if a.Type != "url_citation" || a.URLCitation.URL == "" {
				continue
			}
			result.Citations = append(result.Citations, WebCitation{
				URL:     a.URLCitation.URL,
				Title:   a.URLCitation.Title,
				Content: a.URLCitation.Content,
			})
		}
	}

	// A refusal or a truncation is a completed, billable call that produced
	// no usable output: record it as succeeded (the money was spent) and
	// return a non-retryable error to the caller.
	state := types.LLMCallStateSucceeded
	finishLLMCall(callID, resp.ID, state, usage, model, purpose)
	llmLog.Infof("Chat completed (model=%s purpose=%s finish=%s prompt_tokens=%d completion_tokens=%d usage=%s)",
		result.Model, purpose, result.FinishReason, result.PromptTokens, result.CompletionTokens, result.UsageStatus)

	var unusable error
	switch {
	case len(resp.Choices) > 0 && resp.Choices[0].Message.Refusal != "":
		unusable = ErrRefused
	case result.FinishReason == "content_filter":
		unusable = ErrRefused
	case result.FinishReason == "length":
		unusable = ErrTruncated
	case strings.TrimSpace(result.Text) == "":
		unusable = ErrEmptyCompletion
	}
	if unusable != nil {
		// Which of the three it was is what makes a failed step debuggable;
		// the answer itself is never logged.
		llmLog.Warnf("Chat produced no usable answer (model=%s purpose=%s finish=%s reason=%v)",
			result.Model, purpose, result.FinishReason, unusable)
		return result, unusable
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Server-owned schema validation
// ---------------------------------------------------------------------------

func jsonSchemaFormat(schema any) (map[string]any, error) {
	doc, err := schemaMap(schema)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "nomos_output",
			"strict": true,
			"schema": doc,
		},
	}, nil
}

// schemaMap normalises a schema given as a Go struct, a map or raw JSON.
func schemaMap(schema any) (map[string]any, error) {
	if m, ok := schema.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("schema is not a JSON object: %w", err)
	}
	return m, nil
}

// decodeStrict validates raw against the schema, then decodes it into out.
//
// Two passes on purpose. The schema pass catches what a Go decode cannot: a
// missing required field is indistinguishable from a zero value once it is in
// a struct, and that is exactly the failure that puts an empty date on a piece
// in a bordereau. The decode pass then rejects unknown fields and trailing
// content, so a model that appends prose to valid JSON fails loudly.
func decodeStrict(raw []byte, schema any, out any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("empty response")
	}

	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("response is not valid JSON: %v", err)
	}
	if dec.More() {
		return errors.New("response carries trailing content after the JSON document")
	}

	doc, err := schemaMap(schema)
	if err != nil {
		return err
	}
	if err := validateNode("$", value, doc); err != nil {
		return err
	}

	dec = json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("response does not fit the expected structure: %v", err)
	}
	return nil
}

// validateNode checks one value against the JSON Schema subset Nomos uses:
// type, required, properties, additionalProperties, items and enum. That is
// the whole vocabulary the server-owned schemas need; anything richer would be
// a schema the provider's strict mode would reject anyway.
func validateNode(path string, value any, schema map[string]any) error {
	if enum, ok := schema["enum"].([]any); ok {
		for _, allowed := range enum {
			if fmt.Sprint(allowed) == fmt.Sprint(value) {
				return nil
			}
		}
		return fmt.Errorf("%s: value is not one of the allowed values", path)
	}

	kind, _ := schema["type"].(string)
	switch kind {
	case "", "null":
		return nil

	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected an object, got %s", path, jsonKind(value))
		}
		props, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, r := range required {
				name := fmt.Sprint(r)
				if _, present := obj[name]; !present {
					return fmt.Errorf("%s: required property %q is missing", path, name)
				}
			}
		}
		additional, hasAdditional := schema["additionalProperties"].(bool)
		for name, v := range obj {
			sub, known := props[name].(map[string]any)
			if !known {
				if hasAdditional && !additional {
					return fmt.Errorf("%s: unknown property %q", path, name)
				}
				continue
			}
			if err := validateNode(path+"."+name, v, sub); err != nil {
				return err
			}
		}
		return nil

	case "array":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: expected an array, got %s", path, jsonKind(value))
		}
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return nil
		}
		for i, v := range arr {
			if err := validateNode(fmt.Sprintf("%s[%d]", path, i), v, items); err != nil {
				return err
			}
		}
		return nil

	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected a string, got %s", path, jsonKind(value))
		}
		return nil

	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: expected a number, got %s", path, jsonKind(value))
		}
		return nil

	case "integer":
		n, ok := value.(float64)
		if !ok {
			return fmt.Errorf("%s: expected an integer, got %s", path, jsonKind(value))
		}
		if n != math.Trunc(n) {
			return fmt.Errorf("%s: expected an integer, got a fractional number", path)
		}
		return nil

	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected a boolean, got %s", path, jsonKind(value))
		}
		return nil
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "an unexpected value"
}

// ---------------------------------------------------------------------------
// The cost path (§4.6)
// ---------------------------------------------------------------------------

// CallRef is where a gateway call sits in the run tree. Everything is optional
// — an ad-hoc conversational reply has no step — but the tenant always travels
// with the row, because cost is attributed per firm.
type CallRef struct {
	TenantID      string
	RunID         string
	RunStepID     string
	StepAttemptID string
}

type callRefKey struct{}

// WithCallRef attaches the run coordinates to a context, so Embed and Rerank —
// whose signatures are deliberately narrow — still produce attributable
// llm_calls rows. ChatOpts' own fields win when both are set.
func WithCallRef(ctx context.Context, ref CallRef) context.Context {
	return context.WithValue(ctx, callRefKey{}, ref)
}

// CallRefFrom returns the coordinates attached by WithCallRef, or the zero
// value for a call made outside a run.
func CallRefFrom(ctx context.Context) CallRef {
	if ref, ok := ctx.Value(callRefKey{}).(CallRef); ok {
		return ref
	}
	return CallRef{}
}

// llmUsage is the normalised usage trailer. Known is false when the provider
// returned nothing — a dead connection, a missing trailer — and that stays
// `unknown` forever unless a reconciliation job fills it in. We never invent a
// character-count estimate: that is not auditable billing (§4.6).
type llmUsage struct {
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	Known            bool
}

func (u llmUsage) status() string {
	if u.Known {
		return types.UsageStatusKnown
	}
	return types.UsageStatusUnknown
}

func usageFrom(u *struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
}) llmUsage {
	if u == nil {
		return llmUsage{}
	}
	return llmUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CostUSD:          u.Cost,
		Known:            true,
	}
}

// callAttribution is what an agent dispatch adds to its llm_calls row and its
// cost record: the acting user, the controller
// dispatch, the exact input — and, once the provider answered, the model it
// actually ran and the counters it returned. Legacy callers pass none.
type callAttribution struct {
	ActorID             string
	AgentDispatch       int
	InputManifestSHA256 string
	ReturnedModel       string
	// UsageDetails lands in llm_calls.usage_details.
	UsageDetails map[string]any
}

// newLLMCallRow builds a dispatched llm_calls row with fresh ids.
func newLLMCallRow(model, purpose string, ref CallRef, extra ...callAttribution) types.LLMCall {
	row := types.LLMCall{
		ID:            uuid.NewString(),
		TenantID:      ref.TenantID,
		ExecutionID:   uuid.NewString(),
		Model:         model,
		Purpose:       purpose,
		State:         types.LLMCallStateDispatched,
		UsageStatus:   types.UsageStatusUnknown,
		RunID:         nullable(ref.RunID),
		RunStepID:     nullable(ref.RunStepID),
		StepAttemptID: nullable(ref.StepAttemptID),
	}
	for _, a := range extra {
		row.ActorID = a.ActorID
		row.InputManifestSHA256 = a.InputManifestSHA256
		if a.AgentDispatch > 0 {
			n := a.AgentDispatch
			row.AgentDispatch = &n
		}
	}
	return row
}

// errLLMCallNotRecorded means there is no llm_calls row to reconcile.
var errLLMCallNotRecorded = errors.New("llm call is not recorded")

// beginLLMCall mints the execution id and persists the dispatched row before
// the request leaves the process. It returns the execution id and the row id;
// the row id is empty when Postgres is not configured, which downgrades the
// cost path to "not recorded" rather than blocking inference — the same
// optional-integration contract as everywhere else. A caller that must not
// dispatch unrecorded (the agent) treats the empty id as a refusal.
func beginLLMCall(model, purpose string, ref CallRef, extra ...callAttribution) (executionID, callID string) {
	row := newLLMCallRow(model, purpose, ref, extra...)
	executionID = row.ExecutionID
	if DB == nil {
		return executionID, ""
	}

	if err := DB.Create(&row).Error; err != nil {
		// Losing the record is bad; refusing to work because the audit
		// table is unavailable is worse for a run already in flight. Say it
		// loudly and continue — reconciliation is by execution id.
		llmLog.Errorf("Could not record dispatched llm_call (model=%s purpose=%s): %v", model, purpose, err)
		return executionID, ""
	}
	return executionID, row.ID
}

// finishLLMCall reconciles the row with the usage and, when usage is known,
// the provider-reported cost. Missing usage leaves usage_status=unknown and the
// cost at 0 — there is nothing auditable to record.
//
// The error is returned for callers that must not treat an unreconciled call
// as recorded (the agent); legacy callers keep ignoring it, as before.
func finishLLMCall(callID, providerRequestID, state string, usage llmUsage, model, purpose string,
	extra ...callAttribution) error {
	if DB == nil || callID == "" {
		return errLLMCallNotRecorded
	}
	// A disconnected client must not abandon a half-written cost record: the
	// call already happened and the money is already spent (§9 "reconciles an
	// already-committed side effect").
	ctx := context.Background()
	now := time.Now().Unix()

	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"state":             state,
			"usage_status":      usage.status(),
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"cost_nano_usd":     NanoUSD(usage.CostUSD),
			"finished_at":       now,
		}
		if providerRequestID != "" {
			updates["provider_request_id"] = providerRequestID
		}
		for _, a := range extra {
			if a.ReturnedModel != "" {
				updates["returned_model"] = a.ReturnedModel
			}
			if len(a.UsageDetails) > 0 {
				raw, err := json.Marshal(a.UsageDetails)
				if err != nil {
					return err
				}
				updates["usage_details"] = types.JSONB(raw)
			}
		}
		res := tx.Model(&types.LLMCall{}).Where("id = ?", callID).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if len(extra) > 0 && res.RowsAffected == 0 {
			// An attributed call must reconcile the row it reserved.
			return errLLMCallNotRecorded
		}
		return nil
	})
	if err != nil {
		llmLog.Errorf("Could not reconcile llm_call (model=%s purpose=%s): %v", model, purpose, err)
	}
	return err
}

// NanoUSD converts a provider-reported USD cost to the integer billionths
// llm_calls stores — fine enough for a single embedding call.
func NanoUSD(usd float64) int64 { return int64(math.Round(usd * 1e9)) }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
