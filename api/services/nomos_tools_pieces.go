package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var pieceToolsLog = u.NewLogger("services.nomos_tools_pieces")

// Private dossier tools. Every call is
// checked against the turn's frozen scope: a piece outside it, in another
// dossier or in another tenant is the same non-disclosing unknown_reference.
// Only read_piece and read_evidence deliver citable text and write receipts;
// list_pieces and search_pieces never count as reading.

const (
	listPiecesMaxLimit   = 50
	readPieceDefault     = 12000
	readPieceMin         = 1000
	readPieceMax         = 24000
	searchPiecesDefault  = 8
	searchPiecesMax      = 20
	searchPieceExcerpt   = 600
	maxPrivateQueryRunes = 500
	readEvidenceMaxIDs   = 20
	readEvidenceMaxRunes = 40000
)

// privateScope resolves the turn's scope for a private tool. A non-nil
// outcome is the final answer (no dossier, scope too large).
func privateScope(ctx context.Context, x ToolExecution, tool string, act types.AgentActivityObject) (PieceScopeSnapshot, *ToolOutcome, error) {
	s := x.Contract.Scope
	if s.Mode == types.ScopeNone || s.Mode == "" || s.DossierID == "" {
		out := failure(x, tool, types.ToolCodeMissingDossier, false,
			"cette conversation n'est rattachée à aucun dossier : il n'y a pas de pièces à consulter", nil, act)
		return PieceScopeSnapshot{}, &out, nil
	}
	if !x.Permissions.DossierRead {
		return PieceScopeSnapshot{}, nil, errDossierAccessRevoked
	}
	snap, err := SnapshotPieceScope(ctx, x.Repo(ctx), s)
	switch {
	case errors.Is(err, ErrScopeTooLarge):
		out := failure(x, tool, types.ToolCodeProcessingLimit, false,
			fmt.Sprintf("le dossier dépasse %d pièces, la limite de traitement de l'agent", maxScopePieces), nil, act)
		return snap, &out, nil
	case err != nil:
		return snap, nil, err
	}
	return snap, nil, nil
}

func unknownPiece(x ToolExecution, tool string, act types.AgentActivityObject) ToolOutcome {
	return failure(x, tool, types.ToolCodeUnknownReference, false, "pièce inconnue ou hors du périmètre de la tâche", nil, act)
}

func ingestionState(p types.Piece) string {
	switch p.ClassificationState {
	case types.PieceStatePending:
		return "pending"
	case types.PieceStateFailed:
		return "failed"
	}
	return "extracted"
}

// ---------------------------------------------------------------------------
// list_pieces
// ---------------------------------------------------------------------------

type listPiecesArgs struct {
	types.ToolArgs
	Cursor *string `json:"cursor"`
	Limit  *int    `json:"limit"`
}

func listPiecesTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolListPieces, types.ToolClassDB, true, false,
			"Inventaire complet des pièces du périmètre de la tâche, par page de 50 : numéro, nom, nature, date, parties, "+
				"résumé de qualification et état d'ingestion. Ces métadonnées ne valent pas lecture : lisez une pièce avec read_piece "+
				"avant de lui attribuer un fait.",
			map[string]any{
				"cursor": nullable(strSchema("next_cursor de la page précédente. Null : première page.")),
				"limit":  nullable(intSchema("Pièces par page, 1 à 50 (défaut 50).")),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execListPieces,
	}
}

func execListPieces(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectPieces, Label: "Inventaire du dossier"}
	var a listPiecesArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolListPieces, err, act)
	}
	limit := listPiecesMaxLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > listPiecesMaxLimit {
			return invalid(x, types.ToolListPieces, badArgs("limit doit être entre 1 et %d", listPiecesMaxLimit), act)
		}
		limit = *a.Limit
	}
	offset := 0
	if c := str(a.Cursor); c != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(c, "o:"))
		if err != nil || n < 0 {
			return invalid(x, types.ToolListPieces, badArgs("curseur invalide"), act)
		}
		offset = n
	}
	snap, final, err := privateScope(ctx, x, types.ToolListPieces, act)
	if final != nil || err != nil {
		return derefOutcome(final), err
	}
	total := len(snap.Pieces)
	end := min(offset+limit, total)
	page := []types.Piece{}
	if offset < total {
		page = snap.Pieces[offset:end]
	}

	repo := x.Repo(ctx)
	ids := make([]string, 0, len(page))
	for _, p := range page {
		ids = append(ids, p.ID)
	}
	parties := map[string][]map[string]string{}
	if len(ids) > 0 {
		var rows []types.PiecePartie
		if err := repo.PieceParties().Where("piece_id IN ?", ids).Order("piece_id, role").Find(&rows).Error; err != nil {
			return ToolOutcome{}, err
		}
		for _, r := range rows {
			parties[r.PieceID] = append(parties[r.PieceID], map[string]string{"role": r.Role, "name": r.DisplayName})
		}
	}
	consumed, err := consumedSegments(repo, x.RunID, ids)
	if err != nil {
		return ToolOutcome{}, err
	}

	items := make([]map[string]any, 0, len(page))
	for _, p := range page {
		format := pieceFormat(snap.Assets[p.SourceAssetID].Mime)
		item := map[string]any{
			"piece_id":          p.ID,
			"ordinal":           p.Ordinal,
			"name":              pieceName(p),
			"original_name":     p.OriginalName,
			"kind":              p.PieceKind,
			"kind_label":        PieceKindLabel(p.PieceKind),
			"title":             p.Title,
			"document_date":     FormatPieceDate(p.DocumentDate, p.DatePrecision),
			"date_precision":    p.DatePrecision,
			"summary":           truncateRunes(strings.TrimSpace(p.Summary), 400),
			"parties":           parties[p.ID],
			"ingestion_state":   ingestionState(p),
			"format":            format,
			"segments_consumed": consumed[p.ID],
		}
		// Only a PDF has pages: a DOCX has none until laid out, a workbook
		// has sheets.
		if format == pieceFormatPDF {
			item["pages"] = p.PageCount
		}
		if item["parties"] == nil {
			item["parties"] = []map[string]string{}
		}
		items = append(items, item)
	}
	data := map[string]any{
		"dossier_id":     snap.DossierID,
		"scope":          snap.Mode,
		"total":          total,
		"excluded_count": len(snap.Excluded),
		"missing_count":  len(snap.MissingIDs),
		"offset":         offset,
		"pieces":         items,
		"note":           "Métadonnées de qualification : elles ne valent pas lecture et ne peuvent pas fonder un fait attribué à une pièce.",
	}
	next := ""
	if end < total {
		next = "o:" + strconv.Itoa(end)
		data["next_cursor"] = next
	}
	out := success(x, types.ToolListPieces, total == 0, data, act)
	out.Observation.NextCursor = next
	if len(page) > 0 {
		out.Activity.Detail = fmt.Sprintf("pièces %d à %d sur %d", offset+1, end, total)
	}
	out.Counts = counts(intPtr(len(page)), intPtr(total))
	pieceToolsLog.Debugf("Pieces listed (run=%s call=%s page=%d total=%d)", x.RunID, x.CallID, len(page), total)
	return out, nil
}

func derefOutcome(o *ToolOutcome) ToolOutcome {
	if o == nil {
		return ToolOutcome{}
	}
	return *o
}

// ---------------------------------------------------------------------------
// search_pieces
// ---------------------------------------------------------------------------

type searchPiecesArgs struct {
	types.ToolArgs
	Query    string   `json:"query"`
	PieceIDs []string `json:"piece_ids"`
	Limit    *int     `json:"limit"`
}

func searchPiecesTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolSearchPieces, types.ToolClassExternal, true, false,
			"Recherche dans le texte des pièces du dossier (sémantique et plein texte). Renvoie des extraits de recherche et un curseur "+
				"read_piece pour chaque résultat. Un extrait n'est pas citable et une recherche ne vaut jamais lecture du dossier.",
			map[string]any{
				"query":     strSchema("Ce que vous cherchez dans les pièces, 500 caractères au plus."),
				"piece_ids": map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "string"}, "description": "Limiter à ces pièces du périmètre. Null : tout le périmètre."},
				"limit":     nullable(intSchema("Nombre de résultats, 1 à 20 (défaut 8).")),
			}),
		available: func() bool { return u.DB != nil && u.LLMConfigured() },
		exec:      execSearchPieces,
	}
}

func execSearchPieces(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectQuery, Label: "Recherche dans les pièces", Detail: "Recherche dans les pièces"}
	var a searchPiecesArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolSearchPieces, err, act)
	}
	query := strings.TrimSpace(a.Query)
	if query == "" || utf8.RuneCountInString(query) > maxPrivateQueryRunes {
		return invalid(x, types.ToolSearchPieces, badArgs("query est obligatoire, %d caractères au plus", maxPrivateQueryRunes), act)
	}
	act.Label = truncateRunes(query, 120)
	limit := searchPiecesDefault
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > searchPiecesMax {
			return invalid(x, types.ToolSearchPieces, badArgs("limit doit être entre 1 et %d", searchPiecesMax), act)
		}
		limit = *a.Limit
	}
	snap, final, err := privateScope(ctx, x, types.ToolSearchPieces, act)
	if final != nil || err != nil {
		return derefOutcome(final), err
	}

	// The subset searched is never wider than the scope: the requested pieces
	// (all of them in scope), else the scope itself whenever it is narrower
	// than the dossier.
	var subset []string
	switch {
	case a.PieceIDs != nil:
		subset = []string{}
		for _, id := range a.PieceIDs {
			if !snap.Contains(id) {
				return unknownPiece(x, types.ToolSearchPieces, act), nil
			}
			subset = append(subset, id)
		}
	case snap.Mode == types.ScopeExplicit || len(snap.Excluded) > 0:
		subset = snap.IDs()
	}

	var hits []Hit
	if subset == nil || len(subset) > 0 {
		opts := DefaultRetrievalOptions()
		opts.TopK = limit
		res, err := Retrieve(u.WithCallRef(ctx, callRef(x)), RetrievalRequest{
			Query: query, Corpus: types.CorpusPrivate, Repo: x.Repo(ctx), DossierID: snap.DossierID,
			Options: opts, PieceIDs: subset,
		})
		if errors.Is(err, u.ErrLLMNotConfigured) {
			return failure(x, types.ToolSearchPieces, types.ToolCodeToolUnavailable, false, "la recherche dans les pièces n'est pas configurée", nil, act), nil
		}
		if err != nil {
			pieceToolsLog.Warnf("Private search failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
			return failure(x, types.ToolSearchPieces, types.ToolCodeConnectorFailure, true, "la recherche dans les pièces a échoué", nil, act), nil
		}
		hits = res.Hits
	}

	texts := map[string]*PieceText{}
	results := make([]map[string]any, 0, len(hits))
	repo := x.Repo(ctx)
	for i, h := range hits {
		p, ok := snap.Piece(h.PieceID)
		if !ok {
			continue // indexed text of a piece outside the scope never surfaces
		}
		t, seen := texts[p.ID]
		if !seen {
			var asset *types.Asset
			if as, ok := snap.Assets[p.SourceAssetID]; ok {
				asset = &as
			}
			loaded, err := LoadPieceText(ctx, repo, p, asset)
			if err != nil {
				return ToolOutcome{}, err
			}
			t = &loaded
			texts[p.ID] = t
		}
		result := map[string]any{
			"rank":     i + 1,
			"piece_id": p.ID,
			"ordinal":  p.Ordinal,
			"name":     pieceName(p),
			"excerpt":  truncateRunes(strings.TrimSpace(h.Text), searchPieceExcerpt),
		}
		if pages := piecePageRange(h.PageFrom, h.PageTo, t.Format); pages != "" {
			result["pages"] = pages
		}
		// The hit is placed on the canonical text, at the exact offset its
		// chunk starts — a typed locator, never a page for a DOCX or XLSX.
		if seg, off, ok := t.locateChunk(h.Text, h.PageFrom); ok {
			result["read_cursor"] = FormatSegmentCursor(seg.ID, off)
			result["at"] = pieceLocatorText(t.segmentLocator(seg, off))
		} else if seg, ok := t.segment("p" + strconv.Itoa(h.PageFrom)); ok && t.Format == pieceFormatPDF {
			// The text changed since it was indexed: start of the page only.
			result["read_cursor"] = FormatSegmentCursor(seg.ID, 0)
		}
		results = append(results, result)
	}
	n := len(results)
	out := success(x, types.ToolSearchPieces, n == 0, map[string]any{
		"results": results,
		"note": "Extraits de recherche, non citables. Lisez le passage avec read_piece (read_cursor) avant de l'attribuer à une pièce. " +
			"Une recherche ne parcourt pas tout le dossier : l'absence de résultat ne prouve pas l'absence du fait.",
	}, act)
	out.Counts = counts(intPtr(n), nil)
	pieceToolsLog.Infof("Private search done (run=%s call=%s results=%d subset=%d)", x.RunID, x.CallID, n, len(subset))
	return out, nil
}

func piecePageRange(from, to int, format string) string {
	if format != pieceFormatPDF || from <= 0 {
		return ""
	}
	if to <= from {
		return strconv.Itoa(from)
	}
	return fmt.Sprintf("%d-%d", from, to)
}

// ---------------------------------------------------------------------------
// read_piece
// ---------------------------------------------------------------------------

type readPieceArgs struct {
	types.ToolArgs
	PieceID  string  `json:"piece_id"`
	Cursor   *string `json:"cursor"`
	MaxChars *int    `json:"max_chars"`
}

func readPieceTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolReadPiece, types.ToolClassDB, true, false,
			"Lit le texte exact d'une pièce à partir d'un curseur : un PDF page par page, découpé en paragraphes (p. 2, § 3) ; "+
				"un DOCX paragraphe par paragraphe (§ 4, sans numéro de page) ; un XLSX cellule par cellule, sous le vrai nom de "+
				"chaque feuille (Feuille!B4). Chaque passage a un evidence_id citable et compte dans la couverture de lecture. "+
				"Poursuivez avec next_cursor jusqu'à la fin de la pièce pour la lire entièrement.",
			map[string]any{
				"piece_id":  strSchema("Identifiant de la pièce (list_pieces)."),
				"cursor":    nullable(strSchema("Curseur « segment@décalage » (next_cursor, read_cursor, ou « p7 » pour la page 7, « para12 », « s2!B4 »). Null : début.")),
				"max_chars": nullable(intSchema("Caractères à lire, 1000 à 24000 (défaut 12000).")),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execReadPiece,
	}
}

func execReadPiece(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectPiece, Label: "Pièce"}
	var a readPieceArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolReadPiece, err, act)
	}
	budget := readPieceDefault
	if a.MaxChars != nil {
		if *a.MaxChars < readPieceMin || *a.MaxChars > readPieceMax {
			return invalid(x, types.ToolReadPiece, badArgs("max_chars doit être entre %d et %d", readPieceMin, readPieceMax), act)
		}
		budget = *a.MaxChars
	}
	snap, final, err := privateScope(ctx, x, types.ToolReadPiece, act)
	if final != nil || err != nil {
		return derefOutcome(final), err
	}
	p, ok := snap.Piece(strings.TrimSpace(a.PieceID))
	if !ok {
		return unknownPiece(x, types.ToolReadPiece, act), nil
	}
	act.Label = pieceLabel(p)
	var asset *types.Asset
	if as, ok := snap.Assets[p.SourceAssetID]; ok {
		asset = &as
	}
	text, err := LoadPieceText(ctx, x.Repo(ctx), p, asset)
	if err != nil {
		return ToolOutcome{}, err
	}
	switch text.State {
	case PieceTextPending:
		return failure(x, types.ToolReadPiece, types.ToolCodeSourceUnavailable, true,
			"la pièce est encore en cours d'ingestion ; son texte n'est pas encore lisible", map[string]any{"state": text.State}, act), nil
	case PieceTextUnreadable:
		return failure(x, types.ToolReadPiece, types.ToolCodeSourceUnavailable, false,
			"aucun texte n'a pu être extrait de cette pièce ; elle ne peut pas être lue ni citée",
			map[string]any{"state": text.State, "reason": text.Reason}, act), nil
	}

	ranges, skipped, next, err := text.readWindow(str(a.Cursor), budget)
	if err != nil {
		return invalid(x, types.ToolReadPiece, err, act)
	}
	// One receipt per delivered segment range (what coverage counts), one
	// evidence span per passage inside it (what a claim cites): a PDF
	// paragraph, a DOCX paragraph, an XLSX cell.
	var rows toolRows
	var view readView
	for _, r := range ranges {
		rows.receipts = append(rows.receipts, newReceipt(x, p.ID, text.SourceSHA256, text.ManifestSHA256, r.Seg.ID, r.Start, r.End, r.Text))
		for _, ps := range text.passagesOf(r) {
			// A pièce span carries the original's hash, as receipts and the
			// gate do; the receipt pins the extraction it was cut from.
			span := newSpan(x, types.ToolReadPiece, types.CitationSourcePiece, p.ID, text.SourceSHA256,
				pieceLocator(text, ps.Seg, ps.Start, ps.End, ps.Paragraph), ps.Text, nil)
			rows.spans = append(rows.spans, span)
			view.add(text.Format, r, ps, span.ID)
		}
	}
	unreadable := text.UnreadableIDs()
	data := map[string]any{
		"piece_id":            p.ID,
		"ordinal":             p.Ordinal,
		"name":                pieceName(p),
		"format":              text.Format,
		"unit":                text.Unit,
		"text_version":        text.Version,
		"segments_total":      len(text.Segments),
		"readable_segments":   text.Readable(),
		"unreadable_segments": boundedIDs(unreadable),
		"unreadable_count":    len(unreadable),
		"text_sha256":         text.ManifestSHA256,
		"skipped_unreadable":  boundedIDs(skipped),
		"end_of_piece":        next == "",
	}
	for k, v := range view.data(text.Format) {
		data[k] = v
	}
	if ext := extractionFacts(text); len(ext) > 0 {
		data["extraction"] = ext
	}
	if next != "" {
		data["next_cursor"] = next
	}
	if len(skipped) > 0 {
		data["warning"] = "Des segments sans texte extractible ont été sautés : ils ne sont pas lus et ne peuvent pas être cités."
	}
	out := withRows(success(x, types.ToolReadPiece, len(ranges) == 0, data, act), rows)
	out.Observation.NextCursor, out.Observation.Truncated = next, next != ""
	out.Activity.Detail = rangeDetail(text.Format, ranges)
	out.Counts = &types.AgentActivityCounts{Segments: intPtr(len(ranges)), Total: intPtr(text.Readable())}
	pieceToolsLog.Infof("Piece read (run=%s call=%s piece=%s ranges=%d skipped=%d more=%t)",
		x.RunID, x.CallID, p.ID, len(ranges), len(skipped), next != "")
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// maxListedSegments bounds a list of segment ids in an observation: a scan
// with hundreds of pages past the OCR cap is a count, not a wall of ids.
const maxListedSegments = 20

func boundedIDs(ids []string) []string {
	if len(ids) > maxListedSegments {
		return ids[:maxListedSegments]
	}
	return nonNil(ids)
}

// readView is read_piece's text, shaped like the pièce: PDF pages holding
// their paragraphs, DOCX paragraphs, XLSX cells grouped by sheet. Every
// passage carries its evidence id; the passages of one range tile it, so
// what the agent receives is exactly what the receipt records.
type readView struct {
	pages, paragraphs, sheets []map[string]any
}

func (v *readView) add(format string, r segmentRange, ps piecePassage, evidenceID string) {
	partial := func(item map[string]any) map[string]any {
		if ps.Start > 0 || ps.End < ps.Seg.Runes {
			item["start"], item["end"], item["length"] = ps.Start, ps.End, ps.Seg.Runes
		}
		return item
	}
	switch format {
	case pieceFormatDOCX:
		v.paragraphs = append(v.paragraphs, partial(map[string]any{
			"paragraph": ps.Seg.Paragraph, "segment_id": ps.Seg.ID, "evidence_id": evidenceID, "text": ps.Text}))
	case pieceFormatXLSX:
		if n := len(v.sheets); n == 0 || v.sheets[n-1]["sheet"] != ps.Seg.Sheet {
			v.sheets = append(v.sheets, map[string]any{"sheet": ps.Seg.Sheet, "cells": []map[string]any{}})
		}
		last := v.sheets[len(v.sheets)-1]
		last["cells"] = append(last["cells"].([]map[string]any), partial(map[string]any{
			"cell": ps.Seg.Cell, "evidence_id": evidenceID, "text": ps.Text}))
	default:
		if n := len(v.pages); n == 0 || v.pages[n-1]["segment_id"] != r.Seg.ID {
			v.pages = append(v.pages, map[string]any{
				"page": r.Seg.Page, "segment_id": r.Seg.ID, "start": r.Start, "end": r.End,
				"page_length": r.Seg.Runes, "complete_page": r.Start == 0 && r.End == r.Seg.Runes,
				"paragraphs": []map[string]any{},
			})
		}
		last := v.pages[len(v.pages)-1]
		last["paragraphs"] = append(last["paragraphs"].([]map[string]any), map[string]any{
			"paragraph": ps.Paragraph, "evidence_id": evidenceID, "start": ps.Start, "end": ps.End, "text": ps.Text})
	}
}

func (v *readView) data(format string) map[string]any {
	nonEmpty := func(items []map[string]any) []map[string]any {
		if items == nil {
			return []map[string]any{}
		}
		return items
	}
	switch format {
	case pieceFormatDOCX:
		return map[string]any{"paragraphs": nonEmpty(v.paragraphs)}
	case pieceFormatXLSX:
		return map[string]any{"sheets": nonEmpty(v.sheets)}
	}
	return map[string]any{"pages": nonEmpty(v.pages)}
}

// extractionFacts tells the agent what the extraction holds and what it left
// out: a PDF's expected and processed pages and OCR cap, a workbook's sheets,
// every limitation.
func extractionFacts(t PieceText) map[string]any {
	out := map[string]any{}
	if t.Format == pieceFormatPDF && t.ExpectedPages > 0 {
		out["expected_pages"], out["processed_pages"] = t.ExpectedPages, t.ProcessedPages
		if t.OCRPageCap > 0 {
			out["ocr_page_cap"] = t.OCRPageCap
		}
	}
	if len(t.Sheets) > 0 {
		out["sheets"] = t.Sheets
	}
	if len(t.Limitations) > 0 {
		notes := make([]map[string]any, 0, len(t.Limitations))
		for _, l := range t.Limitations {
			notes = append(notes, map[string]any{"code": l.Code, "note": limitationNote(l)})
		}
		out["limitations"] = notes
	}
	return out
}

// limitationNote says in French what a limitation leaves out.
func limitationNote(l ExtractionLimitation) string {
	switch l.Code {
	case LimitOCRPageCap:
		return fmt.Sprintf("%d page(s) au-delà du plafond OCR n'ont pas été lues : elles ne sont ni lisibles ni citables.", l.Count)
	case LimitDOCXNotes:
		return fmt.Sprintf("%d paragraphe(s) de notes de bas de page ou de fin ne sont pas extraits.", l.Count)
	case LimitDOCXComments:
		return fmt.Sprintf("%d paragraphe(s) de commentaires de relecture ne sont pas extraits.", l.Count)
	case LimitDOCXHeadersFooters:
		return fmt.Sprintf("%d paragraphe(s) d'en-tête ou de pied de page ne sont pas extraits.", l.Count)
	case LimitDOCXListLabels:
		return fmt.Sprintf("%d paragraphe(s) sont numérotés automatiquement : leur numéro n'apparaît pas dans le texte.", l.Count)
	case LimitXLSXHiddenSheet:
		return fmt.Sprintf("La feuille « %s » est masquée dans le classeur.", l.Detail)
	case LimitXLSXOtherSheets:
		return fmt.Sprintf("%d feuille(s) de graphique ou de macro ne sont pas lues.", l.Count)
	case LimitXLSXSheetMissing:
		return fmt.Sprintf("La feuille « %s » est introuvable dans le fichier.", l.Detail)
	case LimitXLSXFormulaNoValue:
		return fmt.Sprintf("%d cellule(s) à formule n'ont pas de valeur calculée enregistrée : leur résultat n'est pas dans le fichier.", l.Count)
	case LimitXLSXCellLimit:
		return fmt.Sprintf("Le classeur dépasse %d cellules : la suite n'est pas extraite.", l.Count)
	}
	return l.Code
}

// newReceipt is one delivered range, pinned to the original's hash and the
// extraction manifest. It counts toward coverage only once the runtime marks
// it consumed.
func newReceipt(x ToolExecution, pieceID, sourceSHA, manifestSHA, segmentID string, start, end int, text string) types.AgentReadReceipt {
	return types.AgentReadReceipt{
		ID:                agentRowID("rr_", x.RunID, x.CallID, pieceID, segmentID, strconv.Itoa(start), strconv.Itoa(end)),
		RunID:             x.RunID,
		PieceID:           pieceID,
		SourceSHA256:      sourceSHA,
		ExtractionVersion: manifestSHA,
		SegmentID:         segmentID,
		StartOffset:       start,
		EndOffset:         end,
		TextSHA256:        sha256Hex(text),
		ToolCallID:        x.CallID,
		RunStepID:         x.StepID,
	}
}

// rangeDetail is the human range of an activity row: "pages 3 à 5",
// "§ 1 à 40", "Récap!A1 à H22", with "(partiel)" when a segment was cut.
func rangeDetail(format string, ranges []segmentRange) string {
	if len(ranges) == 0 {
		return "fin de la pièce"
	}
	first, last := ranges[0], ranges[len(ranges)-1]
	partial := first.Start > 0 || last.End < last.Seg.Runes
	var s string
	switch format {
	case pieceFormatDOCX:
		s = spanText("§", first.Seg.Paragraph, last.Seg.Paragraph)
	case pieceFormatXLSX:
		s = excelRef(first.Seg.Sheet, first.Seg.Cell)
		switch {
		case first.Seg == last.Seg:
		case first.Seg.SheetIndex == last.Seg.SheetIndex:
			s += " à " + last.Seg.Cell
		default:
			s += " à " + excelRef(last.Seg.Sheet, last.Seg.Cell)
		}
	default:
		if first.Seg.Page == last.Seg.Page {
			s = "page " + strconv.Itoa(first.Seg.Page)
		} else {
			s = fmt.Sprintf("pages %d à %d", first.Seg.Page, last.Seg.Page)
		}
	}
	if partial {
		s += " (partiel)"
	}
	return s
}

func spanText(unit string, a, b int) string {
	if a == b {
		return fmt.Sprintf("%s %d", unit, a)
	}
	return fmt.Sprintf("%s %d à %d", unit, a, b)
}

// ---------------------------------------------------------------------------
// read_evidence
// ---------------------------------------------------------------------------

type readEvidenceArgs struct {
	types.ToolArgs
	EvidenceIDs []string `json:"evidence_ids"`
}

func readEvidenceTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolReadEvidence, types.ToolClassDB, true, false,
			"Relit le texte exact et figé d'extraits déjà obtenus dans cette tâche (evidence_id), par exemple après qu'ils ont "+
				"quitté le contexte. Au plus 20 extraits par appel.",
			map[string]any{
				"evidence_ids": arraySchema("Identifiants evidence_id déjà renvoyés dans cette tâche.", map[string]any{"type": "string"}),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execReadEvidence,
	}
}

func execReadEvidence(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectEvidence, Label: "Relecture d'extraits"}
	var a readEvidenceArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolReadEvidence, err, act)
	}
	ids := dedupeStrings(a.EvidenceIDs)
	if len(ids) == 0 || len(ids) > readEvidenceMaxIDs {
		return invalid(x, types.ToolReadEvidence, badArgs("evidence_ids : 1 à %d identifiants", readEvidenceMaxIDs), act)
	}
	repo := x.Repo(ctx)
	var spans []types.AgentEvidenceSpan
	if err := repo.AgentEvidenceSpans().Where("run_id = ? AND id IN ?", x.RunID, ids).Find(&spans).Error; err != nil {
		return ToolOutcome{}, err
	}
	found := map[string]types.AgentEvidenceSpan{}
	for _, s := range spans {
		found[s.ID] = s
	}
	var unknown []string
	runes := 0
	for _, id := range ids {
		s, ok := found[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		runes += utf8.RuneCountInString(s.Quote)
	}
	if len(unknown) > 0 {
		return failure(x, types.ToolReadEvidence, types.ToolCodeUnknownReference, false,
			"extraits inconnus dans cette tâche", map[string]any{"unknown_ids": unknown}, act), nil
	}
	if runes > readEvidenceMaxRunes {
		return failure(x, types.ToolReadEvidence, types.ToolCodeProcessingLimit, false,
			fmt.Sprintf("ces extraits totalisent plus de %d caractères : demandez-en moins à la fois", readEvidenceMaxRunes), nil, act), nil
	}

	// A re-delivered pièce passage is re-read text: it gets a receipt of its
	// own, pinned to the current extraction — only while that extraction
	// still holds the exact frozen passage at its locator. A piece changed
	// since gives no receipt: its old text proves nothing about today's.
	texts, err := pieceTextsOf(ctx, repo, spans)
	if err != nil {
		return ToolOutcome{}, err
	}
	var rows toolRows
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		s := found[id]
		var loc types.SourceLocator
		_ = json.Unmarshal(s.Locator, &loc)
		item := map[string]any{
			"evidence_id": s.ID, "source_type": s.SourceType, "locator": loc,
			"quote": s.Quote, "retrieved_by": s.RetrievalTool,
		}
		switch {
		case s.PieceID != nil:
			item["piece_id"] = *s.PieceID
			if t := texts[*s.PieceID]; t != nil && t.SourceSHA256 == s.SourceSHA256 && loc.SegmentID != "" {
				if seg, ok := t.segment(loc.SegmentID); ok && loc.Start >= 0 && loc.Start < loc.End && loc.End <= seg.Runes &&
					string(seg.runeSlice()[loc.Start:loc.End]) == s.Quote {
					rows.receipts = append(rows.receipts,
						newReceipt(x, *s.PieceID, t.SourceSHA256, t.ManifestSHA256, loc.SegmentID, loc.Start, loc.End, s.Quote))
				}
			}
		case s.LegalVersionID != nil:
			item["legal_version_id"] = *s.LegalVersionID
		case s.WebSourceID != nil:
			item["web_source_id"] = *s.WebSourceID
		}
		items = append(items, item)
	}
	out := withRows(success(x, types.ToolReadEvidence, false, map[string]any{"evidence": items}, act), rows)
	for _, id := range ids {
		out.Observation.EvidenceIDs = appendUnique(out.Observation.EvidenceIDs, id)
	}
	out.Activity.Label = fmt.Sprintf("Relecture de %d extrait(s)", len(ids))
	out.Counts = counts(intPtr(len(ids)), nil)
	pieceToolsLog.Debugf("Evidence re-read (run=%s call=%s spans=%d receipts=%d)", x.RunID, x.CallID, len(ids), len(rows.receipts))
	return out, nil
}

// pieceTextsOf loads the current canonical extraction of the pieces behind
// piece spans, through the tenant predicate. A piece whose text is not ready
// is left out.
func pieceTextsOf(ctx context.Context, repo *u.TenantRepo, spans []types.AgentEvidenceSpan) (map[string]*PieceText, error) {
	var pieceIDs []string
	for _, s := range spans {
		if s.PieceID != nil {
			pieceIDs = append(pieceIDs, *s.PieceID)
		}
	}
	out := map[string]*PieceText{}
	if len(pieceIDs) == 0 {
		return out, nil
	}
	var pieces []types.Piece
	if err := repo.Pieces().Where("id IN ?", dedupeStrings(pieceIDs)).Find(&pieces).Error; err != nil {
		return nil, err
	}
	for _, p := range pieces {
		t, err := LoadPieceText(ctx, repo, p, nil)
		if err != nil {
			return nil, err
		}
		if t.State == PieceTextReady {
			out[p.ID] = &t
		}
	}
	return out, nil
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// FormatPieceDate renders a date at the precision that was actually read.
// A month-precise date is written "09-2019" and a year-precise one "2019":
// padding either into a full day would put a date on a court document that no
// document bears.
func FormatPieceDate(unix *int64, precision string) string {
	if unix == nil {
		return "sans date"
	}
	t := time.Unix(*unix, 0).UTC()
	switch precision {
	case types.DatePrecisionYear:
		return t.Format("2006")
	case types.DatePrecisionMonth:
		return t.Format("01-2006")
	default:
		return t.Format("02-01-2006")
	}
}
