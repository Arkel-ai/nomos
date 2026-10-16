package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var verificationLog = u.NewLogger("services.verification")

// The verification gate. ONE implementation
// serves the agent's verify_output tool and the final gate (Final set), so a
// revision the agent saw pass cannot be judged differently at delivery.
//
// The verifier trusts nothing the model supplies. It loads the turn's
// contract, the frozen scope, the run's evidence spans, the stored legal
// versions and web sources itself, and checks the revision against them:
//
//   - ownership: every cited span belongs to this run; every pièce is in the
//     frozen scope; source hashes still match (blocking);
//   - structure: the typed body is well formed and its hash is the stored one
//     (blocking);
//   - compétence: required sections and mandatory clauses of a pinned
//     compétence (blocking);
//   - pièce claims: quotes, dates and amounts against the passage read
//     (WARNING — download stays allowed, never a green pass);
//   - legal citations: resolve to the exact stored version, quote present,
//     version applicable, decision before the cutoff, and no legal reference
//     in prose without such a citation (blocking — resolve or refuse);
//   - web: a stored URL with an excerpt that contains the quote; a URL alone
//     is a lead, never support (blocking);
//   - coverage: an exhaustive request cannot be presented as complete unless
//     the read-receipt ledger covers its whole scope (blocking, or a warning
//     on a result explicitly labelled incomplete);
//   - final only: every legal citation revalidated against its official
//     source — same version, same text, applicable (blocking; retryable when
//     the source is unreachable) —, required lawyer decisions resolved, and
//     the revision seen verified by a later accepted controller decision
//     (blocking).
//
// It reads and computes; it writes nothing. The caller stores the report with
// StoreVerification inside its fenced transaction.

// Verifier versions. The final gate records a distinct version so its report
// never collides with the verify_output report of the same content, and so
// publication can insist on a final one.
const (
	VerifierVersion      = "nomos-verify-1"
	VerifierVersionFinal = "nomos-verify-1+final"
)

// ErrVerificationTarget means the run, its agent turn, or the revision does
// not exist in this tenant and run. It never says which: the verify_output
// tool answers it with unknown_reference.
var ErrVerificationTarget = errors.New("verification target not found")

// maxVerifyScopePieces bounds one scope load. A larger dossier is an explicit
// processing limit, never a silent truncation of what "all pieces" means.
const maxVerifyScopePieces = 5000

func init() { VerifyOutput = VerifierWith(PisteSources()) }

// VerifierWith is the verifier bound to the official sources its final mode
// revalidates legal citations against (citation_sources.go). With nil
// sources no legal citation can be officially revalidated, so a final
// verification that cites one blocks.
func VerifierWith(official OfficialSources) func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
	return func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
		return verifyOutput(ctx, in, official)
	}
}

// ---------------------------------------------------------------------------
// The segment manifest — the integration point with the pièce reader
// ---------------------------------------------------------------------------

// PieceManifest is the canonical segmentation of one piece's current
// extraction: the unit coverage counts. Segment ids are the ones read_piece
// returns and agent_read_receipts.segment_id records; receipt offsets are
// code-point offsets inside the segment.
type PieceManifest struct {
	SourceSHA256      string // the original's hash, as receipts record it
	ExtractionVersion string
	Unit              string // page | paragraph | cell
	// State is empty when the extraction is usable, else
	// types.CoveragePending or types.CoverageUnreadable.
	State    string
	Segments []ManifestSegment
}

// ManifestSegment is one canonical segment.
type ManifestSegment struct {
	ID       string
	Runes    int  // code points in the segment text
	Readable bool // false for a page OCR could not read
}

// PieceManifestFor returns a piece's manifest. The pièce reader (Track B,
// piece_evidence.go) sets it. While it is nil coverage cannot be measured:
// no piece is ever reported complete, so an exhaustive result can only be
// published as explicitly incomplete — never assumed whole.
var PieceManifestFor func(ctx context.Context, repo *u.TenantRepo, piece types.Piece) (PieceManifest, error)

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

func verifyOutput(ctx context.Context, in VerificationInput, official OfficialSources) (types.VerificationReport, error) {
	if u.DB == nil {
		return types.VerificationReport{}, u.ErrAuthNotConfigured
	}
	if in.TenantID == "" || in.RunID == "" || in.DocumentRevisionID == "" {
		return types.VerificationReport{}, ErrVerificationTarget
	}
	repo := u.ForTenantID(ctx, in.TenantID)
	v := &verifier{ctx: ctx, repo: repo, in: in, sources: official, checked: map[string]int{}, ran: map[string]bool{}}

	if err := v.load(); err != nil {
		return types.VerificationReport{}, err
	}
	if v.checkRevisionIdentity() {
		var err error
		if v.scope, err = loadScope(repo, v.contract); err != nil {
			return types.VerificationReport{}, err
		}
		if err := v.loadEvidence(); err != nil {
			return types.VerificationReport{}, err
		}
		if err := v.loadLawyerInput(); err != nil {
			return types.VerificationReport{}, err
		}
		v.checkStructure()
		v.checkClaims()
		v.checkTextFacts()
		v.checkLegalSpans()
		v.checkLegalProse()
		v.checkWebSpans()
		if err := v.checkCompetence(); err != nil {
			return types.VerificationReport{}, err
		}
		if err := v.checkCoverage(); err != nil {
			return types.VerificationReport{}, err
		}
		if in.Final {
			if err := v.checkOfficialSources(); err != nil {
				return types.VerificationReport{}, err
			}
			v.checkDecisions()
			if err := v.checkReview(); err != nil {
				return types.VerificationReport{}, err
			}
		}
	}
	report := v.report()
	verificationLog.Infof("Output verified (run=%s revision=%s kind=%s final=%t result=%s warnings=%d blocking=%d spans=%d)",
		in.RunID, in.DocumentRevisionID, v.rev.Kind, in.Final, report.Result, report.Warnings, report.Blocking, len(v.spans))
	return report, nil
}

// StoreVerification records a report as the agent_verifications row bound to
// its hashes, inside the caller's (fenced) transaction. The same report
// stored twice converges on one row.
func StoreVerification(tx *u.TenantRepo, runID string, report types.VerificationReport) (types.AgentVerification, error) {
	var existing types.AgentVerification
	err := tx.AgentVerifications().
		Where("document_revision_id = ? AND content_sha256 = ? AND scope_sha256 = ? AND evidence_sha256 = ? AND verifier_version = ?",
			report.DocumentRevisionID, report.ContentSHA256, report.ScopeSHA256, report.EvidenceSHA256, report.VerifierVersion).
		Take(&existing).Error
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return types.AgentVerification{}, err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return types.AgentVerification{}, err
	}
	row := types.AgentVerification{
		ID:                 uuid.NewString(),
		RunID:              runID,
		DocumentRevisionID: report.DocumentRevisionID,
		ContentSHA256:      report.ContentSHA256,
		ScopeSHA256:        report.ScopeSHA256,
		EvidenceSHA256:     report.EvidenceSHA256,
		VerifierVersion:    report.VerifierVersion,
		Result:             report.Result,
		DownloadAllowed:    report.DownloadAllowed,
		Warnings:           report.Warnings,
		Blocking:           report.Blocking,
		Report:             types.JSONB(raw),
	}
	if err := tx.Create(&row); err != nil {
		return types.AgentVerification{}, err
	}
	return row, nil
}

// LoadVerifiedRevision loads a stored verification and the revision it
// checked, and refuses unless the two still match: same revision, same
// content hash (recomputed from the stored body), download allowed. This is
// the only way to obtain the VerifiedRevision the renderer accepts.
func LoadVerifiedRevision(ctx context.Context, tenantID, runID, verificationID string) (VerifiedRevision, error) {
	if u.DB == nil {
		return VerifiedRevision{}, u.ErrAuthNotConfigured
	}
	repo := u.ForTenantID(ctx, tenantID)
	var ver types.AgentVerification
	if err := repo.AgentVerifications().Where("id = ? AND run_id = ?", verificationID, runID).Take(&ver).Error; err != nil {
		return VerifiedRevision{}, ErrVerificationTarget
	}
	var rev types.AgentDocumentRevision
	if err := repo.AgentDocumentRevisions().Where("id = ? AND run_id = ?", ver.DocumentRevisionID, runID).Take(&rev).Error; err != nil {
		return VerifiedRevision{}, ErrVerificationTarget
	}
	vr := VerifiedRevision{Revision: rev, Verification: ver}
	if err := checkVerifiedPair(vr); err != nil {
		return VerifiedRevision{}, err
	}
	return vr, nil
}

// ErrNotVerified means a revision has no matching, download-allowing
// verification. Nothing is rendered or published from it.
var ErrNotVerified = errors.New("revision not verified for publication")

// checkVerifiedPair is the invariant every render and publication rechecks.
func checkVerifiedPair(vr VerifiedRevision) error {
	ver, rev := vr.Verification, vr.Revision
	if ver.DocumentRevisionID != rev.ID || ver.RunID != rev.RunID || ver.TenantID != rev.TenantID {
		return fmt.Errorf("%w: verification is for another revision", ErrNotVerified)
	}
	if ver.VerifierVersion != VerifierVersion && ver.VerifierVersion != VerifierVersionFinal {
		return fmt.Errorf("%w: unknown verifier version", ErrNotVerified)
	}
	if ver.Result == types.VerificationBlocked || !ver.DownloadAllowed {
		return fmt.Errorf("%w: verification blocked", ErrNotVerified)
	}
	sum, err := revisionContentSHA256(rev)
	if err != nil || sum != rev.ContentSHA256 || ver.ContentSHA256 != rev.ContentSHA256 {
		return fmt.Errorf("%w: content hash mismatch", ErrNotVerified)
	}
	return nil
}

// revisionContentSHA256 recomputes a stored revision's content hash.
func revisionContentSHA256(rev types.AgentDocumentRevision) (string, error) {
	var body types.DocumentRevisionBody
	if err := json.Unmarshal(rev.Body, &body); err != nil {
		return "", err
	}
	return types.DocumentContentSHA256(rev.Kind, rev.Title, body)
}

// ---------------------------------------------------------------------------
// The verifier
// ---------------------------------------------------------------------------

type verifier struct {
	ctx     context.Context
	repo    *u.TenantRepo
	in      VerificationInput
	sources OfficialSources // the official publications of the final gate

	run      types.Run
	turn     types.AgentTurn
	contract types.AgentTaskContract
	rev      types.AgentDocumentRevision
	body     types.DocumentRevisionBody

	scope   scopeState
	spans   map[string]types.AgentEvidenceSpan
	legal   map[string]*AgentLegalSource // by legal_versions.id
	web     map[string]types.AgentWebSource
	answers map[string]lawyerAnswer
	// lawyerText is everything the lawyer wrote in this thread and run: a
	// lawyer_provided date or amount must be found in it.
	lawyerText []string

	issues   []types.VerificationIssue
	official []types.OfficialCheck // successful official revalidations
	checked  map[string]int
	ran      map[string]bool
	coverage *types.CoverageReport
	// calcs are the run's successful calculate calls, by call id, loaded
	// on first use.
	calcs map[string]agentCalculation
	// evidence are the parts the result depends on beyond content and scope;
	// their hash keys the stored report, so a changed input never reuses a
	// stale row.
	evidence []string
}

type lawyerAnswer struct {
	answered bool
	text     []string
}

func (v *verifier) load() error {
	if err := v.repo.Runs().Where("id = ?", v.in.RunID).Take(&v.run).Error; err != nil {
		return notFoundOr(err)
	}
	if err := v.repo.AgentTurns().Where("run_id = ?", v.in.RunID).Take(&v.turn).Error; err != nil {
		return notFoundOr(err)
	}
	if err := v.repo.AgentDocumentRevisions().Where("id = ? AND run_id = ?", v.in.DocumentRevisionID, v.in.RunID).
		Take(&v.rev).Error; err != nil {
		return notFoundOr(err)
	}
	if err := json.Unmarshal(v.turn.TaskContract, &v.contract); err != nil {
		return fmt.Errorf("agent turn %s: unreadable task contract: %w", v.turn.ID, err)
	}
	return nil
}

func notFoundOr(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrVerificationTarget
	}
	return err
}

// add records one issue. Blocking unless the caller says warning.
func (v *verifier) add(check, code, severity, blockID, claimID string, evidence []string, correction string) {
	v.ran[check] = true
	v.issues = append(v.issues, types.VerificationIssue{
		Code: code, Severity: severity, Check: check,
		BlockID: blockID, ClaimID: claimID, EvidenceIDs: evidence, Correction: correction,
		Retryable: code == types.IssueLegalOfficialDown,
	})
}

func (v *verifier) block(check, code, blockID, claimID string, evidence []string, correction string) {
	v.add(check, code, types.SeverityBlocking, blockID, claimID, evidence, correction)
}

func (v *verifier) count(check string, n int) {
	v.ran[check] = true
	v.checked[check] += n
}

// checkRevisionIdentity decodes the body and recomputes its hash. A body that
// does not decode, or whose hash is not the stored one, is not checked any
// further: nothing in it can be trusted.
func (v *verifier) checkRevisionIdentity() bool {
	v.count(types.CheckOwnership, 1)
	v.count(types.CheckStructure, 1)
	if err := json.Unmarshal(v.rev.Body, &v.body); err != nil {
		v.block(types.CheckStructure, types.IssueStructureInvalid, "", "", nil,
			"Le contenu de cette révision est illisible : réécrivez le document (write_document).")
		return false
	}
	sum, err := types.DocumentContentSHA256(v.rev.Kind, v.rev.Title, v.body)
	if err != nil || sum != v.rev.ContentSHA256 {
		v.block(types.CheckOwnership, types.IssueRevisionHashMismatch, "", "", nil,
			"Le contenu de cette révision ne correspond plus à son empreinte enregistrée : réécrivez le document.")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// claimEvidenceIDs is every span a claim cites, in first-use order.
func claimEvidenceIDs(c types.DocumentClaim) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range c.EvidenceIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, q := range c.Quotes {
		if q.EvidenceID != "" && !seen[q.EvidenceID] {
			seen[q.EvidenceID] = true
			out = append(out, q.EvidenceID)
		}
	}
	return out
}

// revisionEvidenceOrder is every span a revision cites, in first-use order —
// the numbering of its citations.
func revisionEvidenceOrder(body types.DocumentRevisionBody) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range body.Blocks {
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	return out
}

func (v *verifier) loadEvidence() error {
	ids := revisionEvidenceOrder(v.body)
	v.spans = map[string]types.AgentEvidenceSpan{}
	v.legal = map[string]*AgentLegalSource{}
	v.web = map[string]types.AgentWebSource{}
	if len(ids) == 0 {
		return nil
	}
	var spans []types.AgentEvidenceSpan
	if err := v.repo.AgentEvidenceSpans().Where("run_id = ? AND id IN ?", v.run.ID, ids).Find(&spans).Error; err != nil {
		return err
	}
	var versionIDs, webIDs []string
	for _, s := range spans {
		v.spans[s.ID] = s
		v.evidence = append(v.evidence, "span|"+s.ID+"|"+s.SourceSHA256+"|"+sha256Hex(s.Quote))
		if s.LegalVersionID != nil {
			versionIDs = append(versionIDs, *s.LegalVersionID)
		}
		if s.WebSourceID != nil {
			webIDs = append(webIDs, *s.WebSourceID)
		}
	}
	sources, err := loadLegalSources(v.ctx, versionIDs)
	if err != nil {
		return err
	}
	v.legal = sources
	for id, src := range sources {
		v.evidence = append(v.evidence, fmt.Sprintf("legal|%s|%s|%s|%s|%s|%s", id, src.Version.TextSHA256,
			ptrInt64(src.Version.ValidFrom), ptrInt64(src.Version.ValidTo), src.Version.State, ptrInt64(src.Version.DecisionDate)))
	}
	if len(webIDs) > 0 {
		var web []types.AgentWebSource
		if err := v.repo.AgentWebSources().Where("run_id = ? AND id IN ?", v.run.ID, webIDs).Find(&web).Error; err != nil {
			return err
		}
		for _, w := range web {
			v.web[w.ID] = w
			v.evidence = append(v.evidence, "web|"+w.ID+"|"+w.ContentSHA256)
		}
	}
	return nil
}

// loadLegalSources reads legal_versions and legal_documents — public, shared
// tables without a tenant column — by id.
func loadLegalSources(ctx context.Context, versionIDs []string) (map[string]*AgentLegalSource, error) {
	out := map[string]*AgentLegalSource{}
	if len(versionIDs) == 0 {
		return out, nil
	}
	var versions []types.LegalVersion
	if err := u.DB.WithContext(ctx).Where("id IN ?", versionIDs).Find(&versions).Error; err != nil {
		return nil, err
	}
	docIDs := make([]string, 0, len(versions))
	for _, ver := range versions {
		docIDs = append(docIDs, ver.LegalDocumentID)
	}
	var docs []types.LegalDocument
	if len(docIDs) > 0 {
		if err := u.DB.WithContext(ctx).Where("id IN ?", docIDs).Find(&docs).Error; err != nil {
			return nil, err
		}
	}
	byID := map[string]types.LegalDocument{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	for _, ver := range versions {
		if d, ok := byID[ver.LegalDocumentID]; ok {
			out[ver.ID] = &AgentLegalSource{Version: ver, Document: d}
		}
	}
	return out, nil
}

func ptrInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

// loadLawyerInput reads the run's question answers and what the lawyer wrote
// in the thread — the only places a lawyer_provided fact may come from.
func (v *verifier) loadLawyerInput() error {
	v.answers = map[string]lawyerAnswer{}
	var qs []types.RunQuestion
	if err := v.repo.RunQuestions().Where("run_id = ?", v.run.ID).Find(&qs).Error; err != nil {
		return err
	}
	for _, q := range qs {
		a := v.answers[q.QuestionKey]
		if q.State == types.QuestionStateAnswered {
			a.answered = true
			a.text = append(a.text, jsonStrings(q.Answer)...)
		}
		v.answers[q.QuestionKey] = a
	}
	v.lawyerText = []string{v.contract.Instruction}
	if v.run.ConversationID != nil {
		var msgs []string
		if err := v.repo.Messages().Where("conversation_id = ? AND role = ?", *v.run.ConversationID, types.MessageRoleUser).
			Order("created_at DESC").Limit(200).Pluck("content", &msgs).Error; err != nil {
			return err
		}
		v.lawyerText = append(v.lawyerText, msgs...)
	}
	return nil
}

// jsonStrings collects every string in a JSON document.
func jsonStrings(raw types.JSONB) []string {
	var x any
	if len(raw) == 0 || json.Unmarshal(raw, &x) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(x)
	return out
}

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

// scopeState is the turn's documentary scope, resolved against the dossier
// as it stands. A piece added to the dossier after the turn started is in an
// "all pieces" scope: the scope can only grow, which can only make coverage
// harder to reach — never falsely complete.
type scopeState struct {
	pieces   []types.Piece // in scope, by ordinal
	byID     map[string]types.Piece
	excluded []types.Piece
	missing  []string          // explicitly included ids that no longer exist
	assetSHA map[string]string // piece id → original's sha256
	sha      string
}

func loadScope(repo *u.TenantRepo, contract types.AgentTaskContract) (scopeState, error) {
	s := scopeState{byID: map[string]types.Piece{}, assetSHA: map[string]string{}}
	sc := contract.Scope
	if sc.Mode != types.ScopeNone && sc.DossierID != "" {
		var all []types.Piece
		if err := repo.Pieces().Where("dossier_id = ?", sc.DossierID).
			Order("ordinal ASC, created_at ASC").Limit(maxVerifyScopePieces + 1).Find(&all).Error; err != nil {
			return s, err
		}
		if len(all) > maxVerifyScopePieces {
			return s, fmt.Errorf("scope of dossier %s exceeds %d pieces: processing limit", sc.DossierID, maxVerifyScopePieces)
		}
		excluded := map[string]bool{}
		included := map[string]bool{}
		for _, e := range sc.Excluded {
			excluded[e.PieceID] = true
		}
		for _, e := range sc.Included {
			included[e.PieceID] = true
		}
		found := map[string]bool{}
		for _, p := range all {
			found[p.ID] = true
			inScope := false
			switch sc.Mode {
			case types.ScopeAllPieces:
				inScope = !excluded[p.ID]
			case types.ScopeExplicit:
				inScope = included[p.ID]
			}
			if inScope {
				s.pieces = append(s.pieces, p)
				s.byID[p.ID] = p
			} else {
				s.excluded = append(s.excluded, p)
			}
		}
		if sc.Mode == types.ScopeExplicit {
			for id := range included {
				if !found[id] {
					s.missing = append(s.missing, id)
				}
			}
			sort.Strings(s.missing)
		}
		if len(s.pieces) > 0 {
			assetIDs := make([]string, 0, len(s.pieces))
			for _, p := range s.pieces {
				assetIDs = append(assetIDs, p.SourceAssetID)
			}
			var assets []types.Asset
			if err := repo.Assets().Where("id IN ?", assetIDs).Select("id", "sha256").Find(&assets).Error; err != nil {
				return s, err
			}
			byAsset := map[string]string{}
			for _, a := range assets {
				byAsset[a.ID] = a.SHA256
			}
			for _, p := range s.pieces {
				s.assetSHA[p.ID] = byAsset[p.SourceAssetID]
			}
		}
	}

	type pieceID struct {
		ID, SHA, Extraction string
		Ordinal             int
	}
	ids := make([]pieceID, 0, len(s.pieces))
	for _, p := range s.pieces {
		ids = append(ids, pieceID{p.ID, s.assetSHA[p.ID], p.ExtractionVersion, p.Ordinal})
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].ID < ids[j].ID })
	raw, err := json.Marshal(struct {
		Mode, Dossier string
		Pieces        []pieceID
		Excluded      []types.ScopeEntry
		Missing       []string
	}{sc.Mode, sc.DossierID, ids, sc.Excluded, s.missing})
	if err != nil {
		return s, err
	}
	sum := sha256.Sum256(raw)
	s.sha = hex.EncodeToString(sum[:])
	return s, nil
}

func pieceTitle(p types.Piece) string {
	return firstNonEmpty(p.Title, p.FinalName, p.ProposedName, p.OriginalName)
}

// ---------------------------------------------------------------------------
// Structure
// ---------------------------------------------------------------------------

var revisionKinds = map[string]bool{
	types.AgentOutputAnswer: true, types.AgentOutputChronology: true, types.AgentOutputSynthesis: true,
	types.AgentOutputLegalNote: true, types.AgentOutputActe: true, types.AgentOutputBordereau: true,
}

var blockRoles = map[string]bool{
	types.BlockHeading: true, types.BlockParagraph: true, types.BlockListItem: true,
	types.BlockChronologyRow: true, types.BlockBordereauRow: true, types.BlockLimitation: true,
	types.BlockWarning: true,
}

var claimKinds = map[string]bool{
	types.ClaimFactual: true, types.ClaimLegal: true, types.ClaimAnalysis: true,
	types.ClaimLawyerProvided: true, types.ClaimDerived: true,
}

var moneyQualifiers = map[string]bool{
	"": true, types.MoneyGross: true, types.MoneyNet: true, types.MoneyHT: true, types.MoneyTTC: true,
}

func validPartialDate(d types.PartialDate) bool {
	switch d.Precision {
	case types.DatePrecisionDay:
		return validDay(d.Year, d.Month, d.Day)
	case types.DatePrecisionMonth:
		return d.Year >= 1000 && d.Month >= 1 && d.Month <= 12 && d.Day == 0
	case types.DatePrecisionYear:
		return d.Year >= 1000 && d.Month == 0 && d.Day == 0
	case types.DatePrecisionUnknown:
		return d.Year == 0 && d.Month == 0 && d.Day == 0
	}
	return false
}

func validClaimDate(d types.ClaimDate) bool {
	switch d.Role {
	case types.DateRoleEvent, types.DateRoleDocument, types.DateRoleReceipt:
		return validPartialDate(d.PartialDate)
	}
	return false
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (v *verifier) checkStructure() {
	invalid := func(blockID, claimID, what string) {
		v.block(types.CheckStructure, types.IssueStructureInvalid, blockID, claimID, nil,
			"Structure invalide : "+what+". Corrigez le document (edit_document).")
	}
	if v.body.SchemaVersion != types.DocumentSchemaVersion || v.rev.SchemaVersion != types.DocumentSchemaVersion {
		invalid("", "", "version de schéma inconnue")
	}
	if !revisionKinds[v.rev.Kind] {
		invalid("", "", fmt.Sprintf("type de document %q inconnu", v.rev.Kind))
	}
	if len(v.body.Blocks) == 0 {
		invalid("", "", "le document ne contient aucun bloc")
	}
	blockIDs := map[string]bool{}
	claimIDs := map[string]bool{}
	for _, b := range v.body.Blocks {
		v.count(types.CheckStructure, 1)
		if b.ID == "" || blockIDs[b.ID] {
			invalid(b.ID, "", "chaque bloc doit avoir un identifiant unique")
		}
		blockIDs[b.ID] = true
		if !blockRoles[b.Role] {
			invalid(b.ID, "", fmt.Sprintf("rôle de bloc %q inconnu", b.Role))
		}
		if strings.TrimSpace(b.Text) == "" {
			invalid(b.ID, "", "bloc vide")
		}
		switch b.Role {
		case types.BlockChronologyRow:
			if b.Chronology == nil || !validClaimDate(b.Chronology.Date) {
				invalid(b.ID, "", "une ligne de chronologie doit porter une date valide (précision et rôle)")
			}
		case types.BlockBordereauRow:
			if b.Bordereau == nil || b.Bordereau.Ordinal <= 0 || b.Bordereau.PieceID == "" ||
				(b.Bordereau.Date != nil && !validPartialDate(*b.Bordereau.Date)) {
				invalid(b.ID, "", "une ligne de bordereau doit porter un numéro, une pièce et une date valide")
			} else if _, ok := v.scope.byID[b.Bordereau.PieceID]; !ok {
				v.block(types.CheckOwnership, types.IssueEvidenceUnauthorized, b.ID, "", nil,
					"Cette ligne de bordereau vise une pièce hors du périmètre du dossier de ce tour.")
			}
		}
		runes := len([]rune(b.Text))
		for _, c := range b.Claims {
			if c.ID == "" || claimIDs[c.ID] {
				invalid(b.ID, c.ID, "chaque affirmation doit avoir un identifiant unique")
			}
			claimIDs[c.ID] = true
			if !claimKinds[c.Kind] {
				invalid(b.ID, c.ID, fmt.Sprintf("type d'affirmation %q inconnu", c.Kind))
			}
			if c.Start < 0 || c.End > runes || c.Start >= c.End {
				invalid(b.ID, c.ID, "la plage de l'affirmation sort du texte du bloc")
			}
			for _, d := range c.Dates {
				if !validClaimDate(d) {
					invalid(b.ID, c.ID, "date invalide (précision, rôle, ou jour inventé)")
				}
			}
			for _, m := range c.Amounts {
				if !validCurrency(m.Currency) || !moneyQualifiers[m.Qualifier] {
					invalid(b.ID, c.ID, "montant invalide (devise ISO 4217, qualificatif brut/net/HT/TTC)")
				}
			}
			for _, q := range c.Quotes {
				if strings.TrimSpace(q.Text) == "" || q.EvidenceID == "" {
					invalid(b.ID, c.ID, "une citation doit porter un texte et sa source")
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Claims: ownership, pièces, kinds
// ---------------------------------------------------------------------------

func (v *verifier) checkClaims() {
	for _, b := range v.body.Blocks {
		for _, c := range b.Claims {
			v.checkClaim(b, c)
		}
	}
}

func (v *verifier) checkClaim(b types.DocumentBlock, c types.DocumentClaim) {
	var pieceEv []PieceEvidence
	var hasLegal, hasWeb bool
	for _, id := range claimEvidenceIDs(c) {
		v.count(types.CheckOwnership, 1)
		span, ok := v.spans[id]
		if !ok {
			v.block(types.CheckOwnership, types.IssueEvidenceUnknown, b.ID, c.ID, []string{id},
				"Cette source n'a pas été retournée par un outil pendant ce tour : citez uniquement les preuves (evidence_ids) que les outils ont fournies.")
			continue
		}
		switch span.SourceType {
		case types.CitationSourcePiece:
			pid := ""
			if span.PieceID != nil {
				pid = *span.PieceID
			}
			p, inScope := v.scope.byID[pid]
			if !inScope {
				v.block(types.CheckOwnership, types.IssueEvidenceUnauthorized, b.ID, c.ID, []string{id},
					"Cette pièce est hors du périmètre documentaire de ce tour : retirez cette source.")
				continue
			}
			if span.SourceSHA256 != v.scope.assetSHA[pid] {
				v.block(types.CheckOwnership, types.IssueSourceMutated, b.ID, c.ID, []string{id},
					"L'identité de la pièce citée ne correspond plus à l'original enregistré : relisez la pièce (read_piece).")
				continue
			}
			pieceEv = append(pieceEv, pieceEvidenceOf(id, span, p))
		case types.CitationSourceLegifrance, types.CitationSourceJudilibre:
			hasLegal = true
		case types.CitationSourceWeb:
			hasWeb = true
		default:
			v.block(types.CheckOwnership, types.IssueEvidenceUnknown, b.ID, c.ID, []string{id},
				"Cette source est d'un type inconnu : citez uniquement les preuves fournies par les outils.")
		}
	}
	for _, k := range c.AnswerKeys {
		a, ok := v.answers[k]
		v.evidence = append(v.evidence, fmt.Sprintf("answer|%s|%t", k, a.answered))
		if !ok || !a.answered {
			v.block(types.CheckOwnership, types.IssueEvidenceUnknown, b.ID, c.ID, nil,
				fmt.Sprintf("La réponse de l'avocat « %s » n'existe pas ou n'a pas été donnée : ne l'invoquez pas.", k))
		}
	}

	if len(pieceEv) > 0 {
		v.count(types.CheckPieceClaims, 1)
		v.issues = append(v.issues, CheckPieceClaims(b.ID, c, pieceEv)...)
	}
	warn := func(code, sentence string) {
		v.add(types.CheckPieceClaims, code, types.SeverityWarning, b.ID, c.ID, nil, sentence)
	}
	switch c.Kind {
	case types.ClaimFactual:
		if len(pieceEv) == 0 && !hasLegal && !hasWeb && len(c.AnswerKeys) == 0 {
			v.count(types.CheckPieceClaims, 1)
			warn(types.IssuePieceMetadataOnlySupport, warnUnsupportedFact)
		}
	case types.ClaimLegal:
		v.count(types.CheckLegalCitations, 1)
		if !hasLegal {
			v.block(types.CheckLegalCitations, types.IssueLegalReferenceUncited, b.ID, c.ID, nil,
				"Une proposition juridique doit reposer sur un texte ou une décision retrouvés pendant ce tour (read_law, read_decision) : citez-les, ou retirez la proposition.")
		}
	case types.ClaimLawyerProvided:
		v.count(types.CheckPieceClaims, 1)
		if !v.lawyerStated(c) || !v.lawyerCheckable(b, c) {
			warn(types.IssuePieceMetadataOnlySupport, warnLawyerProvided)
		}
	case types.ClaimDerived:
		if len(c.Amounts) == 0 {
			break
		}
		v.count(types.CheckPieceClaims, 1)
		id := strings.TrimSpace(c.CalculationCallID)
		if id == "" {
			if len(pieceEv) == 0 { // with pièces, CheckPieceClaims already said so
				warn(types.IssuePieceCalculationMissing, "À vérifier — ce montant calculé ne s'appuie sur aucun calcul enregistré.")
			}
			break
		}
		calc, ok := v.calculation(id)
		if !ok {
			warn(types.IssuePieceCalculationMissing, "À vérifier — le calcul cité pour ce montant n'existe pas dans ce travail.")
			break
		}
		for _, m := range c.Amounts {
			if !calc.produced(m) {
				v.add(types.CheckPieceClaims, types.IssuePieceCalculationMissing, types.SeverityWarning, b.ID, c.ID, calc.evidence,
					fmt.Sprintf("À vérifier — le montant de %s ne correspond pas au résultat du calcul enregistré.", FrenchMoney(m)))
			}
		}
	}
}

// lawyerCheckable reports whether a lawyer_provided claim can be traced to
// the lawyer at all: a date, an amount or an answer it rests on, a figure in
// its text (checked by checkTextFacts), or its words found in what the
// lawyer wrote. A claim with none of these asserts something nobody checked.
func (v *verifier) lawyerCheckable(b types.DocumentBlock, c types.DocumentClaim) bool {
	if len(c.Dates) > 0 || len(c.Amounts) > 0 || len(c.AnswerKeys) > 0 {
		return true
	}
	text := substrRunes(b.Text, c.Start, c.End)
	for _, m := range FindTextMentions(b.Text) {
		if m.Start < c.End && c.Start < m.End {
			return true
		}
	}
	for _, t := range v.lawyerTexts(c, true) {
		if QuoteOccursIn(text, t) {
			return true
		}
	}
	return false
}

// lawyerTexts is what the lawyer wrote that a claim may rest on: the answers
// it names, and for a lawyer_provided claim the whole thread and every answer.
func (v *verifier) lawyerTexts(c types.DocumentClaim, provided bool) []string {
	var out []string
	if provided {
		out = append(out, v.lawyerText...)
		for _, a := range v.answers {
			out = append(out, a.text...)
		}
		return out
	}
	for _, k := range c.AnswerKeys {
		out = append(out, v.answers[k].text...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Calculations behind derived amounts
// ---------------------------------------------------------------------------

// agentCalculation is a successful calculate call of this run, as its tool
// step recorded it.
type agentCalculation struct {
	money    *types.Money  // the result, when it is an amount
	operands []types.Money // the amounts it was computed from
	evidence []string      // the operands' evidence
}

// produced reports whether m is this calculation's result.
func (c agentCalculation) produced(m types.Money) bool {
	return c.money != nil && c.money.MinorUnits == m.MinorUnits && c.money.Currency == m.Currency
}

// calculation resolves a calculation_call_id to the run's own calculate
// step, never to what the model says it computed.
func (v *verifier) calculation(callID string) (agentCalculation, bool) {
	if v.calcs == nil {
		v.calcs = map[string]agentCalculation{}
		var steps []types.RunStep
		if err := v.repo.RunSteps().Where("run_id = ? AND step_key LIKE ? AND state = ?",
			v.run.ID, "agent:tool:%", types.StepStateSucceeded).Find(&steps).Error; err != nil {
			verificationLog.Warnf("Calculation steps unreadable (run=%s): %v", v.run.ID, err)
		}
		for _, s := range steps {
			var rec agentToolRecord
			if !decodeRecord(s.Output, &rec) || rec.Call.Name != types.ToolCalculate || rec.Observation.Status != types.ObservationOK {
				continue
			}
			var data struct {
				Money    *types.Money `json:"money"`
				Operands []struct {
					Value       string   `json:"value"`
					Unit        string   `json:"unit"`
					EvidenceIDs []string `json:"evidence_ids"`
				} `json:"operands"`
			}
			if json.Unmarshal(rec.Observation.Data, &data) != nil {
				continue
			}
			calc := agentCalculation{money: data.Money}
			for _, op := range data.Operands {
				calc.evidence = append(calc.evidence, op.EvidenceIDs...)
				if r, err := parseDecimal(op.Value); err == nil && op.Unit != "" {
					scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10(2)))
					if scaled.IsInt() && scaled.Num().IsInt64() {
						calc.operands = append(calc.operands, types.Money{MinorUnits: scaled.Num().Int64(), Currency: op.Unit})
					}
				}
			}
			v.calcs[rec.Call.ID] = calc
		}
	}
	calc, ok := v.calcs[callID]
	result := "-"
	if ok && calc.money != nil {
		result = fmt.Sprintf("%d %s", calc.money.MinorUnits, calc.money.Currency)
	}
	v.evidence = append(v.evidence, fmt.Sprintf("calc|%s|%t|%s", callID, ok, result))
	return calc, ok
}

// ---------------------------------------------------------------------------
// Dates and amounts printed in the text
// ---------------------------------------------------------------------------

// checkTextFacts checks what the lawyer actually reads, not only what the
// model declared: every date and amount printed in a block, the title, a
// chronology row's date column and a bordereau row's date must rest on a
// claim whose sources state it. A figure no claim covers, or that its
// claim's sources do not state, is an « À vérifier » warning beside it —
// pièces are trusted, so this warns and never blocks. Legal references
// carry their own dates and are checked by the legal checks.
func (v *verifier) checkTextFacts() {
	seen := map[string]bool{}
	for _, iss := range v.issues {
		seen[iss.BlockID+"|"+iss.Code+"|"+iss.Correction] = true
	}
	warn := func(blockID, code string, evidence []string, sentence string) {
		key := blockID + "|" + code + "|" + sentence
		v.ran[types.CheckPieceClaims] = true
		if seen[key] {
			return // the claim's own check already said exactly this
		}
		seen[key] = true
		v.add(types.CheckPieceClaims, code, types.SeverityWarning, blockID, "", evidence, sentence)
	}

	// Headings and the title cite nothing themselves: any source the
	// document cites, or the lawyer's own words, may state their figures.
	var docClaims []types.DocumentClaim
	for _, b := range v.body.Blocks {
		docClaims = append(docClaims, b.Claims...)
	}
	checkLoose := func(blockID, text string) {
		sup := v.supportOf(docClaims, true)
		for _, m := range v.mentions(text) {
			v.count(types.CheckPieceClaims, 1)
			if !sup.states(m) {
				code, sentence := textFactWarning(m, nil, false)
				warn(blockID, code, nil, sentence)
			}
		}
	}
	checkLoose("", v.rev.Title)

	for _, b := range v.body.Blocks {
		switch b.Role {
		case types.BlockLimitation, types.BlockWarning:
			continue
		case types.BlockHeading:
			checkLoose(b.ID, b.Text)
			continue
		}
		for _, m := range v.mentions(b.Text) {
			v.count(types.CheckPieceClaims, 1)
			var covering []types.DocumentClaim
			for _, c := range b.Claims {
				if c.Start < m.End && m.Start < c.End {
					covering = append(covering, c)
				}
			}
			sup := v.supportOf(covering, false)
			if b.Role == types.BlockBordereauRow && b.Bordereau != nil {
				// A bordereau row lists the pièce by its name: what the name
				// and the pièce's own date say is its support.
				p := v.scope.byID[b.Bordereau.PieceID]
				sup.texts = append(sup.texts, pieceTitle(p))
				if m.Date != nil && pieceDated(p, *m.Date) {
					continue
				}
			}
			if !sup.states(m) {
				code, sentence := textFactWarning(m, sup.pieces, sup.lawyer)
				warn(b.ID, code, sup.pieceSpanIDs(), sentence)
			}
		}

		var rowDate *types.PartialDate
		switch {
		case b.Role == types.BlockChronologyRow && b.Chronology != nil:
			d := b.Chronology.Date.PartialDate
			rowDate = &d
		case b.Role == types.BlockBordereauRow && b.Bordereau != nil && b.Bordereau.Date != nil:
			rowDate = b.Bordereau.Date
		}
		if rowDate == nil || rowDate.Precision == types.DatePrecisionUnknown {
			continue
		}
		v.count(types.CheckPieceClaims, 1)
		m := TextMention{Date: rowDate}
		sup := v.supportOf(b.Claims, false)
		if sup.states(m) {
			continue
		}
		if b.Role == types.BlockBordereauRow && pieceDated(v.scope.byID[b.Bordereau.PieceID], *rowDate) {
			continue // the date the pièce itself carries
		}
		code, sentence := textFactWarning(m, sup.pieces, sup.lawyer)
		warn(b.ID, code, sup.pieceSpanIDs(), sentence)
	}
}

// mentions are a text's dates and amounts, less those inside a legal
// reference (« loi du 6 juillet 1989 », « Soc., 25 septembre 2024 »).
func (v *verifier) mentions(text string) []TextMention {
	refs := FindLegalReferences(text)
	var out []TextMention
	for _, m := range FindTextMentions(text) {
		inRef := false
		for _, r := range refs {
			if r.Start < m.End && m.Start < r.End {
				inRef = true
				break
			}
		}
		if !inRef {
			out = append(out, m)
		}
	}
	return out
}

// claimSupport is everything a set of claims rests on.
type claimSupport struct {
	pieces []PieceEvidence // in-scope pièce passages, for the attribution
	texts  []string        // every passage, legal text, web excerpt and lawyer word cited
	amount []types.Money   // results and operands of the cited calculations
	lawyer bool            // a claim rests on the lawyer's own words
}

func (s claimSupport) states(m TextMention) bool {
	if MentionStatedIn(m, s.texts...) {
		return true
	}
	if m.Money != nil {
		for _, a := range s.amount {
			if a.MinorUnits == m.Money.MinorUnits && a.Currency == m.Money.Currency {
				return true
			}
		}
	}
	return false
}

func (s claimSupport) pieceSpanIDs() []string {
	var ids []string
	for _, p := range s.pieces {
		ids = append(ids, p.SpanID)
	}
	return ids
}

// supportOf collects what claims cite. loose also admits the lawyer's own
// words, for a heading or a title that no claim covers.
func (v *verifier) supportOf(claims []types.DocumentClaim, loose bool) claimSupport {
	var s claimSupport
	seen := map[string]bool{}
	for _, c := range claims {
		for _, id := range claimEvidenceIDs(c) {
			span, ok := v.spans[id]
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			s.texts = append(s.texts, span.Quote)
			switch span.SourceType {
			case types.CitationSourcePiece:
				if span.PieceID != nil {
					if p, in := v.scope.byID[*span.PieceID]; in {
						s.pieces = append(s.pieces, pieceEvidenceOf(id, span, p))
					}
				}
			case types.CitationSourceLegifrance, types.CitationSourceJudilibre:
				if src := v.legalSourceOf(span); src != nil {
					s.texts = append(s.texts, src.Version.Text)
				}
			}
		}
		provided := c.Kind == types.ClaimLawyerProvided
		if provided || len(c.AnswerKeys) > 0 {
			s.lawyer = true
			s.texts = append(s.texts, v.lawyerTexts(c, provided)...)
		}
		if c.Kind == types.ClaimDerived && strings.TrimSpace(c.CalculationCallID) != "" {
			if calc, ok := v.calculation(strings.TrimSpace(c.CalculationCallID)); ok {
				if calc.money != nil {
					s.amount = append(s.amount, *calc.money)
				}
				s.amount = append(s.amount, calc.operands...)
			}
		}
	}
	if loose {
		s.texts = append(s.texts, v.lawyerTexts(types.DocumentClaim{}, true)...)
	}
	return s
}

// pieceDated reports whether a pièce's own recorded date is d, at d's
// precision.
func pieceDated(p types.Piece, d types.PartialDate) bool {
	if p.DocumentDate == nil {
		return false
	}
	t := time.Unix(*p.DocumentDate, 0).UTC()
	if t.Year() != d.Year {
		return false
	}
	switch d.Precision {
	case types.DatePrecisionYear:
		return true
	case types.DatePrecisionMonth:
		return int(t.Month()) == d.Month && p.DatePrecision != types.DatePrecisionYear
	case types.DatePrecisionDay:
		return int(t.Month()) == d.Month && t.Day() == d.Day && p.DatePrecision == types.DatePrecisionDay
	}
	return false
}

// textFactWarning is the « À vérifier » sentence for a printed date or amount
// its sources do not state.
func textFactWarning(m TextMention, pieces []PieceEvidence, lawyer bool) (string, string) {
	what, code := "", types.IssuePieceDateNotFound
	if m.Date != nil {
		what = "la date du " + FrenchPartialDate(*m.Date)
	} else {
		what, code = "le montant de "+FrenchMoney(*m.Money), types.IssuePieceAmountMismatch
	}
	e := "e" // French agreement: la date … retrouvée, le montant … retrouvé
	if m.Money != nil {
		e = ""
	}
	switch {
	case len(pieces) > 0:
		return code, fmt.Sprintf("À vérifier — %s attribué%s %s n'a pas été retrouvé%s dans %s.",
			what, e, pieceAttribution(pieces), e, passagePhrase(pieces))
	case lawyer:
		return types.IssuePieceMetadataOnlySupport, fmt.Sprintf(
			"À vérifier — %s, présenté%s comme fourni%s par l'avocat, ne figure ni dans ses messages ni dans ses réponses.", what, e, e)
	}
	return types.IssuePieceMetadataOnlySupport, fmt.Sprintf(
		"À vérifier — %s n'est rattaché%s à aucun passage de pièce lu.", what, e)
}

// lawyerStated reports whether a lawyer_provided claim's dates and amounts
// appear in what the lawyer actually wrote: the thread, the instruction, the
// answers. A claim with neither asserts nothing checkable.
func (v *verifier) lawyerStated(c types.DocumentClaim) bool {
	texts := append([]string(nil), v.lawyerText...)
	for _, a := range v.answers {
		texts = append(texts, a.text...)
	}
	for _, d := range c.Dates {
		if !DateStatedIn(d.PartialDate, texts...) {
			return false
		}
	}
	for _, m := range c.Amounts {
		if !AmountStatedIn(m, texts...) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Legal citations
// ---------------------------------------------------------------------------

// spanUse is where a span is first cited.
type spanUse struct{ blockID, claimID string }

func (v *verifier) firstUses(sourceTypes ...string) ([]string, map[string]spanUse) {
	want := map[string]bool{}
	for _, t := range sourceTypes {
		want[t] = true
	}
	uses := map[string]spanUse{}
	var order []string
	for _, b := range v.body.Blocks {
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				span, ok := v.spans[id]
				if !ok || !want[span.SourceType] {
					continue
				}
				if _, seen := uses[id]; !seen {
					uses[id] = spanUse{b.ID, c.ID}
					order = append(order, id)
				}
			}
		}
	}
	return order, uses
}

func (v *verifier) legalSourceOf(span types.AgentEvidenceSpan) *AgentLegalSource {
	if span.LegalVersionID == nil {
		return nil
	}
	return v.legal[*span.LegalVersionID]
}

func (v *verifier) checkLegalSpans() {
	order, uses := v.firstUses(types.CitationSourceLegifrance, types.CitationSourceJudilibre)
	now := time.Now().UTC()
	for _, id := range order {
		span := v.spans[id]
		v.count(types.CheckLegalCitations, 1)
		basis := v.temporalBasis(span, now)
		for _, iss := range CheckAgentLegalSpan(span, v.legalSourceOf(span), basis) {
			v.block(types.CheckLegalCitations, iss.Code, uses[id].blockID, uses[id].claimID, []string{id}, iss.Correction)
		}
	}
	// A claim's own quotation of a legal source must occur in that version.
	for _, b := range v.body.Blocks {
		for _, c := range b.Claims {
			for _, q := range c.Quotes {
				span, ok := v.spans[q.EvidenceID]
				if !ok || (span.SourceType != types.CitationSourceLegifrance && span.SourceType != types.CitationSourceJudilibre) {
					continue
				}
				src := v.legalSourceOf(span)
				if src != nil && !QuoteOccursIn(q.Text, src.Version.Text) {
					v.block(types.CheckLegalCitations, types.IssueLegalQuoteNotFound, b.ID, c.ID, []string{q.EvidenceID},
						"Cette citation n'apparaît pas dans le texte de la version citée : reprenez le texte exact.")
				}
			}
		}
	}
}

// temporalBasis resolves what a legal span is checked against: the
// contract's statute period if it states dates, else the date the span was
// retrieved against, else today — a version cited with no date at all must
// be the one in force now. The case-law cutoff is independent.
func (v *verifier) temporalBasis(span types.AgentEvidenceSpan, now time.Time) LegalTemporalBasis {
	var b LegalTemporalBasis
	if sa := v.contract.StatuteApplicability; sa != nil {
		if s, _, ok := partialDateRange(sa.From); ok {
			b.ApplicabilityFrom = &s
		}
		if _, e, ok := partialDateRange(sa.To); ok {
			b.ApplicabilityTo = &e
		}
		if sa.From != nil && sa.To == nil {
			if _, e, ok := partialDateRange(sa.From); ok {
				b.ApplicabilityTo = &e
			}
		}
	}
	if b.ApplicabilityFrom == nil && b.ApplicabilityTo == nil {
		day := now
		basis := "now|" + now.Format("2006-01-02")
		if span.AsOfDate != nil {
			day = time.Unix(*span.AsOfDate, 0).UTC()
			basis = "asof|" + strconv.FormatInt(*span.AsOfDate, 10)
		}
		start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		s, e := start.Unix(), start.AddDate(0, 0, 1).Unix()
		b.ApplicabilityFrom, b.ApplicabilityTo = &s, &e
		v.evidence = append(v.evidence, "basis|"+span.ID+"|"+basis)
	}
	if _, e, ok := partialDateRange(v.contract.CaseLawCutoff); ok {
		b.CaseLawCutoff = &e
	}
	return b
}

// checkOfficialSources is the final gate's official revalidation (§7.3).
// Each legal span that passed the stored checks is fetched from its official
// source by exact identity and must come back the same version, with the
// same text, applicable to the period requested. A stored-only match is not
// enough to publish: a mismatch blocks, an unreachable source is a blocking
// issue marked retryable when the source may answer later, and once one call
// finds the source unreachable the others are not attempted. Nothing is
// substituted for the version cited.
func (v *verifier) checkOfficialSources() error {
	order, uses := v.firstUses(types.CitationSourceLegifrance, types.CitationSourceJudilibre)
	flagged := map[string]bool{}
	for _, iss := range v.issues {
		for _, id := range iss.EvidenceIDs {
			flagged[id] = true
		}
	}
	now := time.Now().UTC()
	var down *OfficialOutcome
	for _, id := range order {
		span := v.spans[id]
		src := v.legalSourceOf(span)
		if src == nil || flagged[id] {
			continue // already refused on the stored source
		}
		var out OfficialOutcome
		if down != nil {
			out = *down
		} else {
			out = RevalidateOfficial(v.ctx, v.sources, *src, v.temporalBasis(span, now))
			if err := v.ctx.Err(); err != nil {
				return err
			}
			if out.Status == OfficialUnavailable {
				down = &out
			}
		}
		use := uses[id]
		switch out.Status {
		case OfficialMatched:
			v.official = append(v.official, types.OfficialCheck{
				EvidenceID: id, LegalVersionID: src.Version.ID, OfficialID: out.OfficialID,
				TextSHA256: out.TextSHA256, CheckedAt: now.Unix(),
			})
			v.evidence = append(v.evidence, "official|"+id+"|"+out.OfficialID+"|"+out.TextSHA256)
		case OfficialUnavailable:
			v.ran[types.CheckLegalCitations] = true
			v.issues = append(v.issues, types.VerificationIssue{
				Code: types.IssueLegalOfficialDown, Severity: types.SeverityBlocking, Check: types.CheckLegalCitations,
				BlockID: use.blockID, ClaimID: use.claimID, EvidenceIDs: []string{id},
				Correction: out.Correction, Retryable: out.Retryable,
			})
		default:
			v.block(types.CheckLegalCitations, types.IssueLegalOfficialMismatch, use.blockID, use.claimID, []string{id}, out.Correction)
		}
	}
	if len(order) > 0 {
		verificationLog.Infof("Official revalidation (run=%s revision=%s spans=%d matched=%d unavailable=%t)",
			v.run.ID, v.rev.ID, len(order), len(v.official), down != nil)
	}
	return nil
}

// partialDateRange is the half-open UTC interval a partial date covers.
func partialDateRange(d *types.PartialDate) (start, end int64, ok bool) {
	if d == nil || !validPartialDate(*d) {
		return 0, 0, false
	}
	var t, next time.Time
	switch d.Precision {
	case types.DatePrecisionDay:
		t = time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC)
		next = t.AddDate(0, 0, 1)
	case types.DatePrecisionMonth:
		t = time.Date(d.Year, time.Month(d.Month), 1, 0, 0, 0, 0, time.UTC)
		next = t.AddDate(0, 1, 0)
	case types.DatePrecisionYear:
		t = time.Date(d.Year, 1, 1, 0, 0, 0, 0, time.UTC)
		next = t.AddDate(1, 0, 0)
	default:
		return 0, 0, false
	}
	return t.Unix(), next.Unix(), true
}

// checkLegalProse refuses any legal reference written in the text that no
// verified legal citation backs. In a body block the reference must sit
// inside a claim citing the matching source; in a heading or the title it
// must be cited somewhere in the document.
func (v *verifier) checkLegalProse() {
	keysOf := func(c types.DocumentClaim) map[string]bool {
		keys := map[string]bool{}
		for _, id := range claimEvidenceIDs(c) {
			if span, ok := v.spans[id]; ok {
				if src := v.legalSourceOf(span); src != nil {
					for k := range LegalSourceKeys(*src) {
						keys[k] = true
					}
				}
			}
		}
		return keys
	}
	docKeys := map[string]bool{}
	for _, b := range v.body.Blocks {
		for _, c := range b.Claims {
			for k := range keysOf(c) {
				docKeys[k] = true
			}
		}
	}
	refuse := func(blockID string, ref LegalReference) {
		v.block(types.CheckLegalCitations, types.IssueLegalReferenceUncited, blockID, "", nil, fmt.Sprintf(
			"La référence « %s » figure dans le texte sans citation vérifiée : rattachez-la à la source retrouvée qui la porte (read_law, read_decision), ou retirez-la.",
			ref.Display))
	}
	for _, ref := range FindLegalReferences(v.rev.Title) {
		v.count(types.CheckLegalCitations, 1)
		if !docKeys[ref.Key] {
			refuse("", ref)
		}
	}
	for _, b := range v.body.Blocks {
		for _, ref := range FindLegalReferences(b.Text) {
			v.count(types.CheckLegalCitations, 1)
			if b.Role == types.BlockHeading {
				if !docKeys[ref.Key] {
					refuse(b.ID, ref)
				}
				continue
			}
			covered := false
			for _, c := range b.Claims {
				if c.Start < ref.End && ref.Start < c.End && keysOf(c)[ref.Key] {
					covered = true
					break
				}
			}
			if !covered {
				refuse(b.ID, ref)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Web
// ---------------------------------------------------------------------------

func (v *verifier) checkWebSpans() {
	order, uses := v.firstUses(types.CitationSourceWeb)
	for _, id := range order {
		span := v.spans[id]
		use := uses[id]
		v.count(types.CheckWebProvenance, 1)
		var src types.AgentWebSource
		ok := false
		if span.WebSourceID != nil {
			src, ok = v.web[*span.WebSourceID]
		}
		var loc types.SourceLocator
		_ = json.Unmarshal(span.Locator, &loc)
		if !ok || (loc.URL != "" && loc.URL != src.URL) {
			v.block(types.CheckWebProvenance, types.IssueWebURLNotReturned, use.blockID, use.claimID, []string{id},
				"Cette adresse web n'a pas été retournée par search_web pendant ce tour : retirez-la.")
			continue
		}
		if span.SourceSHA256 != src.ContentSHA256 {
			v.block(types.CheckWebProvenance, types.IssueSourceMutated, use.blockID, use.claimID, []string{id},
				"Le contenu enregistré de cette source web ne correspond plus au passage cité.")
		}
		if strings.TrimSpace(src.Content) == "" || strings.TrimSpace(span.Quote) == "" || !QuoteOccursIn(span.Quote, src.Content) {
			v.block(types.CheckWebProvenance, types.IssueWebExcerptMissing, use.blockID, use.claimID, []string{id},
				"Une source web sans extrait retourné n'est qu'une piste, pas un appui : citez un extrait retourné par search_web, ou retirez cette source de l'affirmation.")
		}
	}
	for _, b := range v.body.Blocks {
		for _, c := range b.Claims {
			for _, q := range c.Quotes {
				span, ok := v.spans[q.EvidenceID]
				if ok && span.SourceType == types.CitationSourceWeb && !QuoteOccursIn(q.Text, span.Quote) {
					v.block(types.CheckWebProvenance, types.IssueWebExcerptMissing, b.ID, c.ID, []string{q.EvidenceID},
						"Cette citation n'apparaît pas dans l'extrait web retourné : reprenez le texte exact de l'extrait.")
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Compétence
// ---------------------------------------------------------------------------

func (v *verifier) checkCompetence() error {
	pin := v.contract.Competence
	if pin == nil && v.rev.Kind == types.AgentOutputActe {
		// Owner decision 5: no acte without the lawyer's compétence.
		v.count(types.CheckCompetence, 1)
		v.block(types.CheckCompetence, types.IssueCompetenceSection, "", "", nil,
			"Un acte ne se rédige que sous la compétence choisie par l'avocat pour cette demande, et aucune ne l'est : retirez l'acte de la soumission.")
		return nil
	}
	if pin == nil || v.rev.Kind == types.AgentOutputAnswer || v.rev.Kind == types.AgentOutputBordereau {
		return nil
	}
	v.count(types.CheckCompetence, 1)
	var sv types.SkillVersion
	err := v.repo.SkillVersions().Where("id = ?", pin.SkillVersionID).Take(&sv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		v.block(types.CheckCompetence, types.IssueCompetenceSection, "", "", nil,
			"La compétence retenue pour ce tour est introuvable : le document ne peut pas être contrôlé contre elle.")
		return nil
	}
	if err != nil {
		return err
	}
	var def skillDefinition
	if err := json.Unmarshal(sv.Definition, &def); err != nil {
		return fmt.Errorf("skill version %s: unreadable definition: %w", sv.ID, err)
	}
	v.evidence = append(v.evidence, "competence|"+sv.ID)

	var headings, all []string
	for _, b := range v.body.Blocks {
		all = append(all, b.Text)
		if b.Role == types.BlockHeading {
			headings = append(headings, normalizeForQuoteMatch(foldForMatch(b.Text)))
		}
	}
	for _, s := range def.Sections {
		if !s.Required {
			continue
		}
		v.count(types.CheckCompetence, 1)
		want := normalizeForQuoteMatch(foldForMatch(s.Title))
		found := false
		for _, h := range headings {
			if want != "" && strings.Contains(h, want) {
				found = true
				break
			}
		}
		if !found {
			v.block(types.CheckCompetence, types.IssueCompetenceSection, "", "", nil,
				fmt.Sprintf("Ajoutez la section obligatoire « %s » (un titre de section) prévue par la compétence.", s.Title))
		}
	}
	docText := strings.Join(all, "\n")
	for _, cl := range def.MandatoryClauses {
		v.count(types.CheckCompetence, 1)
		if !QuoteOccursIn(cl.Text, docText) {
			v.block(types.CheckCompetence, types.IssueCompetenceClause, "", "", nil,
				fmt.Sprintf("Reprenez la clause obligatoire « %s » prévue par la compétence : « %s »", cl.Label, cl.Text))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// ComputeCoverage is the run's reading coverage, computed from the
// read-receipt ledger against the turn's frozen scope — never from model
// output. It proves source text was supplied to the agent, not understood.
func ComputeCoverage(ctx context.Context, repo *u.TenantRepo, runID string) (types.CoverageReport, error) {
	var turn types.AgentTurn
	if err := repo.AgentTurns().Where("run_id = ?", runID).Take(&turn).Error; err != nil {
		return types.CoverageReport{}, notFoundOr(err)
	}
	var contract types.AgentTaskContract
	if err := json.Unmarshal(turn.TaskContract, &contract); err != nil {
		return types.CoverageReport{}, fmt.Errorf("agent turn %s: unreadable task contract: %w", turn.ID, err)
	}
	scope, err := loadScope(repo, contract)
	if err != nil {
		return types.CoverageReport{}, err
	}
	report, _, err := computeCoverage(ctx, repo, runID, contract, scope)
	return report, err
}

type rangeRun struct{ start, end int }

func computeCoverage(ctx context.Context, repo *u.TenantRepo, runID string, contract types.AgentTaskContract,
	scope scopeState,
) (types.CoverageReport, []string, error) {
	report := types.CoverageReport{
		RunID:       runID,
		ScopeSHA256: scope.sha,
		Exhaustive:  contract.Exhaustive,
		TotalPieces: len(scope.pieces) + len(scope.missing),
		Pieces:      []types.PieceCoverage{},
		ComputedAt:  time.Now().Unix(),
	}

	var receipts []types.AgentReadReceipt
	if err := repo.AgentReadReceipts().Where("run_id = ? AND consumed_dispatch IS NOT NULL", runID).
		Order("id ASC").Find(&receipts).Error; err != nil {
		return report, nil, err
	}
	byPiece := map[string][]types.AgentReadReceipt{}
	receiptIDs := make([]string, 0, len(receipts))
	for _, r := range receipts {
		byPiece[r.PieceID] = append(byPiece[r.PieceID], r)
		receiptIDs = append(receiptIDs, r.ID)
	}

	// An empty scope is vacuously covered; any piece needs its manifest.
	complete := (PieceManifestFor != nil || len(scope.pieces) == 0) && len(scope.missing) == 0
	for _, p := range scope.pieces {
		pc := types.PieceCoverage{PieceID: p.ID, Ordinal: p.Ordinal, Name: pieceTitle(p)}
		mine := byPiece[p.ID]
		if PieceManifestFor == nil {
			pc.State = types.CoverageUnread
			if len(mine) > 0 {
				pc.State = types.CoveragePartial
			}
			report.Pieces = append(report.Pieces, pc)
			continue
		}
		m, err := PieceManifestFor(ctx, repo, p)
		if err != nil {
			return report, nil, fmt.Errorf("piece %s: manifest: %w", p.ID, err)
		}
		pc.Unit = m.Unit
		switch m.State {
		case types.CoveragePending:
			pc.State = types.CoveragePending
			pc.Segments.Pending = max(len(m.Segments), 1)
		case types.CoverageUnreadable:
			pc.State = types.CoverageUnreadable
			pc.Segments.Unreadable = max(len(m.Segments), 1)
		default:
			// Only receipts of the current extraction count; older ones prove
			// nothing about today's text, and are reported unless re-read.
			ranges := map[string][]rangeRun{}
			stale := false
			for _, r := range mine {
				if r.SourceSHA256 != m.SourceSHA256 || r.ExtractionVersion != m.ExtractionVersion {
					stale = true
					continue
				}
				ranges[r.SegmentID] = append(ranges[r.SegmentID], rangeRun{r.StartOffset, r.EndOffset})
			}
			for _, seg := range m.Segments {
				if !seg.Readable {
					pc.Segments.Unreadable++
					continue
				}
				pc.Segments.Expected++
				if rangesCover(ranges[seg.ID], seg.Runes) {
					pc.Segments.Consumed++
				} else {
					pc.Segments.Missing++
				}
			}
			switch {
			case pc.Segments.Expected == 0 && pc.Segments.Unreadable > 0:
				pc.State = types.CoverageUnreadable
			case pc.Segments.Missing == 0 && pc.Segments.Unreadable == 0:
				pc.State = types.CoverageComplete
			case pc.Segments.Consumed > 0:
				pc.State = types.CoveragePartial
			default:
				pc.State = types.CoverageUnread
			}
			pc.Mutated = stale && pc.State != types.CoverageComplete
		}
		if pc.State != types.CoverageComplete {
			complete = false
		} else {
			report.FullyRead++
		}
		report.Segments.Expected += pc.Segments.Expected
		report.Segments.Consumed += pc.Segments.Consumed
		report.Segments.Missing += pc.Segments.Missing
		report.Segments.Pending += pc.Segments.Pending
		report.Segments.Unreadable += pc.Segments.Unreadable
		report.Pieces = append(report.Pieces, pc)
	}
	for _, id := range scope.missing {
		report.Pieces = append(report.Pieces, types.PieceCoverage{PieceID: id, State: types.CoverageUnreadable})
	}
	for _, p := range scope.excluded {
		report.Pieces = append(report.Pieces, types.PieceCoverage{
			PieceID: p.ID, Ordinal: p.Ordinal, Name: pieceTitle(p), State: types.CoverageExcluded,
		})
	}
	report.Complete = complete
	return report, receiptIDs, nil
}

// rangesCover reports whether the union of half-open ranges covers
// [0, length). Overlapping reads count once.
func rangesCover(rs []rangeRun, length int) bool {
	if length <= 0 {
		return len(rs) > 0
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	reached := 0
	for _, r := range rs {
		if r.start > reached {
			return false
		}
		reached = max(reached, r.end)
		if reached >= length {
			return true
		}
	}
	return reached >= length
}

func (v *verifier) checkCoverage() error {
	if !v.contract.Exhaustive {
		return nil
	}
	report, receiptIDs, err := computeCoverage(v.ctx, v.repo, v.run.ID, v.contract, v.scope)
	if err != nil {
		return err
	}
	v.coverage = &report
	v.count(types.CheckCoverage, report.TotalPieces)
	for _, id := range receiptIDs {
		v.evidence = append(v.evidence, "receipt|"+id)
	}
	if report.Complete {
		return nil
	}

	var notRead []string
	pending := false
	for _, p := range report.Pieces {
		if p.State == types.CoverageComplete || p.State == types.CoverageExcluded {
			continue
		}
		pending = pending || p.State == types.CoveragePending
		if p.Ordinal > 0 {
			notRead = append(notRead, strconv.Itoa(p.Ordinal))
		} else {
			notRead = append(notRead, "« "+firstNonEmpty(p.Name, "sans titre")+" »")
		}
	}
	code := types.IssueCoverageIncomplete
	if pending {
		code = types.IssueCoveragePending
	}
	missing := len(notRead)
	which := ""
	if missing > 0 {
		which = " (n° " + joinFrench(notRead) + ")"
	}
	if PieceManifestFor == nil {
		which = " — la couverture de lecture ne peut pas être mesurée dans cette configuration"
	}

	labelled := false
	for _, b := range v.body.Blocks {
		if b.Role == types.BlockLimitation {
			labelled = true
			break
		}
	}
	if labelled {
		v.add(types.CheckCoverage, code, types.SeverityWarning, "", "", nil, fmt.Sprintf(
			"Résultat incomplet : %d pièce(s) sur %d n'ont pas été lues intégralement%s. Ce document ne peut pas être présenté comme exhaustif.",
			missing, report.TotalPieces, which))
		return nil
	}
	v.block(types.CheckCoverage, code, "", "", nil, fmt.Sprintf(
		"La demande est exhaustive mais %d pièce(s) sur %d n'ont pas été lues intégralement%s : lisez-les (read_piece), ou présentez le résultat comme incomplet dans un bloc « limitation » qui dit ce qui manque.",
		missing, report.TotalPieces, which))
	return nil
}

// ---------------------------------------------------------------------------
// Final-only checks
// ---------------------------------------------------------------------------

func (v *verifier) checkDecisions() {
	for _, d := range v.contract.RequiredDecisions {
		v.count(types.CheckLawyerDecisions, 1)
		v.evidence = append(v.evidence, "decision|"+d.Key+"|"+d.State)
		if d.State == types.DecisionOpen || d.State == types.DecisionAsked {
			v.block(types.CheckLawyerDecisions, types.IssueLawyerDecisionOpen, "", "", nil,
				fmt.Sprintf("La décision « %s » appartient à l'avocat et reste sans réponse : posez-lui la question (ask_lawyer).",
					firstNonEmpty(d.Description, d.Key)))
		}
	}
}

// checkReview requires the review cycle of §7.4 for substantive documents:
// this exact content was verified by verify_output, and a later controller
// decision was accepted — the agent saw the findings before submitting. An
// edit changes the content hash and needs a fresh cycle. The answer itself is
// exempt.
func (v *verifier) checkReview() error {
	if v.rev.Kind == types.AgentOutputAnswer {
		return nil
	}
	v.count(types.CheckReview, 1)
	var first types.AgentVerification
	err := v.repo.AgentVerifications().
		Where("document_revision_id = ? AND content_sha256 = ? AND verifier_version = ?", v.rev.ID, v.rev.ContentSHA256, VerifierVersion).
		Order("created_at ASC").Take(&first).Error
	seen := false
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		return err
	default:
		var n int64
		if err := v.repo.LLMCalls().
			Where("run_id = ? AND agent_dispatch IS NOT NULL AND state = ? AND started_at >= ?",
				v.run.ID, types.LLMCallStateSucceeded, first.CreatedAt).Count(&n).Error; err != nil {
			return err
		}
		seen = n > 0
		v.evidence = append(v.evidence, fmt.Sprintf("review|%s|%t", first.ID, seen))
	}
	if !seen {
		v.block(types.CheckReview, types.IssueReviewMissing, "", "", nil,
			"Vérifiez cette révision (verify_output) et examinez le résultat avant de la soumettre ; toute modification demande une nouvelle vérification.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

var checkOrder = []string{
	types.CheckOwnership, types.CheckStructure, types.CheckCompetence, types.CheckPieceClaims,
	types.CheckLegalCitations, types.CheckWebProvenance, types.CheckCoverage,
	types.CheckLawyerDecisions, types.CheckReview, types.CheckArtifactFidelity,
}

func (v *verifier) report() types.VerificationReport {
	version := VerifierVersion
	if v.in.Final {
		version = VerifierVersionFinal
	}
	r := types.VerificationReport{
		VerifierVersion:    version,
		DocumentRevisionID: v.rev.ID,
		ContentSHA256:      v.rev.ContentSHA256,
		ScopeSHA256:        v.scope.sha,
		Issues:             v.issues,
		Coverage:           v.coverage,
		Official:           v.official,
		CheckedAt:          time.Now().Unix(),
	}
	if r.Issues == nil {
		r.Issues = []types.VerificationIssue{}
	}
	if r.ScopeSHA256 == "" {
		r.ScopeSHA256 = sha256Hex("scope-unchecked")
	}
	worst := map[string]string{}
	for _, iss := range v.issues {
		if iss.Severity == types.SeverityBlocking {
			r.Blocking++
			worst[iss.Check] = types.CheckBlocked
		} else {
			r.Warnings++
			if worst[iss.Check] != types.CheckBlocked {
				worst[iss.Check] = types.CheckWarning
			}
		}
	}
	for _, c := range checkOrder {
		status := types.CheckNotApplicable
		if v.ran[c] {
			status = firstNonEmpty(worst[c], types.CheckPassed)
		}
		r.Checks = append(r.Checks, types.VerificationCheck{Check: c, Status: status, Checked: v.checked[c]})
	}
	switch {
	case r.Blocking > 0:
		r.Result = types.VerificationBlocked
	case r.Warnings > 0:
		r.Result, r.DownloadAllowed = types.VerificationWarnings, true
	default:
		r.Result, r.DownloadAllowed = types.VerificationPassed, true
	}

	// The evidence hash covers every input the result depends on beyond the
	// content and the scope, so StoreVerification never returns a stale row
	// for a changed input.
	parts := append([]string(nil), v.evidence...)
	sort.Strings(parts)
	issues, _ := json.Marshal(r.Issues)
	parts = append(parts, "issues|"+sha256Hex(string(issues)))
	r.EvidenceSHA256 = sha256Hex(strings.Join(parts, "\n"))
	return r
}

// IssuesSHA256 is the hash of a report's issues — the "warning report" part
// of a publication's identity. A final report and the verify_output report
// of the same content carry the same issues when both pass.
func IssuesSHA256(r types.VerificationReport) string {
	raw, _ := json.Marshal(r.Issues)
	return sha256Hex(string(raw))
}
