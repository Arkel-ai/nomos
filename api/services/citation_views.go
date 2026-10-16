package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var citationViewsLog = u.NewLogger("services.citation_views")

// Citation views. A published citation is shown
// by source type (pièce, Légifrance, Judilibre, web), hydrated from the frozen
// evidence span and the stored source. Missing metadata stays missing; a web
// source never carries a legal-authority field.
//
// Every route authorizes through the run (tenant, then owner or dossier
// grant) BEFORE any source lookup, and a foreign or unknown id is a 404 that
// says nothing about existence.

const (
	citationPageDefault = 50
	citationPageMax     = 100
	// citationPassageContext is how much frozen source text the detail shows
	// on each side of the quote.
	citationPassageContext = 400
)

// CitationListQuery is GET /runs/:id/citations' query string.
type CitationListQuery struct {
	DocumentRevisionID string `form:"document_revision_id"`
	MessageID          string `form:"message_id"`
	After              *int   `form:"after"`
	Limit              int    `form:"limit"`
}

// ---------------------------------------------------------------------------
// Sources behind evidence spans
// ---------------------------------------------------------------------------

// spanSource is one evidence span with the stored source behind it; a nil
// source is one that no longer exists.
type spanSource struct {
	Span  types.AgentEvidenceSpan
	Piece *types.Piece
	Legal *AgentLegalSource
	Web   *types.AgentWebSource
}

// loadSpanSources batch-loads spans (of one run when runID is set) and their
// sources: pièces through the tenant repository, legal versions from the
// public corpus, web sources of the same tenant.
func loadSpanSources(ctx context.Context, repo *u.TenantRepo, runID string, ids []string) (map[string]*spanSource, error) {
	out := map[string]*spanSource{}
	if len(ids) == 0 {
		return out, nil
	}
	q := repo.AgentEvidenceSpans().Where("id IN ?", ids)
	if runID != "" {
		q = q.Where("run_id = ?", runID)
	}
	var spans []types.AgentEvidenceSpan
	if err := q.Find(&spans).Error; err != nil {
		return nil, err
	}
	var pieceIDs, versionIDs, webIDs []string
	for _, s := range spans {
		out[s.ID] = &spanSource{Span: s}
		switch {
		case s.PieceID != nil:
			pieceIDs = append(pieceIDs, *s.PieceID)
		case s.LegalVersionID != nil:
			versionIDs = append(versionIDs, *s.LegalVersionID)
		case s.WebSourceID != nil:
			webIDs = append(webIDs, *s.WebSourceID)
		}
	}
	pieces := map[string]*types.Piece{}
	if len(pieceIDs) > 0 {
		var rows []types.Piece
		if err := repo.Pieces().Where("id IN ?", pieceIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			pieces[rows[i].ID] = &rows[i]
		}
	}
	legal, err := loadLegalSources(ctx, versionIDs)
	if err != nil {
		return nil, err
	}
	web := map[string]*types.AgentWebSource{}
	if len(webIDs) > 0 {
		var rows []types.AgentWebSource
		if err := repo.AgentWebSources().Where("id IN ?", webIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			web[rows[i].ID] = &rows[i]
		}
	}
	for _, s := range out {
		switch {
		case s.Span.PieceID != nil:
			s.Piece = pieces[*s.Span.PieceID]
		case s.Span.LegalVersionID != nil:
			s.Legal = legal[*s.Span.LegalVersionID]
		case s.Span.WebSourceID != nil:
			s.Web = web[*s.Span.WebSourceID]
		}
	}
	return out, nil
}

func (s *spanSource) locator() types.SourceLocator {
	var loc types.SourceLocator
	_ = json.Unmarshal(s.Span.Locator, &loc)
	return loc
}

// label is how the source is named in a document's sources and in the
// citation list: "Pièce n° 3 — Contrat de travail, p. 2", "C. trav., art.
// L. 1234-1 (version en vigueur du … au …)", "Titre — host (source web,
// consultée le …)".
func (s *spanSource) label() string {
	loc := s.locator()
	switch s.Span.SourceType {
	case types.CitationSourcePiece:
		if s.Piece == nil {
			return "Pièce retirée du dossier"
		}
		name := pieceTitle(*s.Piece)
		head := "Pièce"
		if s.Piece.Ordinal > 0 {
			head = fmt.Sprintf("Pièce n° %d", s.Piece.Ordinal)
		}
		return head + " — " + name + pieceLocatorDetail(loc)
	case types.CitationSourceLegifrance:
		if s.Legal == nil {
			return "Texte introuvable"
		}
		return firstNonEmpty(s.Legal.Document.CitationLabel, s.Legal.Version.SourceVersion) + versionValidity(s.Legal.Version)
	case types.CitationSourceJudilibre:
		if s.Legal == nil {
			return "Décision introuvable"
		}
		return firstNonEmpty(s.Legal.Document.CitationLabel, s.Legal.Version.ECLI, s.Legal.Document.SourceID)
	case types.CitationSourceWeb:
		if s.Web == nil {
			return "Source web introuvable"
		}
		host := firstNonEmpty(s.Web.Host, hostOf(s.Web.URL))
		return fmt.Sprintf("%s — %s (source web, consultée le %s)", firstNonEmpty(s.Web.Title, host), host,
			FrenchDate(time.Unix(s.Web.RetrievedAt, 0).UTC()))
	}
	return "Source"
}

// pieceLocatorDetail is ", p. 2, § 3" / ", § 4" / ", 'Feuille'!B4": never a
// page for a DOCX or a workbook, nothing when the locator names no place.
func pieceLocatorDetail(loc types.SourceLocator) string {
	if at := pieceLocatorText(loc); at != "" {
		return ", " + at
	}
	return ""
}

// versionValidity renders a version's half-open interval [from, to) with its
// last day of validity.
func versionValidity(v types.LegalVersion) string {
	if v.ValidFrom == nil {
		return " (période de validité inconnue)"
	}
	from := FrenchDate(time.Unix(*v.ValidFrom, 0).UTC())
	if v.ValidTo == nil {
		return " (version en vigueur depuis le " + from + ")"
	}
	last := time.Unix(*v.ValidTo, 0).UTC().AddDate(0, 0, -1)
	return " (version en vigueur du " + from + " au " + FrenchDate(last) + ")"
}

func hostOf(raw string) string {
	if p, err := url.Parse(raw); err == nil {
		return p.Hostname()
	}
	return ""
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// citationScope is what a run's citation rows need from their revisions:
// which claims cite which span, and the warnings the final report left on it.
type citationScope struct {
	claims   map[string][]string // span id → claim ids
	warnings map[string][]string // span id → warning codes
}

func loadCitationScopes(repo *u.TenantRepo, revisionIDs []string) (map[string]citationScope, error) {
	out := map[string]citationScope{}
	if len(revisionIDs) == 0 {
		return out, nil
	}
	var revs []types.AgentDocumentRevision
	if err := repo.AgentDocumentRevisions().Where("id IN ?", revisionIDs).Find(&revs).Error; err != nil {
		return nil, err
	}
	for _, rev := range revs {
		sc := citationScope{claims: map[string][]string{}, warnings: map[string][]string{}}
		var body types.DocumentRevisionBody
		if json.Unmarshal(rev.Body, &body) == nil {
			for _, b := range body.Blocks {
				for _, c := range b.Claims {
					for _, id := range claimEvidenceIDs(c) {
						sc.claims[id] = append(sc.claims[id], c.ID)
					}
				}
			}
		}
		out[rev.ID] = sc
	}
	var vers []types.AgentVerification
	if err := repo.AgentVerifications().Where("document_revision_id IN ? AND verifier_version = ?", revisionIDs, VerifierVersionFinal).
		Order("created_at DESC").Find(&vers).Error; err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, ver := range vers {
		if done[ver.DocumentRevisionID] {
			continue
		}
		done[ver.DocumentRevisionID] = true
		var report types.VerificationReport
		if json.Unmarshal(ver.Report, &report) != nil {
			continue
		}
		sc := out[ver.DocumentRevisionID]
		if sc.warnings == nil {
			continue
		}
		for _, iss := range report.Issues {
			if iss.Severity != types.SeverityWarning {
				continue
			}
			for _, id := range iss.EvidenceIDs {
				if !containsString(sc.warnings[id], iss.Code) {
					sc.warnings[id] = append(sc.warnings[id], iss.Code)
				}
			}
		}
	}
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// buildAgentCitations turns citation rows into their wire shape. Historical
// rows (source_type "legal", no evidence span) are decoded too: their origin
// is read from the stored legal document, never guessed.
func buildAgentCitations(ctx context.Context, repo *u.TenantRepo, rows []types.Citation) ([]types.AgentCitation, error) {
	var spanIDs, revIDs, legacyVersions, legacyPieces []string
	for _, r := range rows {
		if r.EvidenceSpanID != nil {
			spanIDs = append(spanIDs, *r.EvidenceSpanID)
		} else if r.LegalVersionID != nil {
			legacyVersions = append(legacyVersions, *r.LegalVersionID)
		} else if r.PieceID != nil {
			legacyPieces = append(legacyPieces, *r.PieceID)
		}
		if r.DocumentRevisionID != nil && !containsString(revIDs, *r.DocumentRevisionID) {
			revIDs = append(revIDs, *r.DocumentRevisionID)
		}
	}
	srcs, err := loadSpanSources(ctx, repo, "", spanIDs)
	if err != nil {
		return nil, err
	}
	scopes, err := loadCitationScopes(repo, revIDs)
	if err != nil {
		return nil, err
	}
	legacyLegal, err := loadLegalSources(ctx, legacyVersions)
	if err != nil {
		return nil, err
	}
	legacyPieceRows := map[string]*types.Piece{}
	if len(legacyPieces) > 0 {
		var ps []types.Piece
		if err := repo.Pieces().Where("id IN ?", legacyPieces).Find(&ps).Error; err != nil {
			return nil, err
		}
		for i := range ps {
			legacyPieceRows[ps[i].ID] = &ps[i]
		}
	}

	out := make([]types.AgentCitation, 0, len(rows))
	for _, r := range rows {
		var src *spanSource
		if r.EvidenceSpanID != nil {
			src = srcs[*r.EvidenceSpanID]
		}
		if src == nil {
			// A historical row: rebuild what the stored source says, nothing more.
			src = &spanSource{Span: types.AgentEvidenceSpan{SourceType: r.SourceType, Quote: r.Quote, SourceSHA256: r.SourceSHA256}}
			switch {
			case r.LegalVersionID != nil:
				src.Legal = legacyLegal[*r.LegalVersionID]
				src.Span.LegalVersionID = r.LegalVersionID
				src.Span.SourceType = types.CitationSourceLegifrance
				if src.Legal != nil && (src.Legal.Document.Kind == legalKindDecision || src.Legal.Document.Source == types.SourceJudilibre) {
					src.Span.SourceType = types.CitationSourceJudilibre
				}
				kind := types.LocatorLegislation
				if src.Span.SourceType == types.CitationSourceJudilibre {
					kind = types.LocatorDecision
				}
				raw, _ := json.Marshal(types.SourceLocator{Kind: kind, VersionID: *r.LegalVersionID})
				src.Span.Locator = raw
			case r.PieceID != nil:
				src.Piece = legacyPieceRows[*r.PieceID]
				src.Span.PieceID = r.PieceID
				if r.PageFrom > 0 {
					raw, _ := json.Marshal(types.SourceLocator{Kind: types.LocatorPDF, Page: r.PageFrom})
					src.Span.Locator = raw
				}
			}
		}
		out = append(out, agentCitationDTO(r, src, scopes))
	}
	return out, nil
}

func agentCitationDTO(r types.Citation, src *spanSource, scopes map[string]citationScope) types.AgentCitation {
	dto := types.AgentCitation{
		ID:                 r.ID,
		Number:             r.Number,
		SourceType:         src.Span.SourceType,
		ClaimIDs:           []string{},
		MessageID:          r.MessageID,
		DocumentRevisionID: r.DocumentRevisionID,
		Label:              firstNonEmpty(r.Locator, src.label()),
		Quote:              r.Quote,
		Locator:            src.locator(),
		SourceSHA256:       r.SourceSHA256,
		RetrievedAt:        r.RetrievedAt,
		Checks:             []types.CitationCheck{},
		WarningCodes:       []string{},
		VerifiedBy:         r.VerifiedBy,
		VerifiedAt:         r.VerifiedAt,
	}
	if r.EvidenceSpanID != nil {
		dto.EvidenceSpanID = *r.EvidenceSpanID
	}
	if len(r.Checks) > 0 {
		_ = json.Unmarshal(r.Checks, &dto.Checks)
	}
	for _, c := range dto.Checks {
		if c.CheckedAt != nil && (dto.CheckedAt == nil || *c.CheckedAt > *dto.CheckedAt) {
			at := *c.CheckedAt
			dto.CheckedAt = &at
		}
	}
	if r.DocumentRevisionID != nil {
		if sc, ok := scopes[*r.DocumentRevisionID]; ok {
			if ids := sc.claims[dto.EvidenceSpanID]; ids != nil {
				dto.ClaimIDs = ids
			}
			if codes := sc.warnings[dto.EvidenceSpanID]; codes != nil {
				dto.WarningCodes = codes
			}
		}
	}
	loc := dto.Locator
	switch dto.SourceType {
	case types.CitationSourcePiece:
		if src.Span.PieceID != nil {
			dto.PieceID = *src.Span.PieceID
		}
		if src.Piece != nil {
			dto.PieceOrdinal = src.Piece.Ordinal
			dto.Filename = src.Piece.OriginalName
		}
	case types.CitationSourceLegifrance:
		dto.ArticleID = loc.ArticleID
		if src.Legal != nil {
			dto.ArticleID = firstNonEmpty(dto.ArticleID, src.Legal.Document.SourceID)
			dto.ArticleLabel = src.Legal.Document.CitationLabel
			dto.StoredVersionID = src.Legal.Version.ID
			dto.OfficialVersionID = src.Legal.Version.SourceVersion
			dto.ValidFrom, dto.ValidTo = src.Legal.Version.ValidFrom, src.Legal.Version.ValidTo
			dto.URL = src.Legal.Document.CanonicalURL
		}
		dto.ApplicabilityDate = r.AsOfDate
	case types.CitationSourceJudilibre:
		if src.Legal != nil {
			dto.DecisionID = src.Legal.Document.SourceID
			dto.DecisionDate = src.Legal.Version.DecisionDate
			dto.ECLI = src.Legal.Version.ECLI
			dto.URL = src.Legal.Document.CanonicalURL
			var meta map[string]any
			if len(src.Legal.Version.SourceMeta) > 0 && json.Unmarshal(src.Legal.Version.SourceMeta, &meta) == nil {
				dto.Court, _ = meta["jurisdiction"].(string)
				dto.Chamber, _ = meta["chamber"].(string)
				if nums, ok := meta["numbers"].([]any); ok && len(nums) > 0 {
					dto.Pourvoi, _ = nums[0].(string)
				}
			}
		}
	case types.CitationSourceWeb:
		if src.Web != nil {
			dto.URL = src.Web.URL
			dto.Host = firstNonEmpty(src.Web.Host, hostOf(src.Web.URL))
			dto.Title = src.Web.Title
			dto.Extent = src.Web.Extent
		} else {
			dto.URL = loc.URL
		}
	}
	return dto
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

// ListRunCitations is GET /runs/:id/citations: the run's published
// citations in first-use order, optionally limited to one answer message or
// one document revision. 50 per page, 100 at most; `after` is the last
// number seen.
func ListRunCitations(c *gin.Context, runID string, q CitationListQuery) (int, any) {
	repo, run, _, status, body := runsLoad(c, runID, types.ActionRead)
	if run == nil {
		return status, body
	}
	limit := q.Limit
	if limit <= 0 {
		limit = citationPageDefault
	}
	limit = min(limit, citationPageMax)

	scoped := func() *gorm.DB {
		db := repo.Citations().Where("run_id = ? AND evidence_span_id IS NOT NULL", run.ID)
		if q.DocumentRevisionID != "" {
			db = db.Where("document_revision_id = ?", q.DocumentRevisionID)
		}
		if q.MessageID != "" {
			db = db.Where("message_id = ?", q.MessageID)
		}
		return db
	}
	var total int64
	if err := scoped().Count(&total).Error; err != nil {
		citationViewsLog.Errorf("Citation count failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Citation lookup failed"}
	}
	page := scoped()
	if q.After != nil {
		page = page.Where("number > ?", *q.After)
	}
	var rows []types.Citation
	if err := page.Order("number ASC, id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		citationViewsLog.Errorf("Citation listing failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Citation lookup failed"}
	}
	var next *int
	if len(rows) > limit {
		// Numbers restart per scope, so an unscoped listing can carry the
		// same number several times: a page ends on a whole number group.
		cut := limit
		for cut > 0 && rows[cut-1].Number == rows[limit].Number {
			cut--
		}
		if cut == 0 {
			cut = limit // one group wider than a page — only with more than 100 scopes
		}
		rows = rows[:cut]
		n := rows[cut-1].Number
		next = &n
	}
	items, err := buildAgentCitations(c.Request.Context(), repo, rows)
	if err != nil {
		citationViewsLog.Errorf("Citation hydration failed (run=%s rows=%d): %v", run.ID, len(rows), err)
		return http.StatusInternalServerError, gin.H{"detail": "Citation lookup failed"}
	}
	citationViewsLog.Debugf("Citations listed (run=%s items=%d total=%d scoped=%t)", run.ID, len(items), total,
		q.DocumentRevisionID != "" || q.MessageID != "")
	return http.StatusOK, types.AgentCitationList{Items: items, Total: total, NextAfter: next}
}

// GetCitation is GET /citations/:id: one citation plus a bounded passage of
// the frozen source with the quote's position in it.
func GetCitation(c *gin.Context, id string) (int, any) {
	repo, status, body := tenantRepo(c)
	if repo == nil {
		return status, body
	}
	notFound := gin.H{"detail": "Citation not found"}
	var row types.Citation
	if err := repo.Citations().Where("id = ?", id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return http.StatusNotFound, notFound
		}
		citationViewsLog.Errorf("Citation lookup failed (citation=%s): %v", id, err)
		return http.StatusInternalServerError, gin.H{"detail": "Citation lookup failed"}
	}
	var run types.Run
	if err := repo.Runs().Where("id = ?", row.RunID).Take(&run).Error; err != nil || !runsMayAccess(c, repo, run, types.ActionRead) {
		return http.StatusNotFound, notFound
	}
	items, err := buildAgentCitations(c.Request.Context(), repo, []types.Citation{row})
	if err != nil || len(items) != 1 {
		citationViewsLog.Errorf("Citation hydration failed (citation=%s): %v", id, err)
		return http.StatusInternalServerError, gin.H{"detail": "Citation lookup failed"}
	}
	detail := types.AgentCitationDetail{AgentCitation: items[0]}
	if passage, start, end, ok := piecePassageAround(c.Request.Context(), repo, row, items[0].Locator); ok {
		detail.SourceAvailable, detail.Passage = true, passage
		detail.HighlightStart, detail.HighlightEnd = start, end
		citationViewsLog.Debugf("Citation detail served (citation=%s source_type=%s located=true)", id, detail.SourceType)
		return http.StatusOK, detail
	}
	source, available := citationSourceText(c.Request.Context(), repo, row)
	detail.SourceAvailable = available
	if available {
		start, end, ok := LocateQuote(source, row.Quote)
		if !ok {
			start, end = 0, 0
		}
		runes := []rune(source)
		from := max(start-citationPassageContext, 0)
		to := min(end+citationPassageContext, len(runes))
		if !ok {
			to = min(2*citationPassageContext, len(runes))
		}
		detail.Passage = string(runes[from:to])
		if ok {
			detail.HighlightStart, detail.HighlightEnd = start-from, end-from
		}
	}
	citationViewsLog.Debugf("Citation detail served (citation=%s source_type=%s available=%t)", id, detail.SourceType, available)
	return http.StatusOK, detail
}

// piecePassageAround shows a pièce citation in place: the canonical text
// around its locator — the rest of the page, the neighbouring paragraphs or
// cells — with the cited range highlighted at its exact offsets. Only while
// the current extraction still holds the cited text at that locator (same
// original, same text); otherwise ok is false and the frozen quote is shown
// alone.
func piecePassageAround(ctx context.Context, repo *u.TenantRepo, row types.Citation, loc types.SourceLocator) (string, int, int, bool) {
	if row.SourceType != types.CitationSourcePiece || row.PieceID == nil || loc.SegmentID == "" {
		return "", 0, 0, false
	}
	var piece types.Piece
	if err := repo.Pieces().Where("id = ?", *row.PieceID).Take(&piece).Error; err != nil {
		return "", 0, 0, false
	}
	text, err := LoadPieceText(ctx, repo, piece, nil)
	if err != nil || text.State != PieceTextReady || text.SourceSHA256 != row.SourceSHA256 {
		return "", 0, 0, false
	}
	seg, ok := text.segment(loc.SegmentID)
	if !ok || loc.Start < 0 || loc.Start >= loc.End || loc.End > seg.Runes || string(seg.runeSlice()[loc.Start:loc.End]) != row.Quote {
		return "", 0, 0, false
	}
	unit, start, end := seg.runeSlice(), loc.Start, loc.End
	for _, pg := range text.units {
		if pg.Number == seg.unit {
			if r := []rune(pg.Text); seg.base+loc.End <= len(r) {
				unit, start, end = r, seg.base+loc.Start, seg.base+loc.End
			}
			break
		}
	}
	from, to := max(start-citationPassageContext, 0), min(end+citationPassageContext, len(unit))
	return string(unit[from:to]), start - from, end - from, true
}

// citationSourceText is the frozen text a citation's quote is located in:
// the stored legal version, the returned web excerpt, the pièce passage the
// agent read — or, for a historical pièce citation, the piece's extracted
// text.
func citationSourceText(ctx context.Context, repo *u.TenantRepo, row types.Citation) (string, bool) {
	if row.EvidenceSpanID != nil {
		srcs, err := loadSpanSources(ctx, repo, row.RunID, []string{*row.EvidenceSpanID})
		if err != nil || srcs[*row.EvidenceSpanID] == nil {
			return "", false
		}
		s := srcs[*row.EvidenceSpanID]
		switch {
		case s.Legal != nil:
			return s.Legal.Version.Text, true
		case s.Web != nil:
			return s.Web.Content, strings.TrimSpace(s.Web.Content) != ""
		case s.Piece != nil:
			return s.Span.Quote, true
		}
		return "", false
	}
	id, ok := citationSourceID(row)
	if !ok {
		return "", false
	}
	rec, err := (&dbSourceResolver{tenantID: repo.TenantID()}).Resolve(ctx, row.SourceType, id)
	if err != nil {
		return "", false
	}
	return rec.Text, true
}

// RunCoverage is GET /runs/:id/coverage: the reading coverage of an agent
// run's frozen scope, from the read-receipt ledger.
func RunCoverage(c *gin.Context, runID string) (int, any) {
	repo, run, _, status, body := runsLoad(c, runID, types.ActionRead)
	if run == nil {
		return status, body
	}
	report, err := ComputeCoverage(c.Request.Context(), repo, run.ID)
	if errors.Is(err, ErrVerificationTarget) {
		return http.StatusNotFound, gin.H{"detail": "Coverage not found"}
	}
	if err != nil {
		citationViewsLog.Errorf("Coverage computation failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Coverage computation failed"}
	}
	citationViewsLog.Debugf("Coverage served (run=%s pieces=%d fully_read=%d complete=%t)",
		run.ID, report.TotalPieces, report.FullyRead, report.Complete)
	return http.StatusOK, report
}
