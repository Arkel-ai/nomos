package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var toolsLog = u.NewLogger("services.nomos_tools")

// Agent tools. This file owns the registry the
// runtime reads (AgentTools), the schema helpers every tool definition is
// built from, and the observation plumbing shared by the tool files:
//
//	nomos_tools_pieces.go     list/search/read pièces, read_evidence
//	nomos_tools_research.go   law, case law, web, compétences
//	nomos_tools_documents.go  revisions, calculate, verify/review, render,
//	                          export, ask_lawyer, submit_result
//
// Every schema is strict-mode compatible: each property is listed in
// `required`, and an optional one is typed as nullable instead. The model
// therefore always sends every key, and the decoders treat null as absent.
// Arguments are decoded with unknown fields rejected; every semantic check
// (scope, ownership, ranges) happens again on the server, never trusting the
// provider's schema enforcement.

// errDossierAccessRevoked fails the run: the actor lost read access to the
// turn's dossier between intake and this call.
var errDossierAccessRevoked = errors.New("dossier read access revoked")

func init() {
	AgentTools = newToolRegistry(allAgentTools()...)
}

// allAgentTools is the Slice 1 catalogue, in the order it is offered.
func allAgentTools() []AgentTool {
	return []AgentTool{
		listPiecesTool(), searchPiecesTool(), readPieceTool(), readEvidenceTool(),
		searchLawTool(), readLawTool(), searchCaselawTool(), readDecisionTool(), searchWebTool(),
		listCompetencesTool(), loadCompetenceTool(),
		askLawyerTool(),
		writeDocumentTool(), editDocumentTool(), readDocumentTool(),
		calculateTool(),
		verifyOutputTool(), reviewOutputTool(), renderDocumentTool(), exportDossierTool(),
		submitResultTool(),
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// availability is implemented by a tool whose prerequisites can be missing
// (a connector, the gateway, a Track C implementation). Definitions offers
// only available tools; Execute on an unavailable one answers
// tool_unavailable rather than an empty result.
type availability interface {
	Available() bool
}

type toolRegistry struct {
	mu    sync.RWMutex
	tools map[string]AgentTool
	order []string
}

func newToolRegistry(tools ...AgentTool) *toolRegistry {
	r := &toolRegistry{tools: map[string]AgentTool{}}
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			// A malformed built-in definition is a programming error that
			// must not reach a deployment.
			panic(err)
		}
	}
	return r
}

func (r *toolRegistry) Register(tool AgentTool) error {
	def := tool.Definition()
	if err := ValidateToolDefinition(def); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[def.Name]; dup {
		return fmt.Errorf("tool %s registered twice", def.Name)
	}
	r.tools[def.Name] = tool
	r.order = append(r.order, def.Name)
	return nil
}

func (r *toolRegistry) Lookup(name string) (AgentTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *toolRegistry) Definitions() []types.AgentToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]types.AgentToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		if a, ok := t.(availability); ok && !a.Available() {
			continue
		}
		out = append(out, t.Definition())
	}
	return out
}

// agentTool is the one AgentTool implementation: a definition, an optional
// availability probe and the execute function.
type agentTool struct {
	def       types.AgentToolDefinition
	available func() bool
	exec      func(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error)
}

func (t *agentTool) Definition() types.AgentToolDefinition { return t.def }

func (t *agentTool) Available() bool { return t.available == nil || t.available() }

// Execute runs the tool behind a recover: a bug inside one tool fails its
// step with an error the runtime records, never the worker process.
func (t *agentTool) Execute(ctx context.Context, x ToolExecution, args json.RawMessage) (out ToolOutcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			toolsLog.Errorf("Tool panicked (tool=%s run=%s call=%s): %v", t.def.Name, x.RunID, x.CallID, p)
			out, err = ToolOutcome{}, fmt.Errorf("tool %s panicked", t.def.Name)
		}
	}()
	if !t.Available() {
		toolsLog.Warnf("Unavailable tool called (tool=%s run=%s call=%s)", t.def.Name, x.RunID, x.CallID)
		return failure(x, t.def.Name, types.ToolCodeToolUnavailable, false,
			"cet outil n'est pas disponible dans cette configuration", nil,
			types.AgentActivityObject{Kind: activityKindFor(t.def.Name), Label: t.def.Name}), nil
	}
	return t.exec(ctx, x, args)
}

// ---------------------------------------------------------------------------
// Definitions and schemas
// ---------------------------------------------------------------------------

func toolDef(name, class string, readOnly, exclusive bool, description string, props map[string]any) types.AgentToolDefinition {
	props[types.ProgressNoteArg] = types.ProgressNoteSchema()
	return types.AgentToolDefinition{
		Name:        name,
		Description: description,
		Parameters:  objectSchema(props),
		Class:       class,
		ReadOnly:    readOnly,
		Exclusive:   exclusive,
	}
}

// objectSchema is a strict object: every property required, nothing else
// allowed. Optional properties are nullable, never omitted.
func objectSchema(props map[string]any) map[string]any {
	required := make([]any, 0, len(props))
	for _, name := range sortedKeys(props) {
		required = append(required, name)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             required,
	}
}

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func enumSchema(desc string, values ...string) map[string]any {
	vs := make([]any, 0, len(values))
	for _, v := range values {
		vs = append(vs, v)
	}
	return map[string]any{"type": "string", "enum": vs, "description": desc}
}

func arraySchema(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": desc}
}

// nullable widens a schema to accept null. An enum also lists null, as
// strict mode requires.
func nullable(s map[string]any) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	if t, ok := s["type"].(string); ok {
		out["type"] = []any{t, "null"}
	}
	if enum, ok := s["enum"].([]any); ok {
		out["enum"] = append(append([]any{}, enum...), nil)
	}
	return out
}

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

// argError is an invalid-arguments finding, returned to the agent verbatim.
type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

func badArgs(format string, a ...any) error { return &argError{msg: fmt.Sprintf(format, a...)} }

// decodeArgs strictly decodes a call's arguments: one JSON object, unknown
// fields rejected, and the progress note present and bounded (owner decision
// 1). Any failure is an argError.
func decodeArgs(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return badArgs("arguments manquants")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badArgs("arguments invalides : %s", jsonErrorText(err))
	}
	if dec.More() {
		return badArgs("arguments invalides : contenu après l'objet JSON")
	}
	var note struct {
		ProgressNote *string `json:"progress_note"`
	}
	_ = json.Unmarshal(trimmed, &note)
	if note.ProgressNote == nil || strings.TrimSpace(*note.ProgressNote) == "" {
		return badArgs("progress_note est obligatoire")
	}
	if utf8.RuneCountInString(*note.ProgressNote) > types.MaxProgressNoteRunes {
		return badArgs("progress_note dépasse %d caractères", types.MaxProgressNoteRunes)
	}
	return nil
}

// jsonErrorText keeps a decode error useful without echoing argument values.
func jsonErrorText(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("le champ %q attend le type %s", typeErr.Field, typeErr.Type.String())
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field") {
		return strings.TrimPrefix(msg, "json: ")
	}
	return "JSON mal formé"
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// ---------------------------------------------------------------------------
// Observations
// ---------------------------------------------------------------------------

// success builds an ok (or empty, when results is zero) observation.
func success(x ToolExecution, tool string, empty bool, data any, activity types.AgentActivityObject) ToolOutcome {
	status := types.ObservationOK
	if empty {
		status = types.ObservationEmpty
	}
	return ToolOutcome{
		Observation: types.ToolObservation{
			CallID:         x.CallID,
			Tool:           tool,
			Status:         status,
			Data:           mustJSON(data),
			EvidenceIDs:    []string{},
			ReadReceiptIDs: []string{},
		},
		Activity: activity,
	}
}

// failure builds an error observation the agent can recover from. message
// is server-written guidance; it never quotes source text.
func failure(x ToolExecution, tool, code string, retryable bool, message string, extra map[string]any, activity types.AgentActivityObject) ToolOutcome {
	data := map[string]any{"message": message}
	for k, v := range extra {
		data[k] = v
	}
	return ToolOutcome{
		Observation: types.ToolObservation{
			CallID:         x.CallID,
			Tool:           tool,
			Status:         types.ObservationError,
			Code:           code,
			Retryable:      retryable,
			Data:           mustJSON(data),
			EvidenceIDs:    []string{},
			ReadReceiptIDs: []string{},
		},
		Activity: activity,
	}
}

// invalid turns an argError into its observation; any other error is
// returned for the run to fail on.
func invalid(x ToolExecution, tool string, err error, activity types.AgentActivityObject) (ToolOutcome, error) {
	var ae *argError
	if errors.As(err, &ae) {
		toolsLog.Debugf("Tool arguments rejected (tool=%s run=%s call=%s)", tool, x.RunID, x.CallID)
		return failure(x, tool, types.ToolCodeInvalidArguments, false, ae.msg, nil, activity), nil
	}
	return ToolOutcome{}, err
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Every payload here is a plain struct or map; a marshal failure is a
		// programming error, reported rather than hidden.
		b, _ = json.Marshal(map[string]any{"message": "observation illisible"})
	}
	return b
}

func counts(results, total *int) *types.AgentActivityCounts {
	return &types.AgentActivityCounts{Results: results, Total: total}
}

// ---------------------------------------------------------------------------
// Identity and persistence
// ---------------------------------------------------------------------------

// agentRowID is a content-derived primary key: a replayed call converges on
// the same row instead of duplicating it. 96 bits of a sha256 behind a
// readable prefix the model can tell apart.
func agentRowID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])[:24]
}

// toolRows are the private rows one call makes durable, inserted inside the
// runner's fenced transaction by ToolOutcome.Persist.
type toolRows struct {
	spans    []types.AgentEvidenceSpan
	receipts []types.AgentReadReceipt
	web      []types.AgentWebSource
}

// persist inserts the rows idempotently: the same passage, receipt or web
// source converges on its natural key. tenant_id is stamped from the
// transaction's repository, never from the rows.
func (r toolRows) persist(tx *u.TenantRepo) error {
	tenant := tx.TenantID()
	if len(r.web) > 0 {
		for i := range r.web {
			r.web[i].TenantID = tenant
		}
		if err := tx.AgentWebSources().Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}, {Name: "url_sha256"}, {Name: "content_sha256"}},
			DoNothing: true,
		}).Create(&r.web).Error; err != nil {
			return fmt.Errorf("insert web sources: %w", err)
		}
	}
	if len(r.spans) > 0 {
		for i := range r.spans {
			r.spans[i].TenantID = tenant
		}
		if err := tx.AgentEvidenceSpans().Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}, {Name: "span_key"}},
			DoNothing: true,
		}).Create(&r.spans).Error; err != nil {
			return fmt.Errorf("insert evidence spans: %w", err)
		}
	}
	if len(r.receipts) > 0 {
		for i := range r.receipts {
			r.receipts[i].TenantID = tenant
		}
		if err := tx.AgentReadReceipts().Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "run_id"}, {Name: "tool_call_id"}, {Name: "piece_id"},
				{Name: "segment_id"}, {Name: "start_offset"}, {Name: "end_offset"},
			},
			DoNothing: true,
		}).Create(&r.receipts).Error; err != nil {
			return fmt.Errorf("insert read receipts: %w", err)
		}
	}
	return nil
}

func (r toolRows) empty() bool { return len(r.spans) == 0 && len(r.receipts) == 0 && len(r.web) == 0 }

// withRows attaches the rows to an outcome, with their ids on the
// observation.
func withRows(out ToolOutcome, rows toolRows) ToolOutcome {
	for _, s := range rows.spans {
		out.Observation.EvidenceIDs = appendUnique(out.Observation.EvidenceIDs, s.ID)
	}
	for _, r := range rows.receipts {
		out.Observation.ReadReceiptIDs = appendUnique(out.Observation.ReadReceiptIDs, r.ID)
	}
	if !rows.empty() {
		out.Persist = rows.persist
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

// newSpan builds one evidence span with its content-derived identity.
func newSpan(x ToolExecution, tool, sourceType, sourceID, sourceSHA string, loc types.SourceLocator, quote string, asOf *int64) types.AgentEvidenceSpan {
	key := types.EvidenceSpanKey(sourceType, sourceID, sourceSHA, loc, quote)
	locJSON, _ := json.Marshal(loc)
	span := types.AgentEvidenceSpan{
		ID:            agentRowID("ev_", x.RunID, key),
		RunID:         x.RunID,
		SpanKey:       key,
		SourceType:    sourceType,
		Locator:       types.JSONB(locJSON),
		Quote:         quote,
		SourceSHA256:  sourceSHA,
		RetrievalTool: tool,
		ToolCallID:    x.CallID,
		AsOfDate:      asOf,
	}
	id := sourceID
	switch sourceType {
	case types.CitationSourcePiece:
		span.PieceID = &id
	case types.CitationSourceLegifrance, types.CitationSourceJudilibre:
		span.LegalVersionID = &id
	case types.CitationSourceWeb:
		span.WebSourceID = &id
	}
	return span
}
