package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Agent service contracts. Track 0 owns this file.
// The runtime (Track A), the tools (Track B) and the gate and publication
// (Track C) implement and consume exactly these signatures; a change goes
// through Track 0 together with the wire types in api/types.
//
// Nothing here may be filled with a success-returning placeholder. Until an
// implementation is wired, its variable stays nil and the consumer must treat
// nil as "unavailable": no tools offered, verification refused, nothing
// published.

// ---------------------------------------------------------------------------
// Intake
// ---------------------------------------------------------------------------

// StartAgentTurnFunc is the signature of StartAgentTurn, which Track A
// defines in nomos_agent.go:
//
//	func StartAgentTurn(ctx context.Context, req types.AgentTurnRequest) (types.AgentStartResult, error)
//
// It authorizes the conversation and dossier, pins the compétence, and in one
// transaction inserts the user message, assistant placeholder, run, agent
// turn, bootstrap step and initial events. A refusal is an *AgentIntakeError.
type StartAgentTurnFunc func(ctx context.Context, req types.AgentTurnRequest) (types.AgentStartResult, error)

// Intake refusal codes, returned as {"detail", "code", "run_id"?}.
const (
	IntakeActiveTurn        = "active_turn"        // 409: another substantive turn is running; RunID names it
	IntakeInvalidCompetence = "invalid_competence" // 422: unknown, foreign, archived or incompatible compétence
)

// AgentIntakeError is StartAgentTurn's refusal: the HTTP status the caller
// returns, a stable code, and the active run for IntakeActiveTurn.
type AgentIntakeError struct {
	Status int
	Code   string
	RunID  string
}

func (e *AgentIntakeError) Error() string { return "agent intake refused: " + e.Code }

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// AgentTool is one tool the controller may call.
//
// Execute returns an observation for every EXPECTED outcome — zero results,
// bad arguments, unknown references, unavailable sources, privacy
// rejections, stale revisions — so the agent can recover. It returns an error
// only when the run itself must fail: corrupted data, revoked authorization,
// or an inability to record the execution durably.
type AgentTool interface {
	Definition() types.AgentToolDefinition
	Execute(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error)
}

// ToolRegistry holds the tools offered to the controller. Definitions lists
// only the tools whose prerequisites are configured: an unavailable connector
// is reported as unavailable, never as an empty search.
type ToolRegistry interface {
	// Register rejects a definition that fails ValidateToolDefinition, and a
	// name registered twice.
	Register(tool AgentTool) error
	Lookup(name string) (AgentTool, bool)
	Definitions() []types.AgentToolDefinition
}

// AgentTools is the registry Track B's nomos_tools.go sets. Nil means the
// agent has no tools and intake must not be enabled.
var AgentTools ToolRegistry

// ToolPermissions are the actor's rights on the turn's dossier, resolved by
// the server for this step.
type ToolPermissions struct {
	DossierRead  bool
	DossierWrite bool
}

// ToolBudget is what remains of the run's separate budgets when the call
// starts. The tool refuses with types.ToolCodeBudgetExhausted at zero.
type ToolBudget struct {
	ReviewCallsLeft int
	WebSearchesLeft int
}

// ToolExecution is the server-resolved context of one call. Nothing in it
// comes from the model's arguments: tool arguments never carry an
// authoritative tenant, dossier, run or storage key.
type ToolExecution struct {
	TenantID       string
	ActorID        string
	RunID          string
	StepID         string
	StepAttemptID  string
	ExecutionID    string // tags any paid call this tool makes (llm_calls)
	Dispatch       int
	CallID         string // types.AgentCallID(Dispatch, ordinal)
	ConversationID *string
	// Contract is the turn's pinned task contract; Contract.Scope is the
	// frozen dossier scope every private read is checked against.
	Contract    types.AgentTaskContract
	Permissions ToolPermissions
	Budget      ToolBudget
	// Heartbeat extends the step lease during long work. Nil in scripted
	// tests.
	Heartbeat func() error
}

// Repo is the tenant-scoped repository for this call's reads. Writes go
// through ToolOutcome.Persist instead.
func (x ToolExecution) Repo(ctx context.Context) *u.TenantRepo {
	return u.ForTenantID(ctx, x.TenantID)
}

// ToolOutcome is what a call produced.
type ToolOutcome struct {
	Observation types.ToolObservation
	// Activity and Counts describe the call's concrete object for its
	// agent.activity row; the runtime fills dispatch, call id, tool, progress
	// note, status and time.
	Activity types.AgentActivityObject
	Counts   *types.AgentActivityCounts
	// Persist, when set, runs inside the runner's lease-fenced transaction
	// that accepts this tool step — the only place a tool's rows (receipts,
	// evidence spans, revisions, web sources) become durable. It must do no
	// HTTP, S3, pandoc or model work; an error rolls the acceptance back.
	Persist func(tx *u.TenantRepo) error
}

// ValidateToolDefinition checks what the runtime relies on: a name, a known
// class, a strict object schema, and the required progress_note argument
// (owner decision 1). Every ToolRegistry.Register must call it.
func ValidateToolDefinition(def types.AgentToolDefinition) error {
	if def.Name == "" {
		return errors.New("tool definition: name is required")
	}
	switch def.Class {
	case types.ToolClassDB, types.ToolClassExternal, types.ToolClassRender:
	default:
		return fmt.Errorf("tool %s: unknown class %q", def.Name, def.Class)
	}
	p := def.Parameters
	if p["type"] != "object" || p["additionalProperties"] != false {
		return fmt.Errorf("tool %s: parameters must be an object schema with additionalProperties false", def.Name)
	}
	props, _ := p["properties"].(map[string]any)
	if _, ok := props[types.ProgressNoteArg]; !ok {
		return fmt.Errorf("tool %s: missing %s property", def.Name, types.ProgressNoteArg)
	}
	var required []string
	switch r := p["required"].(type) {
	case []string:
		required = r
	case []any:
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	if !slices.Contains(required, types.ProgressNoteArg) {
		return fmt.Errorf("tool %s: %s must be required", def.Name, types.ProgressNoteArg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// VerificationInput names the one revision to check (the answer is a
// revision of kind `answer`). The verifier loads the turn's contract, scope,
// evidence and receipts itself; the model supplies nothing it trusts.
type VerificationInput struct {
	TenantID           string
	RunID              string
	DocumentRevisionID string
	// Final is set by the final gate, which adds the delivery-only checks
	// (review cycle seen, required decisions resolved). verify_output runs
	// the same implementation with Final false.
	Final bool
}

// VerifyOutput is the one verifier behind verify_output and the final gate,
// set by Track C's verification.go. It reads and computes; it writes nothing —
// the caller stores the report as an agent_verifications row in its fenced
// transaction. Nil means verification is unavailable: publication refuses.
var VerifyOutput func(ctx context.Context, in VerificationInput) (types.VerificationReport, error)

// ---------------------------------------------------------------------------
// Rendering and publication
// ---------------------------------------------------------------------------

// ArtifactFormat is a renderable output format.
type ArtifactFormat string

const ArtifactFormatDOCX ArtifactFormat = "docx"

// VerifiedRevision is a revision loaded together with its server-created,
// hash-matching verification. Only this reaches the renderer.
type VerifiedRevision struct {
	Revision     types.AgentDocumentRevision
	Verification types.AgentVerification
}

// StagedArtifact is a rendered object in private storage, not yet published:
// no artifact row, no presigned URL.
type StagedArtifact struct {
	Kind               string // types.ArtifactKind*
	Name               string
	StagingKey         string // private key under the tenant prefix
	SHA256             string
	Size               int64
	DocumentRevisionID *string
	VerificationID     *string
	// PublicationKey is the stable identity of this publication: tenant,
	// run, revision hash, format, renderer/template hash, warning-report
	// hash. A retry reuses the published row instead of duplicating it.
	PublicationKey  string
	TemplateVersion string
	RendererVersion string
	PieceWarnings   int
	Partial         bool
}

// ArtifactPreparer renders a verified revision into a staged artifact,
// checking the rendered text against the verified structure.
type ArtifactPreparer interface {
	PrepareArtifact(ctx context.Context, rev VerifiedRevision, format ArtifactFormat) (StagedArtifact, error)
}

// PublicationInput is everything the final publication commits together.
type PublicationInput struct {
	TenantID           string
	RunID              string
	ActorID            string
	AssistantMessageID string
	Answer             VerifiedRevision
	Artifacts          []StagedArtifact
	Outcome            string // types.AgentOutcomeCompleted or AgentOutcomePartial
	// Fence runs fn inside the runner's lease-fenced transaction. Every row
	// the publication writes (artifacts, citations, the frozen answer,
	// artifact.ready) goes through it; byte checks on staged objects happen
	// before, outside the transaction.
	Fence func(ctx context.Context, fn func(tx *u.TenantRepo) error) error
}

// PublicationResult is what was published, or reused on a retry.
type PublicationResult struct {
	ArtifactIDs []string
	CitationIDs []string
	Reused      bool
}

// ResultPublisher atomically publishes verified results and freezes the
// final answer with its citation bindings.
type ResultPublisher interface {
	PublishResult(ctx context.Context, in PublicationInput) (PublicationResult, error)
}

// AgentArtifacts and AgentPublisher are set by Track C's agent_publication.go.
// Nil means nothing can be rendered or published.
var (
	AgentArtifacts ArtifactPreparer
	AgentPublisher ResultPublisher
)
