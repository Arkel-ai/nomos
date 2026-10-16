package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
)

// ---------------------------------------------------------------------------
// Release configuration (-config)
// ---------------------------------------------------------------------------

// ReleaseConfig is the reviewed, non-secret effective configuration a
// candidate is evaluated under — and the only one a passing report can
// authorise. It names models, not keys: credentials come from the
// execution environment.
type ReleaseConfig struct {
	Name       string   `json:"name"`
	TariffDate string   `json:"tariff_date"`
	Models     ModelSet `json:"models"`
	// ApprovedReturnedModels lists, per requested model, the ids a provider
	// may answer with (dated snapshots). The requested id itself is always
	// approved; anything else is a substitution and fails the gate.
	ApprovedReturnedModels map[string][]string `json:"approved_returned_models"`
	ApprovedDomains        []string            `json:"approved_domains"`
	ProviderAllowlist      []string            `json:"provider_allowlist"`
	StepConcurrency        Concurrency         `json:"step_concurrency"`
	Connectors             ConnectorSet        `json:"connectors"`
	// Tariffs is the dated price snapshot preflight compares the gateway's
	// current prices with, in USD per million tokens.
	Tariffs map[string]Tariff `json:"tariffs"`
}

// ModelSet is every model the API calls. WebSearch empty means the variable
// must be unset (owner decision 6: the web adapter must not rely on the
// legacy :online default).
type ModelSet struct {
	Controller string `json:"controller"`
	Review     string `json:"review"`
	Extraction string `json:"extraction"`
	Vision     string `json:"vision"`
	Embedding  string `json:"embedding"`
	Rerank     string `json:"rerank"`
	WebSearch  string `json:"web_search"`
}

func (m ModelSet) all() []string {
	out := []string{}
	for _, v := range []string{m.Controller, m.Review, m.Extraction, m.Vision, m.Embedding, m.Rerank, m.WebSearch} {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Concurrency is the runner pools (owner decision 6: LLM=4 DB=8 OCR=1 RENDER=1).
type Concurrency struct {
	LLM    int `json:"llm"`
	DB     int `json:"db"`
	OCR    int `json:"ocr"`
	Render int `json:"render"`
}

// ConnectorSet is the PISTE environment (sandbox or production).
type ConnectorSet struct {
	PisteTokenURL     string `json:"piste_token_url"`
	LegifranceBaseURL string `json:"legifrance_base_url"`
	JudilibreBaseURL  string `json:"judilibre_base_url"`
}

// Tariff is a price in USD per million tokens.
type Tariff struct {
	PromptUSDPerMTok     float64 `json:"prompt_usd_per_mtok"`
	CompletionUSDPerMTok float64 `json:"completion_usd_per_mtok"`
}

// LoadReleaseConfig reads -config strictly and returns it with the hash of
// its canonical form.
func LoadReleaseConfig(path string) (ReleaseConfig, string, error) {
	var rc ReleaseConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return rc, "", fmt.Errorf("release config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rc); err != nil {
		return rc, "", fmt.Errorf("release config %s: %w", path, err)
	}
	var problems []string
	if rc.Name == "" || rc.TariffDate == "" {
		problems = append(problems, "name and tariff_date are required")
	}
	if _, err := time.Parse("2006-01-02", rc.TariffDate); rc.TariffDate != "" && err != nil {
		problems = append(problems, "tariff_date is not YYYY-MM-DD")
	}
	m := rc.Models
	if m.Controller == "" || m.Review == "" || m.Extraction == "" || m.Vision == "" || m.Embedding == "" || m.Rerank == "" {
		problems = append(problems, "every model but web_search is required")
	}
	if strings.HasSuffix(m.WebSearch, ":online") {
		problems = append(problems, "web_search must not be an :online model (owner decision 6)")
	}
	if len(rc.ApprovedDomains) == 0 {
		problems = append(problems, "approved_domains is required")
	}
	if c := rc.StepConcurrency; c.LLM <= 0 || c.DB <= 0 || c.OCR <= 0 || c.Render <= 0 {
		problems = append(problems, "step_concurrency needs all four pools")
	}
	if len(problems) > 0 {
		return rc, "", errors.New("release config is invalid: " + strings.Join(problems, "; "))
	}
	return rc, canonicalSHA256(rc), nil
}

// ApprovedReturn reports whether a provider answering `returned` for a
// request of `requested` is approved. The requested id without its vendor
// prefix is the same model (the embeddings endpoint answers
// "text-embedding-3-small" for "openai/text-embedding-3-small"); any other
// name needs an approved alias.
func (rc ReleaseConfig) ApprovedReturn(requested, returned string) bool {
	_, bare, prefixed := strings.Cut(requested, "/")
	return returned == requested || (prefixed && returned == bare) ||
		slices.Contains(rc.ApprovedReturnedModels[requested], returned)
}

// CheckProduction compares the execution environment with the release
// config: models, web model, provider allowlist, pools and connectors. Every
// mismatch is named; -require-models refuses to spend anything if one exists.
func CheckProduction(cfg config.Config, rc ReleaseConfig) []string {
	var out []string
	cmp := func(name, have, want string) {
		if have != want {
			out = append(out, fmt.Sprintf("%s is %q, the release config says %q", name, have, want))
		}
	}
	cmp("PLANNER_MODEL", cfg.PlannerModel, rc.Models.Controller)
	cmp("DRAFTER_MODEL", cfg.DrafterModel, rc.Models.Review)
	cmp("EXTRACTION_MODEL", cfg.ExtractionModel, rc.Models.Extraction)
	cmp("VISION_MODEL", cfg.VisionModel, rc.Models.Vision)
	cmp("EMBEDDING_MODEL", cfg.EmbeddingModel, rc.Models.Embedding)
	cmp("RERANK_MODEL", cfg.RerankModel, rc.Models.Rerank)
	cmp("WEB_SEARCH_MODEL", strings.TrimSpace(os.Getenv("WEB_SEARCH_MODEL")), rc.Models.WebSearch)
	cmp("OPENROUTER_PROVIDER_ALLOWLIST", strings.Join(splitList(os.Getenv("OPENROUTER_PROVIDER_ALLOWLIST")), ","), strings.Join(rc.ProviderAllowlist, ","))
	cmpInt := func(name string, have, want int) {
		if have != want {
			out = append(out, fmt.Sprintf("%s is %d, the release config says %d", name, have, want))
		}
	}
	cmpInt("STEP_CONCURRENCY_LLM", cfg.StepConcurrencyLLM, rc.StepConcurrency.LLM)
	cmpInt("STEP_CONCURRENCY_DB", cfg.StepConcurrencyDB, rc.StepConcurrency.DB)
	cmpInt("STEP_CONCURRENCY_OCR", cfg.StepConcurrencyOCR, rc.StepConcurrency.OCR)
	cmpInt("STEP_CONCURRENCY_RENDER", cfg.StepConcurrencyRender, rc.StepConcurrency.Render)
	cmp("PISTE_TOKEN_URL", cfg.PisteTokenURL, rc.Connectors.PisteTokenURL)
	cmp("LEGIFRANCE_BASE_URL", cfg.LegifranceBaseURL, rc.Connectors.LegifranceBaseURL)
	cmp("JUDILIBRE_BASE_URL", cfg.JudilibreBaseURL, rc.Connectors.JudilibreBaseURL)
	return out
}

// EffectiveConfig is the non-secret configuration the run actually had.
func EffectiveConfig(cfg config.Config, upstream string) map[string]string {
	return map[string]string{
		"planner_model":           cfg.PlannerModel,
		"drafter_model":           cfg.DrafterModel,
		"extraction_model":        cfg.ExtractionModel,
		"vision_model":            cfg.VisionModel,
		"embedding_model":         cfg.EmbeddingModel,
		"embedding_dims":          strconv.Itoa(cfg.EmbeddingDims),
		"rerank_model":            cfg.RerankModel,
		"web_search_model":        strings.TrimSpace(os.Getenv("WEB_SEARCH_MODEL")),
		"gateway":                 upstream,
		"provider_allowlist":      strings.Join(splitList(os.Getenv("OPENROUTER_PROVIDER_ALLOWLIST")), ","),
		"vision_ocr_enabled":      strconv.FormatBool(cfg.VisionOCREnabled),
		"step_concurrency":        fmt.Sprintf("llm=%d db=%d ocr=%d render=%d", cfg.StepConcurrencyLLM, cfg.StepConcurrencyDB, cfg.StepConcurrencyOCR, cfg.StepConcurrencyRender),
		"piste_token_url":         cfg.PisteTokenURL,
		"legifrance_base_url":     cfg.LegifranceBaseURL,
		"judilibre_base_url":      cfg.JudilibreBaseURL,
		"piste_configured":        strconv.FormatBool(cfg.PisteClientID != "" && cfg.PisteClientSecret != ""),
		"corpus_autoseed_sources": strings.Join(cfg.CorpusAutoseedSources, ","),
	}
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

// CheckTariffs compares the gateway's current model list with the release
// config's dated price snapshot. The chat models must be listed; a price
// that moved is a drift the owner re-approves, never a silent change.
func CheckTariffs(ctx context.Context, baseURL, apiKey string, rc ReleaseConfig) (listed map[string]Tariff, problems []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("model list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("model list: status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, fmt.Errorf("model list: %w", err)
	}
	listed = map[string]Tariff{}
	for _, m := range body.Data {
		p, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		c, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		listed[m.ID] = Tariff{PromptUSDPerMTok: p * 1e6, CompletionUSDPerMTok: c * 1e6}
	}
	chat := ModelSet{Controller: rc.Models.Controller, Review: rc.Models.Review, Extraction: rc.Models.Extraction, Vision: rc.Models.Vision}
	for _, id := range chat.all() {
		got, ok := listed[id]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is not offered by the gateway", id))
			continue
		}
		want, pinned := rc.Tariffs[id]
		switch {
		case !pinned:
			problems = append(problems, fmt.Sprintf("%s has no dated tariff in the release config (gateway: $%.4f / $%.4f per Mtok)",
				id, got.PromptUSDPerMTok, got.CompletionUSDPerMTok))
		case !samePrice(got.PromptUSDPerMTok, want.PromptUSDPerMTok) || !samePrice(got.CompletionUSDPerMTok, want.CompletionUSDPerMTok):
			problems = append(problems, fmt.Sprintf("%s price drifted since %s: $%.4f / $%.4f per Mtok, snapshot $%.4f / $%.4f",
				id, rc.TariffDate, got.PromptUSDPerMTok, got.CompletionUSDPerMTok, want.PromptUSDPerMTok, want.CompletionUSDPerMTok))
		}
	}
	return listed, problems, nil
}

func samePrice(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// ---------------------------------------------------------------------------
// Usage and cost, from the product's own durable accounting
// ---------------------------------------------------------------------------

// PurposeUsage is the usage of one call purpose (agent, draft, qualify, …).
type PurposeUsage struct {
	Calls            int     `json:"calls"`
	Succeeded        int     `json:"succeeded"`
	Failed           int     `json:"failed"`
	Unreconciled     int     `json:"unreconciled"`
	UnknownUsage     int     `json:"unknown_usage"`
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

func (p *PurposeUsage) add(o PurposeUsage) {
	p.Calls += o.Calls
	p.Succeeded += o.Succeeded
	p.Failed += o.Failed
	p.Unreconciled += o.Unreconciled
	p.UnknownUsage += o.UnknownUsage
	p.PromptTokens += o.PromptTokens
	p.CachedTokens += o.CachedTokens
	p.ReasoningTokens += o.ReasoningTokens
	p.CompletionTokens += o.CompletionTokens
	p.CostUSD += o.CostUSD
}

// Usage is the accounted usage of a set of llm_calls rows.
type Usage struct {
	PurposeUsage
	ByPurpose   map[string]*PurposeUsage `json:"by_purpose"`
	ServerTools map[string]float64       `json:"server_tools,omitempty"`
	// Models counts calls by requested model; ReturnedModels by the model
	// the provider recorded as having run (agent dispatches record it).
	Models         map[string]int `json:"models"`
	ReturnedModels map[string]int `json:"returned_models,omitempty"`
	// UnknownDispatches are controller dispatches whose provider outcome is
	// unknown (no status: the request may have been accepted).
	UnknownDispatches int `json:"unknown_dispatches"`
}

// Add merges another usage into u.
func (us *Usage) Add(o Usage) {
	us.PurposeUsage.add(o.PurposeUsage)
	us.UnknownDispatches += o.UnknownDispatches
	for k, v := range o.ByPurpose {
		if us.ByPurpose == nil {
			us.ByPurpose = map[string]*PurposeUsage{}
		}
		if us.ByPurpose[k] == nil {
			us.ByPurpose[k] = &PurposeUsage{}
		}
		us.ByPurpose[k].add(*v)
	}
	for k, v := range o.ServerTools {
		if us.ServerTools == nil {
			us.ServerTools = map[string]float64{}
		}
		us.ServerTools[k] += v
	}
	for k, v := range o.Models {
		if us.Models == nil {
			us.Models = map[string]int{}
		}
		us.Models[k] += v
	}
	for k, v := range o.ReturnedModels {
		if us.ReturnedModels == nil {
			us.ReturnedModels = map[string]int{}
		}
		us.ReturnedModels[k] += v
	}
}

type usageRow struct {
	Purpose          string
	Model            string
	ReturnedModel    string
	State            string
	UsageStatus      string
	PromptTokens     int
	CompletionTokens int
	CostNanoUSD      int64
	UsageDetails     types.JSONB
}

// CollectUsage sums the llm_calls rows the filter selects, with the billed
// cost the provider reported — not an estimate. A call whose usage never arrived stays `unknown` and is
// counted as such; it is never priced at zero.
func CollectUsage(db *gorm.DB, filter string, args ...any) (Usage, error) {
	var rows []usageRow
	err := db.Table("llm_calls l").
		Select("l.purpose, l.model, l.returned_model, l.state, l.usage_status, l.prompt_tokens, l.completion_tokens, l.cost_nano_usd, l.usage_details").
		Where(filter, args...).Scan(&rows).Error
	if err != nil {
		return Usage{}, err
	}
	us := Usage{ByPurpose: map[string]*PurposeUsage{}, Models: map[string]int{}}
	for _, r := range rows {
		var pu PurposeUsage
		pu.Calls = 1
		var details struct {
			Outcome         string         `json:"outcome"`
			CachedTokens    int            `json:"cached_tokens"`
			ReasoningTokens int            `json:"reasoning_tokens"`
			ServerToolUse   map[string]any `json:"server_tool_use"`
		}
		if len(r.UsageDetails) > 0 {
			_ = json.Unmarshal(r.UsageDetails, &details)
		}
		switch r.State {
		case types.LLMCallStateSucceeded:
			pu.Succeeded = 1
		case types.LLMCallStateFailed:
			pu.Failed = 1
		default:
			pu.Unreconciled = 1
		}
		// A call the provider refused with a status was not billed; every
		// other call without usage — a dead connection, a missing trailer, a
		// row never reconciled — has an unknown cost.
		if r.UsageStatus != types.UsageStatusKnown && !(r.State == types.LLMCallStateFailed && details.Outcome == "failed") {
			pu.UnknownUsage = 1
		}
		if r.Purpose == "agent" && details.Outcome == "unknown" {
			us.UnknownDispatches++
		}
		pu.PromptTokens = r.PromptTokens
		pu.CompletionTokens = r.CompletionTokens
		pu.CachedTokens = details.CachedTokens
		pu.ReasoningTokens = details.ReasoningTokens
		pu.CostUSD = float64(r.CostNanoUSD) / 1e9
		for k, v := range details.ServerToolUse {
			if n, ok := v.(float64); ok {
				if us.ServerTools == nil {
					us.ServerTools = map[string]float64{}
				}
				us.ServerTools[k] += n
			}
		}
		us.PurposeUsage.add(pu)
		if us.ByPurpose[r.Purpose] == nil {
			us.ByPurpose[r.Purpose] = &PurposeUsage{}
		}
		us.ByPurpose[r.Purpose].add(pu)
		us.Models[r.Model]++
		if r.ReturnedModel != "" {
			if us.ReturnedModels == nil {
				us.ReturnedModels = map[string]int{}
			}
			us.ReturnedModels[r.ReturnedModel]++
		}
	}
	return us, nil
}

// ---------------------------------------------------------------------------
// Budget
// ---------------------------------------------------------------------------

// Budget is the suite's stopping limit ($25 fast, $100 full). It is
// not a price estimate: when the known spend reaches it the current trial is
// cancelled and the suite stops as incomplete_budget, with every planned
// trial still in the denominator.
type Budget struct {
	LimitUSD float64
	db       *gorm.DB
	tenantID string
	since    int64
}

// NewBudget meters the evaluation tenant's calls and the unattributed ones
// (corpus seeding) made since the suite started.
func NewBudget(db *gorm.DB, tenantID string, limitUSD float64, since time.Time) *Budget {
	return &Budget{LimitUSD: limitUSD, db: db, tenantID: tenantID, since: since.Unix()}
}

// Usage is everything the suite has spent so far.
func (b *Budget) Usage() (Usage, error) {
	return CollectUsage(b.db, "(l.tenant_id = ? OR (l.tenant_id = '' AND l.started_at >= ?))", b.tenantID, b.since)
}

// Exhausted reports whether the known spend has reached the limit.
func (b *Budget) Exhausted() (bool, float64, error) {
	if b.LimitUSD <= 0 {
		return false, 0, nil
	}
	us, err := b.Usage()
	if err != nil {
		return false, 0, err
	}
	return us.CostUSD >= b.LimitUSD, us.CostUSD, nil
}

// ---------------------------------------------------------------------------
// Model integrity
// ---------------------------------------------------------------------------

// ModelIssues checks what the proxy saw against the release config: every
// requested model must be one the release names, and every model a provider
// says it ran must be the requested one or an approved alias. Scripted
// dispatches never reached a provider and are not judged.
func ModelIssues(rc ReleaseConfig, calls []CallRecord) []string {
	allowed := rc.Models.all()
	seen := map[string]bool{}
	var out []string
	note := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, c := range calls {
		if c.Scripted || c.Status < 200 || c.Status >= 300 {
			continue
		}
		if c.RequestedModel != "" && !slices.Contains(allowed, c.RequestedModel) {
			note(fmt.Sprintf("requested model %s is not in the release config", c.RequestedModel))
		}
		switch {
		case c.ReturnedModel == "" && c.Path == "/chat/completions":
			note(fmt.Sprintf("a %s completion did not say which model ran", c.RequestedModel))
		case c.ReturnedModel != "" && c.RequestedModel != "" && !rc.ApprovedReturn(c.RequestedModel, c.ReturnedModel):
			note(fmt.Sprintf("unapproved substitution: requested %s, provider ran %s", c.RequestedModel, c.ReturnedModel))
		}
	}
	sort.Strings(out)
	return out
}

// ReturnedModels tallies requested → returned as the proxy saw them.
func ReturnedModels(calls []CallRecord) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, c := range calls {
		if c.Scripted || c.Status < 200 || c.Status >= 300 {
			continue
		}
		if out[c.RequestedModel] == nil {
			out[c.RequestedModel] = map[string]int{}
		}
		out[c.RequestedModel][firstNonEmpty(c.ReturnedModel, "(unstated)")]++
	}
	return out
}
