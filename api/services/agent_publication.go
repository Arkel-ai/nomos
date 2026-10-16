package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var publicationLog = u.NewLogger("services.agent_publication")

// Agent rendering and publication.
//
// PrepareArtifact renders a VERIFIED revision (a stored revision paired with
// its matching, download-allowing verification), checks the DOCX against the
// revision, and stages the bytes at a private key derived from the
// publication identity. Nothing is visible yet: no artifact row, no URL.
//
// PublishResult then rechecks every hash, the scope, the actor and
// cancellation, requires every legal citation to carry the final gate's
// official revalidation, re-reads the staged bytes, and inside the runner's fenced
// transaction inserts — or, on a retry, reuses — the artifact rows, the
// citation bindings, the frozen answer and artifact.ready. The publication
// key (tenant, run, revision hash, format, template and renderer, issues
// hash) is what makes a retry converge on one row instead of two.
//
// DownloadArtifact then serves an agent artifact only while a final,
// hash-matching verification backs it. Pièce warnings allow download; a
// blocked report never reaches publication.

// ErrPublicationRefused names every refusal: a stale hash, a non-final or
// blocked report, a scope that moved, a cancelled run, staged bytes that do
// not match. Nothing is published.
var ErrPublicationRefused = errors.New("publication refused")

// errPublicationScopeChanged is the refusal of a result verified over a
// dossier that has moved since (a pièce added, removed or replaced): a race,
// not a defect — a new submission is verified over the dossier as it is.
var errPublicationScopeChanged = fmt.Errorf("%w: the scope changed since it was verified", ErrPublicationRefused)

func init() {
	AgentArtifacts = agentPublisher{}
	AgentPublisher = agentPublisher{}
}

type agentPublisher struct{}

// Server-built artifacts a publication may carry without a document
// revision: their content comes from piece rows, not from model text.
var serverBuiltKinds = map[string]bool{
	types.ArtifactKindExportZip:     true,
	types.ArtifactKindBordereauDocx: true,
}

// publicationKey is the stable identity of one publication.
func publicationKey(tenantID, runID, contentSHA256 string, format ArtifactFormat, templateVersion, rendererVersion, issuesSHA256 string) string {
	h := sha256.New()
	for _, part := range []string{tenantID, runID, contentSHA256, string(format), templateVersion, rendererVersion, issuesSHA256} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// tenantKeyPrefix is the storage prefix every key of a tenant starts with.
func tenantKeyPrefix(tenantID string) string {
	return strings.TrimSuffix(u.DossierKey(tenantID, "d", "f"), "d/f")
}

func agentStagingKey(tenantID string, run types.Run, pubKey string) string {
	return u.DossierKey(tenantID, firstNonEmpty(run.DossierID, "run-"+run.ID), "agent-"+pubKey+".docx")
}

// ---------------------------------------------------------------------------
// PrepareArtifact
// ---------------------------------------------------------------------------

func (agentPublisher) PrepareArtifact(ctx context.Context, in VerifiedRevision, format ArtifactFormat) (StagedArtifact, error) {
	if format != ArtifactFormatDOCX {
		return StagedArtifact{}, fmt.Errorf("%w: format %q is not renderable", ErrPublicationRefused, format)
	}
	if u.DB == nil {
		return StagedArtifact{}, u.ErrAuthNotConfigured
	}
	if u.Blob == nil {
		return StagedArtifact{}, fmt.Errorf("%w: storage not configured", ErrPublicationRefused)
	}
	// Never trust the struct the caller hands over: reload both rows.
	vr, err := LoadVerifiedRevision(ctx, in.Revision.TenantID, in.Revision.RunID, in.Verification.ID)
	if err != nil {
		return StagedArtifact{}, err
	}
	if vr.Revision.ID != in.Revision.ID {
		return StagedArtifact{}, fmt.Errorf("%w: verification is for another revision", ErrNotVerified)
	}
	tenantID, runID := vr.Revision.TenantID, vr.Revision.RunID
	repo := u.ForTenantID(ctx, tenantID)
	var run types.Run
	if err := repo.Runs().Where("id = ?", runID).Take(&run).Error; err != nil {
		return StagedArtifact{}, notFoundOr(err)
	}

	var body types.DocumentRevisionBody
	if err := json.Unmarshal(vr.Revision.Body, &body); err != nil {
		return StagedArtifact{}, fmt.Errorf("%w: unreadable revision body", ErrNotVerified)
	}
	srcs, err := loadSpanSources(ctx, repo, runID, revisionEvidenceOrder(body))
	if err != nil {
		return StagedArtifact{}, err
	}
	labels := make(map[string]string, len(srcs))
	for id, s := range srcs {
		labels[id] = s.label()
	}

	res, err := RenderVerifiedDocument(ctx, vr, labels)
	if err != nil {
		return StagedArtifact{}, err
	}
	if err := CheckDocxFidelity(res.Bytes, res.Expected); err != nil {
		publicationLog.Errorf("Rendered document refused by the fidelity check (run=%s revision=%s): %v", runID, vr.Revision.ID, err)
		return StagedArtifact{}, err
	}

	var report types.VerificationReport
	if err := json.Unmarshal(vr.Verification.Report, &report); err != nil {
		return StagedArtifact{}, fmt.Errorf("%w: unreadable verification report", ErrNotVerified)
	}
	renderer := res.RendererVersion + " ref:" + res.ReferenceSHA256[:12]
	key := publicationKey(tenantID, runID, vr.Revision.ContentSHA256, format, res.TemplateVersion, renderer, IssuesSHA256(report))
	staging := agentStagingKey(tenantID, run, key)

	// The object write survives a disconnect: the publication that follows
	// must find exactly these bytes.
	pubCtx := context.WithoutCancel(ctx)
	if err := u.Blob.Put(pubCtx, staging, bytes.NewReader(res.Bytes), int64(len(res.Bytes)), docxMime); err != nil {
		return StagedArtifact{}, fmt.Errorf("staging write failed: %w", err)
	}
	if err := stagedBytesMatch(pubCtx, staging, res.SHA256, int64(len(res.Bytes)), types.ArtifactKindNoteDocx); err != nil {
		return StagedArtifact{}, fmt.Errorf("%w: %v", ErrPublicationRefused, err)
	}

	revID, verID := vr.Revision.ID, vr.Verification.ID
	staged := StagedArtifact{
		Kind:               types.ArtifactKindNoteDocx,
		Name:               sanitizeFileSegment(firstNonEmpty(vr.Revision.Title, agentDocTitles[vr.Revision.Kind], "Document")) + ".docx",
		StagingKey:         staging,
		SHA256:             res.SHA256,
		Size:               int64(len(res.Bytes)),
		DocumentRevisionID: &revID,
		VerificationID:     &verID,
		PublicationKey:     key,
		TemplateVersion:    res.TemplateVersion,
		RendererVersion:    renderer,
		PieceWarnings:      res.PieceWarnings,
		Partial:            res.Partial,
	}
	publicationLog.Infof("Agent document staged (run=%s revision=%s bytes=%d warnings=%d partial=%t)",
		runID, revID, staged.Size, staged.PieceWarnings, staged.Partial)
	return staged, nil
}

// stagedBytesMatch re-reads a staged object and checks it is the package that
// was verified: same hash and size, and a valid package of its kind.
func stagedBytesMatch(ctx context.Context, key, sha string, size int64, kind string) error {
	rc, err := u.Blob.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("staged object unreadable: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxExportBytes+1))
	if err != nil {
		return fmt.Errorf("staged object unreadable: %v", err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != sha || int64(len(b)) != size {
		return errors.New("staged object does not match its hash")
	}
	if kind == types.ArtifactKindExportZip {
		if _, err := zip.NewReader(bytes.NewReader(b), int64(len(b))); err != nil {
			return errors.New("staged archive unreadable")
		}
		return nil
	}
	return ValidateOOXML(b)
}

// ---------------------------------------------------------------------------
// PublishResult
// ---------------------------------------------------------------------------

// plannedArtifact is a staged artifact whose checks passed, ready to commit.
type plannedArtifact struct {
	staged StagedArtifact
	rev    *VerifiedRevision // nil for a server-built artifact
	report types.VerificationReport
}

func (agentPublisher) PublishResult(ctx context.Context, in PublicationInput) (PublicationResult, error) {
	refuse := func(why string) (PublicationResult, error) {
		publicationLog.Warnf("Publication refused (run=%s): %s", in.RunID, why)
		return PublicationResult{}, fmt.Errorf("%w: %s", ErrPublicationRefused, why)
	}
	scopeMoved := func(what string) (PublicationResult, error) {
		publicationLog.Warnf("Publication refused (run=%s): the scope changed since the %s was verified", in.RunID, what)
		return PublicationResult{}, fmt.Errorf("%w (%s)", errPublicationScopeChanged, what)
	}
	if u.DB == nil {
		return PublicationResult{}, u.ErrAuthNotConfigured
	}
	if in.Fence == nil || in.TenantID == "" || in.RunID == "" {
		return refuse("incomplete publication input")
	}
	if in.Outcome != types.AgentOutcomeCompleted && in.Outcome != types.AgentOutcomePartial {
		return refuse("unknown outcome")
	}
	repo := u.ForTenantID(ctx, in.TenantID)
	var run types.Run
	if err := repo.Runs().Where("id = ?", in.RunID).Take(&run).Error; err != nil {
		return PublicationResult{}, notFoundOr(err)
	}
	var turn types.AgentTurn
	if err := repo.AgentTurns().Where("run_id = ?", in.RunID).Take(&turn).Error; err != nil {
		return PublicationResult{}, notFoundOr(err)
	}
	if turn.ActorID != in.ActorID {
		return refuse("actor is not the turn's actor")
	}
	if turn.AssistantMessageID == nil || *turn.AssistantMessageID != in.AssistantMessageID {
		return refuse("assistant message is not the turn's")
	}
	if run.CancelRequestedAt != nil {
		return refuse("run cancelled")
	}
	var contract types.AgentTaskContract
	if err := json.Unmarshal(turn.TaskContract, &contract); err != nil {
		return PublicationResult{}, fmt.Errorf("agent turn %s: unreadable task contract: %w", turn.ID, err)
	}
	scope, err := loadScope(repo, contract)
	if err != nil {
		return PublicationResult{}, err
	}

	// The answer: its own final verification, loaded and rechecked.
	answer, answerReport, err := finalVerified(repo, in.RunID, in.Answer.Revision.ID, in.Answer.Verification.ID)
	if err != nil {
		return refuse("answer: " + err.Error())
	}
	if answer.Revision.Kind != types.AgentOutputAnswer {
		return refuse("the answer revision is not of kind answer")
	}
	if answerReport.ScopeSHA256 != scope.sha {
		return scopeMoved("answer")
	}
	partial := coverageIncomplete(answerReport)

	var planned []plannedArtifact
	for _, st := range in.Artifacts {
		p := plannedArtifact{staged: st}
		if st.DocumentRevisionID != nil {
			vr, rep, err := finalVerified(repo, in.RunID, *st.DocumentRevisionID, "")
			if err != nil {
				return refuse("artifact: " + err.Error())
			}
			if rep.ScopeSHA256 != scope.sha {
				return scopeMoved("document")
			}
			want := publicationKey(in.TenantID, in.RunID, vr.Revision.ContentSHA256, ArtifactFormatDOCX,
				st.TemplateVersion, st.RendererVersion, IssuesSHA256(rep))
			if st.PublicationKey != want || st.StagingKey != agentStagingKey(in.TenantID, run, want) {
				return refuse("the staged document was not rendered from its final verification")
			}
			if coverageIncomplete(rep) {
				partial = true
				if !st.Partial {
					return refuse("an incomplete exhaustive document is not labelled partial")
				}
			}
			p.rev, p.report = &vr, rep
		} else {
			if !serverBuiltKinds[st.Kind] || st.PublicationKey == "" || len(st.PublicationKey) > 64 {
				return refuse("an artifact without a verified revision must be a server-built export")
			}
			if !strings.HasPrefix(st.StagingKey, tenantKeyPrefix(in.TenantID)) {
				return refuse("staged object outside the tenant prefix")
			}
		}
		if err := stagedBytesMatch(ctx, st.StagingKey, st.SHA256, st.Size, st.Kind); err != nil {
			return refuse(err.Error())
		}
		planned = append(planned, p)
	}
	if partial && in.Outcome != types.AgentOutcomePartial {
		return refuse("exhaustive work with incomplete coverage must be published as partial")
	}

	// Sources of every citation to bind, read before the transaction.
	var spanIDs []string
	bodies := map[string]types.DocumentRevisionBody{}
	for _, vr := range append([]VerifiedRevision{answer}, revisionsOf(planned)...) {
		var body types.DocumentRevisionBody
		if err := json.Unmarshal(vr.Revision.Body, &body); err != nil {
			return refuse("unreadable revision body")
		}
		bodies[vr.Revision.ID] = body
		spanIDs = append(spanIDs, revisionEvidenceOrder(body)...)
	}
	srcs, err := loadSpanSources(ctx, repo, in.RunID, spanIDs)
	if err != nil {
		return PublicationResult{}, err
	}
	// A legal citation is published only on a final report that revalidated
	// its exact version against the official source (§7.3). A report without
	// that round-trip — an older verifier's — is no authority to publish it.
	checked := map[string]types.VerificationReport{answer.Revision.ID: answerReport}
	for _, p := range planned {
		if p.rev != nil {
			checked[p.rev.Revision.ID] = p.report
		}
	}
	for revID, report := range checked {
		for _, id := range revisionEvidenceOrder(bodies[revID]) {
			if s := srcs[id]; s != nil && isLegalSourceType(s.Span.SourceType) && officialCheckFor(report, id) == nil {
				publicationLog.Warnf("Publication refused (run=%s revision=%s): legal citation without official revalidation", in.RunID, revID)
				return PublicationResult{}, errOfficialNotRevalidated
			}
		}
	}

	var result PublicationResult
	err = in.Fence(ctx, func(tx *u.TenantRepo) error {
		result = PublicationResult{Reused: true}
		var live int64
		if err := tx.Runs().Where("id = ? AND cancel_requested_at IS NULL", in.RunID).Count(&live).Error; err != nil {
			return err
		}
		if live == 0 {
			return fmt.Errorf("%w: run cancelled", ErrPublicationRefused)
		}
		for _, p := range planned {
			id, reused, err := publishArtifactRow(tx, run, p, in.ActorID)
			if err != nil {
				return err
			}
			result.ArtifactIDs = append(result.ArtifactIDs, id)
			result.Reused = result.Reused && reused
		}
		var msgID *string
		if in.AssistantMessageID != "" {
			msgID = &in.AssistantMessageID
		}
		ids, reused, err := bindCitations(tx, run, answer.Revision, bodies[answer.Revision.ID], msgID, answerReport, srcs)
		if err != nil {
			return err
		}
		result.CitationIDs = append(result.CitationIDs, ids...)
		result.Reused = result.Reused && reused
		for _, p := range planned {
			if p.rev == nil {
				continue
			}
			ids, reused, err := bindCitations(tx, run, p.rev.Revision, bodies[p.rev.Revision.ID], nil, p.report, srcs)
			if err != nil {
				return err
			}
			result.CitationIDs = append(result.CitationIDs, ids...)
			result.Reused = result.Reused && reused
		}
		// Freeze the answer: the message now names the exact revision and
		// report it is delivered from (PublishedAnswerText).
		if msgID != nil {
			upd := tx.Messages().Where("id = ? AND run_id = ?", *msgID, in.RunID).Updates(map[string]any{
				"answer_revision_id": answer.Revision.ID,
				"verification_id":    answer.Verification.ID,
				"agent_outcome":      in.Outcome,
			})
			if upd.Error != nil {
				return upd.Error
			}
			if upd.RowsAffected == 0 {
				return fmt.Errorf("%w: assistant message is not part of this run", ErrPublicationRefused)
			}
		}
		return tx.AgentTurns().Where("run_id = ?", in.RunID).Update("final_verification_id", answer.Verification.ID).Error
	})
	if err != nil {
		publicationLog.Errorf("Publication failed (run=%s artifacts=%d): %v", in.RunID, len(planned), err)
		return PublicationResult{}, err
	}
	publicationLog.Infof("Result published (run=%s outcome=%s artifacts=%d citations=%d reused=%t)",
		in.RunID, in.Outcome, len(result.ArtifactIDs), len(result.CitationIDs), result.Reused)
	return result, nil
}

func revisionsOf(planned []plannedArtifact) []VerifiedRevision {
	var out []VerifiedRevision
	for _, p := range planned {
		if p.rev != nil {
			out = append(out, *p.rev)
		}
	}
	return out
}

func coverageIncomplete(r types.VerificationReport) bool {
	return r.Coverage != nil && r.Coverage.Exhaustive && !r.Coverage.Complete
}

// finalVerified loads a revision of the run and the FINAL verification that
// backs its current content — the given one when verificationID is set, else
// the latest — and rechecks the pair.
func finalVerified(repo *u.TenantRepo, runID, revisionID, verificationID string) (VerifiedRevision, types.VerificationReport, error) {
	var rev types.AgentDocumentRevision
	if err := repo.AgentDocumentRevisions().Where("id = ? AND run_id = ?", revisionID, runID).Take(&rev).Error; err != nil {
		return VerifiedRevision{}, types.VerificationReport{}, fmt.Errorf("revision not found")
	}
	q := repo.AgentVerifications().Where("run_id = ? AND document_revision_id = ? AND content_sha256 = ? AND verifier_version = ?",
		runID, rev.ID, rev.ContentSHA256, VerifierVersionFinal)
	if verificationID != "" {
		q = q.Where("id = ?", verificationID)
	}
	var ver types.AgentVerification
	if err := q.Order("created_at DESC").Take(&ver).Error; err != nil {
		return VerifiedRevision{}, types.VerificationReport{}, fmt.Errorf("no final verification for this content")
	}
	vr := VerifiedRevision{Revision: rev, Verification: ver}
	if err := checkVerifiedPair(vr); err != nil {
		return VerifiedRevision{}, types.VerificationReport{}, err
	}
	var report types.VerificationReport
	if err := json.Unmarshal(ver.Report, &report); err != nil {
		return VerifiedRevision{}, types.VerificationReport{}, fmt.Errorf("unreadable verification report")
	}
	if report.ContentSHA256 != rev.ContentSHA256 || report.DocumentRevisionID != rev.ID || report.Result == types.VerificationBlocked {
		return VerifiedRevision{}, types.VerificationReport{}, fmt.Errorf("verification report does not match its row")
	}
	return vr, report, nil
}

// publishArtifactRow inserts the asset, the artifact and artifact.ready — or
// returns the row an earlier attempt already published under the same key.
func publishArtifactRow(tx *u.TenantRepo, run types.Run, p plannedArtifact, actorID string) (string, bool, error) {
	st := p.staged
	key := st.PublicationKey
	var existing types.Artifact
	if err := tx.Artifacts().Where("publication_key = ?", key).Take(&existing).Error; err == nil {
		return existing.ID, true, nil
	}
	mime, assetKind := docxMime, types.AssetKindArtifact
	if st.Kind == types.ArtifactKindExportZip {
		mime, assetKind = "application/zip", types.AssetKindExport
	}
	asset := types.Asset{
		ID: uuid.NewString(), S3Key: st.StagingKey, Kind: assetKind,
		Mime: mime, Size: st.Size, SHA256: st.SHA256, CreatedBy: actorID,
	}
	art := types.Artifact{
		ID: uuid.NewString(), RunID: run.ID, DossierID: run.DossierID,
		Kind: st.Kind, AssetID: asset.ID, Name: st.Name,
		TemplateVersion: st.TemplateVersion, RendererVersion: st.RendererVersion,
		DocumentRevisionID: st.DocumentRevisionID,
		PublicationKey:     &key,
		Partial:            st.Partial,
		PieceWarnings:      st.PieceWarnings,
	}
	if p.rev != nil {
		verID := p.rev.Verification.ID
		art.VerificationID = &verID
	}
	if err := tx.Create(&asset); err != nil {
		return "", false, err
	}
	if err := tx.Create(&art); err != nil {
		return "", false, err
	}
	payload, err := json.Marshal(map[string]any{
		"artifact_id":    art.ID,
		"kind":           art.Kind,
		"name":           art.Name,
		"approved":       false,
		"partial":        art.Partial,
		"piece_warnings": art.PieceWarnings,
	})
	if err != nil {
		return "", false, err
	}
	runID := run.ID
	if err := tx.Create(&types.RunEvent{
		RunID: &runID, ConversationID: run.ConversationID,
		Type: types.EventArtifactReady, Payload: types.JSONB(payload),
	}); err != nil {
		return "", false, err
	}
	return art.ID, false, nil
}

// bindCitations freezes one scope's citations — the answer message, or one
// document revision — numbered in first-use order, each bound to its
// evidence span with its per-check results. A scope already bound is reused.
func bindCitations(tx *u.TenantRepo, run types.Run, rev types.AgentDocumentRevision, body types.DocumentRevisionBody,
	messageID *string, report types.VerificationReport, srcs map[string]*spanSource,
) ([]string, bool, error) {
	q := tx.Citations().Where("run_id = ? AND document_revision_id = ? AND evidence_span_id IS NOT NULL", run.ID, rev.ID)
	if messageID != nil {
		q = q.Where("message_id = ?", *messageID)
	} else {
		q = q.Where("message_id IS NULL")
	}
	var existing []string
	if err := q.Order("number ASC").Pluck("id", &existing).Error; err != nil {
		return nil, false, err
	}
	if len(existing) > 0 {
		return existing, true, nil
	}

	order := revisionEvidenceOrder(body)
	if len(order) == 0 {
		return nil, true, nil
	}
	firstClaim := map[string]string{}
	for _, b := range body.Blocks {
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				if _, ok := firstClaim[id]; !ok {
					firstClaim[id] = c.ID
				}
			}
		}
	}
	now := time.Now().Unix()
	revID := rev.ID
	rows := make([]types.Citation, 0, len(order))
	for i, id := range order {
		s := srcs[id]
		if s == nil {
			return nil, false, fmt.Errorf("%w: evidence span %s vanished before publication", ErrPublicationRefused, id)
		}
		checks, clean := citationChecks(s.Span, report, now)
		rawChecks, err := json.Marshal(checks)
		if err != nil {
			return nil, false, err
		}
		state := types.CitationRetrieved
		if clean {
			state = types.CitationMachineChecked
		}
		loc := s.locator()
		spanID := id
		claimKey := []rune(firstNonEmpty(firstClaim[id], "claim"))
		row := types.Citation{
			ID:                 uuid.NewString(),
			RunID:              run.ID,
			ClaimKey:           string(claimKey[:min(len(claimKey), 128)]),
			SourceType:         s.Span.SourceType,
			LegalVersionID:     s.Span.LegalVersionID,
			PieceID:            s.Span.PieceID,
			WebSourceID:        s.Span.WebSourceID,
			Locator:            s.label(),
			PageFrom:           loc.Page,
			PageTo:             loc.Page,
			Quote:              s.Span.Quote,
			SourceSHA256:       s.Span.SourceSHA256,
			AsOfDate:           s.Span.AsOfDate,
			RetrievedAt:        s.Span.CreatedAt,
			VerificationState:  state,
			MessageID:          messageID,
			DocumentRevisionID: &revID,
			EvidenceSpanID:     &spanID,
			Number:             i + 1,
			Checks:             types.JSONB(rawChecks),
		}
		rows = append(rows, row)
	}
	if err := tx.Create(&rows); err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, false, nil
}

// citationChecks are one citation's separate results. Publication only
// happens without blocking issues, so the source resolved; the quote and
// temporal results come from the report; the official result is passed only
// where the final report records an official round-trip for this span (a
// stored-only match never is); semantic support is not performed here and
// says so. clean is true when no issue at all touches the span — only then
// is the citation machine-checked.
func citationChecks(span types.AgentEvidenceSpan, report types.VerificationReport, at int64) ([]types.CitationCheck, bool) {
	codes := map[string]bool{}
	for _, iss := range report.Issues {
		if containsString(iss.EvidenceIDs, span.ID) {
			codes[iss.Code] = true
		}
	}
	status := func(ok bool) string {
		if ok {
			return types.CitationCheckPassed
		}
		return types.CitationCheckFailed
	}
	checkedAt := &at
	checks := []types.CitationCheck{
		{Check: types.CitationCheckSourceResolved, Status: types.CitationCheckPassed, CheckedAt: checkedAt},
		{Check: types.CitationCheckQuoteMatched, Status: status(!codes[types.IssuePieceQuoteNotFound] && !codes[types.IssueWebExcerptMissing]), CheckedAt: checkedAt},
	}
	switch span.SourceType {
	case types.CitationSourceLegifrance, types.CitationSourceJudilibre:
		checks = append(checks, types.CitationCheck{Check: types.CitationCheckTemporalValid, Status: types.CitationCheckPassed, CheckedAt: checkedAt})
	default:
		checks = append(checks, types.CitationCheck{Check: types.CitationCheckTemporalValid, Status: types.CitationCheckNotChecked})
	}
	official := types.CitationCheck{Check: types.CitationCheckOfficialMatched, Status: types.CitationCheckNotChecked}
	if oc := officialCheckFor(report, span.ID); oc != nil {
		at := oc.CheckedAt
		official = types.CitationCheck{Check: types.CitationCheckOfficialMatched, Status: types.CitationCheckPassed, CheckedAt: &at}
	}
	checks = append(checks, official,
		types.CitationCheck{Check: types.CitationCheckSemanticSupport, Status: types.CitationCheckNotChecked},
	)
	return checks, len(codes) == 0
}

// ---------------------------------------------------------------------------
// The frozen answer
// ---------------------------------------------------------------------------

// PublishedAnswerText is the text of a published answer, rebuilt only from
// immutable rows — the revision, its final report and the citation bindings
// frozen at publication — so a delivery resumed after a crash streams the
// same characters. Citation markers [n] are the bound numbers; pièce
// warnings sit under the block they concern.
func PublishedAnswerText(ctx context.Context, repo *u.TenantRepo, messageID string) (string, error) {
	var msg types.Message
	if err := repo.Messages().Where("id = ?", messageID).Take(&msg).Error; err != nil {
		return "", notFoundOr(err)
	}
	if msg.AnswerRevisionID == nil || msg.VerificationID == nil || msg.RunID == nil {
		return "", fmt.Errorf("%w: message has no published answer", ErrNotVerified)
	}
	vr, err := LoadVerifiedRevision(ctx, repo.TenantID(), *msg.RunID, *msg.VerificationID)
	if err != nil {
		return "", err
	}
	if vr.Revision.ID != *msg.AnswerRevisionID || vr.Verification.VerifierVersion != VerifierVersionFinal {
		return "", fmt.Errorf("%w: frozen answer does not match its verification", ErrNotVerified)
	}
	var body types.DocumentRevisionBody
	if err := json.Unmarshal(vr.Revision.Body, &body); err != nil {
		return "", err
	}
	var report types.VerificationReport
	if err := json.Unmarshal(vr.Verification.Report, &report); err != nil {
		return "", err
	}
	var cits []types.Citation
	if err := repo.Citations().Where("message_id = ? AND document_revision_id = ?", msg.ID, vr.Revision.ID).
		Find(&cits).Error; err != nil {
		return "", err
	}
	return composeAnswerText(body, report, cits), nil
}

func composeAnswerText(body types.DocumentRevisionBody, report types.VerificationReport, cits []types.Citation) string {
	numbers := map[string]int{}
	for _, c := range cits {
		if c.EvidenceSpanID != nil {
			numbers[*c.EvidenceSpanID] = c.Number
		}
	}
	warnings := map[string][]string{}
	var notices []string
	seen := map[string]bool{}
	for _, iss := range report.Issues {
		if iss.Severity != types.SeverityWarning || iss.Correction == "" || seen[iss.BlockID+"|"+iss.Correction] {
			continue
		}
		seen[iss.BlockID+"|"+iss.Correction] = true
		if iss.BlockID == "" {
			notices = append(notices, iss.Correction)
		} else {
			warnings[iss.BlockID] = append(warnings[iss.BlockID], iss.Correction)
		}
	}
	var parts []string
	for _, n := range notices {
		parts = append(parts, "> "+n)
	}
	for _, b := range body.Blocks {
		var marks strings.Builder
		done := map[int]bool{}
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				if n := numbers[id]; n > 0 && !done[n] {
					done[n] = true
					fmt.Fprintf(&marks, " [%d]", n)
				}
			}
		}
		text := b.Text + marks.String()
		switch b.Role {
		case types.BlockHeading:
			text = "## " + text
			if b.Level > 1 {
				text = "#" + text
			}
		case types.BlockListItem:
			text = strings.Repeat("  ", max(b.Level-1, 0)) + "- " + text
		case types.BlockChronologyRow:
			date := "date non établie"
			if b.Chronology != nil {
				date = FrenchPartialDate(b.Chronology.Date.PartialDate) + dateRoleSuffix(b.Chronology.Date.Role)
			}
			text = "- **" + date + "** — " + text
		case types.BlockBordereauRow:
			if b.Bordereau != nil {
				text = fmt.Sprintf("- Pièce n° %d — %s", b.Bordereau.Ordinal, text)
			}
		case types.BlockLimitation:
			text = "> Limite : " + text
		case types.BlockWarning:
			text = "> " + text
		}
		parts = append(parts, text)
		for _, w := range warnings[b.ID] {
			parts = append(parts, "> "+w)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ---------------------------------------------------------------------------
// Download policy
// ---------------------------------------------------------------------------

// agentArtifactDownloadPolicy returns a refusal (status, body) for an agent
// artifact that may not be downloaded, or status 0. An agent artifact is
// served only through its run's authorization and while a final verification
// still matches its revision's content; pièce warnings allow download.
func agentArtifactDownloadPolicy(c *gin.Context, repo *u.TenantRepo, art types.Artifact) (int, any) {
	notFound := gin.H{"detail": "Artifact not found"}
	refused := gin.H{"detail": "Artifact verification missing or invalid"}
	var run types.Run
	if err := repo.Runs().Where("id = ?", art.RunID).Take(&run).Error; err != nil || !runsMayAccess(c, repo, run, types.ActionRead) {
		return http.StatusNotFound, notFound
	}
	if art.DocumentRevisionID == nil {
		if serverBuiltKinds[art.Kind] {
			return 0, nil
		}
		publicationLog.Warnf("Agent artifact download refused, no revision (artifact=%s kind=%s)", art.ID, art.Kind)
		return http.StatusConflict, refused
	}
	if art.VerificationID == nil {
		publicationLog.Warnf("Agent artifact download refused, no verification (artifact=%s)", art.ID)
		return http.StatusConflict, refused
	}
	var ver types.AgentVerification
	if err := repo.AgentVerifications().Where("id = ? AND run_id = ?", *art.VerificationID, art.RunID).Take(&ver).Error; err != nil {
		publicationLog.Warnf("Agent artifact download refused, verification missing (artifact=%s)", art.ID)
		return http.StatusConflict, refused
	}
	var rev types.AgentDocumentRevision
	if err := repo.AgentDocumentRevisions().Where("id = ? AND run_id = ?", *art.DocumentRevisionID, art.RunID).Take(&rev).Error; err != nil {
		publicationLog.Warnf("Agent artifact download refused, revision missing (artifact=%s)", art.ID)
		return http.StatusConflict, refused
	}
	if ver.VerifierVersion != VerifierVersionFinal || checkVerifiedPair(VerifiedRevision{Revision: rev, Verification: ver}) != nil {
		publicationLog.Warnf("Agent artifact download refused, verification does not match (artifact=%s)", art.ID)
		return http.StatusConflict, refused
	}
	return 0, nil
}
