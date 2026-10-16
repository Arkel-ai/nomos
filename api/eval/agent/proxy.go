package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
)

// Proxy is the evaluation's window on the gateway. Every model call the API
// makes — controller dispatches, reviews, qualification, OCR, embeddings,
// rerank and the isolated web search — goes through it on its way to the real
// OpenRouter, because the runner points OPENROUTER_BASE_URL at it before any
// client is built. It forwards bodies unchanged (production provider and
// reasoning settings stay exactly as the API wrote them) and records, per
// trial, the model each response says actually ran.
//
// For agent cases it also scripts controller dispatches (Fault): the
// dispatch is answered here and never reaches the provider. Scripted
// dispatches are recorded as such and excluded from the model check.
type Proxy struct {
	upstream string
	apiKey   string
	srv      *http.Server
	ln       net.Listener
	client   *http.Client

	mu  sync.Mutex
	tap *trialTap
	// inflight counts the requests being handled; fenced answers every new
	// one at once (Fence).
	inflight int
	fenced   bool
	// outside are the calls made with no trial running: ingestion,
	// qualification and corpus seeding. Their models are judged too.
	outside []CallRecord
}

// CallRecord is one HTTP exchange the proxy saw.
type CallRecord struct {
	Path           string  `json:"path"`
	RequestedModel string  `json:"requested_model"`
	ReturnedModel  string  `json:"returned_model"`
	Controller     bool    `json:"controller"`
	Dispatch       int     `json:"dispatch,omitempty"`
	Status         int     `json:"status"`
	Scripted       bool    `json:"scripted,omitempty"`
	Fault          string  `json:"fault,omitempty"`
	UsageKnown     bool    `json:"usage_known"`
	CostUSD        float64 `json:"cost_usd"`
	PromptSHA256   string  `json:"prompt_sha256,omitempty"` // controller system prompt
	DurationMS     int64   `json:"duration_ms"`
	// WebQuery is the text of an isolated web-search request — what left
	// for the open web — as forwarded.
	WebQuery string `json:"-"`
}

type trialTap struct {
	faults   []Fault
	dispatch int
	records  []CallRecord
}

// StartProxy listens on a loopback port and forwards to upstream.
func StartProxy(upstream, apiKey string) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		upstream: strings.TrimRight(upstream, "/"),
		apiKey:   apiKey,
		ln:       ln,
		// Longer than the gateway's own 5-minute attempt timeout: the API's
		// client is the one that decides when a call has taken too long.
		client: &http.Client{Timeout: 6 * time.Minute},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the base URL the API must use instead of the upstream.
func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

// Close stops the listener.
func (p *Proxy) Close() { _ = p.srv.Close() }

// BeginTrial starts recording for one trial and arms its faults.
func (p *Proxy) BeginTrial(faults []Fault) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tap = &trialTap{faults: faults}
}

// EndTrial stops recording and returns what the trial's calls were.
func (p *Proxy) EndTrial() []CallRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tap == nil {
		return nil
	}
	out := p.tap.records
	p.tap = nil
	return out
}

// FaultFence marks a request the proxy answered itself while fenced.
const FaultFence = "fence"

// Fence, while on, answers every new request at once with a 503 — a status
// the API records as a failed, unbilled call — instead of forwarding it. A
// scripted interruption (restart, cancel) raises it and waits for
// InFlight to drain, so it never cuts a provider call whose spend would then
// be unknown.
func (p *Proxy) Fence(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fenced = on
}

// InFlight is the number of requests the proxy is handling.
func (p *Proxy) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight
}

// TakeOutside returns, and forgets, the calls made outside any trial.
func (p *Proxy) TakeOutside() []CallRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.outside
	p.outside = nil
	return out
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight--
		p.mu.Unlock()
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)
	rec := CallRecord{Path: r.URL.Path, RequestedModel: req.Model}
	web := false
	for _, t := range req.Tools {
		switch {
		case t.Type == "function":
			rec.Controller = true
		case strings.HasPrefix(t.Type, "openrouter:web_search"):
			web = true
		}
	}
	if rec.Controller && len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		if s, ok := req.Messages[0].Content.(string); ok {
			rec.PromptSHA256 = sha256Hex([]byte(s))
		}
	}

	var fault *Fault
	p.mu.Lock()
	fenced := p.fenced
	if p.tap != nil && rec.Controller {
		p.tap.dispatch++
		rec.Dispatch = p.tap.dispatch
		for i := range p.tap.faults {
			if p.tap.faults[i].applies(rec.Dispatch) {
				f := p.tap.faults[i]
				fault = &f
				break
			}
		}
	}
	p.mu.Unlock()

	status, respBody := 0, []byte(nil)
	switch {
	case fenced:
		rec.Scripted, rec.Fault = true, FaultFence
		status = http.StatusServiceUnavailable
		respBody = []byte(`{"error":{"message":"evaluation interruption in progress (proxy fence)","code":503}}`)
	case fault != nil && (fault.Kind == FaultScriptedCall || fault.Kind == FaultMalformed):
		rec.Scripted, rec.Fault = true, fault.Kind
		status, respBody = http.StatusOK, scriptedCompletion(req.Model, rec.Dispatch, *fault)
	case fault != nil && fault.Kind == FaultHTTPError:
		rec.Scripted, rec.Fault = true, fault.Kind
		status = fault.Status
		respBody = []byte(`{"error":{"message":"injected provider failure (evaluation fault)","code":` + fmt.Sprint(fault.Status) + `}}`)
	default:
		status, respBody, err = p.forward(r, body)
		if err != nil {
			status, respBody = http.StatusBadGateway, []byte(`{"error":{"message":"evaluation proxy could not reach the gateway"}}`)
		}
	}
	rec.Status = status
	if web && !rec.Scripted {
		var parts []string
		for _, m := range req.Messages {
			if s, ok := m.Content.(string); ok && m.Role != "system" {
				parts = append(parts, s)
			}
		}
		rec.WebQuery = strings.Join(parts, "\n")
	}
	if status >= 200 && status < 300 {
		rec.ReturnedModel, rec.UsageKnown, rec.CostUSD = responseFacts(respBody)
	}
	rec.DurationMS = time.Since(start).Milliseconds()

	p.mu.Lock()
	if p.tap != nil {
		p.tap.records = append(p.tap.records, rec)
	} else {
		p.outside = append(p.outside, rec)
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// forward replays the request upstream with the same method, path, query and
// body. The API's own Authorization header is kept: the proxy never needs to
// know the key, except to answer for a client that sent none.
func (p *Proxy) forward(r *http.Request, body []byte) (int, []byte, error) {
	target := p.upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for _, h := range []string{"Content-Type", "Authorization", "HTTP-Referer", "X-Title"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("Authorization") == "" && p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// responseFacts reads the model the provider says it ran and its usage.
func responseFacts(raw []byte) (model string, usageKnown bool, cost float64) {
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", false, 0
	}
	if resp.Usage != nil {
		return resp.Model, true, resp.Usage.Cost
	}
	return resp.Model, false, 0
}

// scriptedCompletion answers a controller dispatch with one native tool call
// (or a malformed one), without the provider. It carries the requested model
// and a known, zero usage: nothing was billed.
func scriptedCompletion(model string, dispatch int, f Fault) []byte {
	name, args := f.Tool, string(f.Arguments)
	if f.Kind == FaultMalformed && f.Raw != "" {
		args = f.Raw
	}
	if name == "" {
		name = types.ToolListPieces
	}
	raw, _ := json.Marshal(map[string]any{
		"id":    "gen-eval-" + uuid.NewString()[:8],
		"model": model,
		"choices": []map[string]any{{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   fmt.Sprintf("call_eval_%d", dispatch),
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "cost": 0},
	})
	return raw
}
