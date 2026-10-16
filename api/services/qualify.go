package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var qualifyLog = u.NewLogger("services.qualify")

// Piece qualification — docs/PLAN.md §7.1(4).
//
// ONE cheap LLM call per piece against a SERVER-OWNED JSON schema. Three
// things about it are load-bearing:
//
//   - The piece kind comes from a CLOSED French vocabulary (pieceKinds below).
//     The model PICKS from the list; it never invents a label. A free-text
//     type column would make the classement's rubriques — which are derived
//     from the kinds — impossible to compute deterministically.
//   - The parties are written into piece_parties ROWS with a normalized_name,
//     not buried in the extraction JSON. Burying them is the schema §9.1(6)
//     says we would regret: name reconciliation and timeline queries would
//     become an expensive rewrite.
//   - Low confidence is SURFACED for the lawyer to correct, never silently
//     accepted (§7.1(6)). Nothing here ever writes classification_state =
//     approved: approval is a human act.
//
// The step is idempotent. Re-running it for the same piece UPDATES the row and
// REPLACES its parties, so the at-least-once runner (§4.3) cannot leave a
// piece with two sets of parties.

// StepKindQualifyPiece is the runner step kind. One step per piece: the
// fan-out width is data, the shape is code (§4.1).
const StepKindQualifyPiece = "qualify_piece"

// QualifyExtractionVersion pins prompt + schema + parser. It lands in
// pieces.extraction_version so a re-run is comparable and a regression is
// attributable (§7.1).
const QualifyExtractionVersion = "qualify_piece_v2"

// qualifyReviewThreshold is the confidence below which a qualification is
// flagged for lawyer review. It gates a FLAG, never an acceptance — every
// proposed field is editable before export whatever the score.
const qualifyReviewThreshold = 0.75

// qualifyMaxTextRunes bounds what is sent to the gateway. A qualification
// needs the head of a document — the letterhead, the date, the parties, the
// object — not its 80th page of annexes, and an unbounded prompt is an
// unbounded bill.
const qualifyMaxTextRunes = 20000

// The CLOSED vocabulary of piece kinds. French, snake_case, and deliberately
// small: a bordereau rubrique is derived from these values, so a new kind is a
// code change reviewed alongside the rubrique it lands in — never a label a
// model invented on a Tuesday.
const (
	PieceKindContratDeTravail   = "contrat_de_travail"
	PieceKindAvenant            = "avenant"
	PieceKindBulletinDePaie     = "bulletin_de_paie"
	PieceKindLettreLicenciement = "lettre_de_licenciement"
	PieceKindMiseEnDemeure      = "mise_en_demeure"
	PieceKindConvocationEP      = "convocation_entretien_prealable"
	PieceKindAttestation        = "attestation"
	PieceKindCourriel           = "courriel"
	PieceKindKbis               = "kbis"
	PieceKindFacture            = "facture"
	PieceKindJugement           = "jugement"
	PieceKindConclusions        = "conclusions"
	PieceKindProcesVerbal       = "proces_verbal"
	PieceKindCertificatMedical  = "certificat_medical"
	PieceKindAutre              = "autre"
)

// pieceKinds is the enum sent to the provider AND the list validated against
// locally — a provider honouring an enum is a courtesy, not a guarantee.
// Order is fixed so the schema, and therefore the prompt, is byte-stable.
var pieceKinds = []string{
	PieceKindContratDeTravail,
	PieceKindAvenant,
	PieceKindBulletinDePaie,
	PieceKindLettreLicenciement,
	PieceKindMiseEnDemeure,
	PieceKindConvocationEP,
	PieceKindAttestation,
	PieceKindCourriel,
	PieceKindKbis,
	PieceKindFacture,
	PieceKindJugement,
	PieceKindConclusions,
	PieceKindProcesVerbal,
	PieceKindCertificatMedical,
	PieceKindAutre,
}

// pieceKindLabels is the human French wording used in a bordereau line. The
// classement builds "Pièce n°1 - Contrat de travail - 12-09-2019.pdf" from it,
// which is why it lives next to the vocabulary rather than in a locale file:
// it is the legal form of the document, not UI chrome.
var pieceKindLabels = map[string]string{
	PieceKindContratDeTravail:   "Contrat de travail",
	PieceKindAvenant:            "Avenant",
	PieceKindBulletinDePaie:     "Bulletin de paie",
	PieceKindLettreLicenciement: "Lettre de licenciement",
	PieceKindMiseEnDemeure:      "Mise en demeure",
	PieceKindConvocationEP:      "Convocation à entretien préalable",
	PieceKindAttestation:        "Attestation",
	PieceKindCourriel:           "Courriel",
	PieceKindKbis:               "Extrait Kbis",
	PieceKindFacture:            "Facture",
	PieceKindJugement:           "Jugement",
	PieceKindConclusions:        "Conclusions",
	PieceKindProcesVerbal:       "Procès-verbal",
	PieceKindCertificatMedical:  "Certificat médical",
	PieceKindAutre:              "Pièce diverse",
}

// PieceKinds returns the closed vocabulary. Exported so the lawyer-correction
// endpoint and the frontend can offer exactly the same list the model was
// given — a correction UI with a different vocabulary is a second, silent
// schema.
func PieceKinds() []string { return append([]string(nil), pieceKinds...) }

// IsPieceKind reports whether s is in the closed vocabulary.
func IsPieceKind(s string) bool {
	_, ok := pieceKindLabels[s]
	return ok
}

// PieceKindLabel is the French wording for a kind. An unknown kind falls back
// to the "autre" wording rather than leaking a raw identifier into a document
// that goes to a court.
func PieceKindLabel(kind string) string {
	if label, ok := pieceKindLabels[kind]; ok {
		return label
	}
	return pieceKindLabels[PieceKindAutre]
}

// partyRoles is the closed role vocabulary, mirroring types.PartyRole*.
var partyRoles = []string{
	types.PartyRoleEmitter,
	types.PartyRoleRecipient,
	types.PartyRoleParty,
	types.PartyRoleThirdParty,
	types.PartyRoleRepresented,
}

// ---------------------------------------------------------------------------
// Step input / output
// ---------------------------------------------------------------------------

// QualifyPieceInput is the step's jsonb input.
//
// Text is optional: the ingestion step that ran `pdftotext` already holds the
// extracted text and passing it forward saves a second read. When it is empty
// the text is recovered from piece_chunks, so a REPLAYED attempt (the runner
// is at-least-once) still has something to work from.
type QualifyPieceInput struct {
	PieceID string `json:"piece_id"`
	Text    string `json:"text,omitempty"`
}

// QualifyPieceOutput is what lands on the step row. Shapes and identifiers
// only — never a party name, a title or an excerpt (§7.4).
type QualifyPieceOutput struct {
	PieceID       string  `json:"piece_id"`
	PieceKind     string  `json:"piece_kind"`
	DocumentDate  *int64  `json:"document_date"`
	DatePrecision string  `json:"date_precision"`
	PartyCount    int     `json:"party_count"`
	Confidence    float64 `json:"confidence"`
	NeedsReview   bool    `json:"needs_review"`
}

// qualification is the decoded model answer. It is also the shape stored in
// pieces.extraction, so the raw proposal stays inspectable next to the typed
// columns derived from it.
type qualification struct {
	PieceKind     string             `json:"piece_kind"`
	DocumentDate  string             `json:"document_date"`
	DatePrecision string             `json:"date_precision"`
	Title         string             `json:"title"`
	Summary       string             `json:"summary"`
	Parties       []qualifiedParty   `json:"parties"`
	Confidence    float64            `json:"confidence"`
	Meta          *qualificationMeta `json:"meta,omitempty"`
}

type qualifiedParty struct {
	Role        string `json:"role"`
	DisplayName string `json:"display_name"`
}

// qualificationMeta is written by us, not by the model: what the server made
// of the answer. It is never part of the schema the provider sees.
type qualificationMeta struct {
	ExtractionVersion string `json:"extraction_version"`
	Model             string `json:"model"`
	NeedsReview       bool   `json:"needs_review"`
	QualifiedAt       int64  `json:"qualified_at"`
}

// qualifySchema is the SERVER-OWNED JSON schema. It is sent as response_format
// and re-validated locally by utils.ChatJSON, with exactly one repair
// round-trip; there is no regex for "the first JSON-looking blob" (§4.3).
//
// document_date is a STRING, not a number, precisely so a partially legible
// date ("mars 2020") can be expressed as "2020-03" instead of being rounded
// into a fabricated day.
func qualifySchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []any{
			"piece_kind", "document_date", "date_precision", "title", "summary", "parties", "confidence",
		},
		"properties": map[string]any{
			"piece_kind": map[string]any{"enum": toAny(pieceKinds)},
			"document_date": map[string]any{
				"type": "string",
			},
			"date_precision": map[string]any{"enum": []any{
				types.DatePrecisionDay, types.DatePrecisionMonth,
				types.DatePrecisionYear, types.DatePrecisionUnknown,
			}},
			"title":   map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"},
			"parties": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"role", "display_name"},
					"properties": map[string]any{
						"role":         map[string]any{"enum": toAny(partyRoles)},
						"display_name": map[string]any{"type": "string"},
					},
				},
			},
			"confidence": map[string]any{"type": "number"},
		},
	}
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

// qualifySystemPrompt is STATIC: the identity and the task, and
// nothing about this particular piece. Every variable byte — the filename, the
// document text — travels in the user message.
const qualifySystemPrompt = `<role>
Tu es un assistant juridique français. Tu qualifies les pièces d'un dossier
d'avocat en vue de l'établissement d'un bordereau de communication de pièces.
Tu ne donnes aucun conseil juridique, tu ne commentes pas le fond, et tu
n'inventes jamais une information absente du document.
</role>

<tasks>
1. Déterminer le TYPE de la pièce (piece_kind). Tu dois choisir une valeur de
   la liste fermée imposée par le schéma, et rien d'autre. Si le document ne
   correspond à aucune de ces catégories, choisis "autre" — n'invente jamais
   une nouvelle étiquette et ne rapproche pas un document d'une catégorie qui
   ne lui correspond pas.
2. Relever la DATE DE L'ACTE (document_date) : la date que le document porte
   lui-même (date de signature, d'établissement, d'envoi, de l'audience), et
   non la date d'un fait qu'il relate. Format ISO :
   "AAAA-MM-JJ" si le jour est lisible, "AAAA-MM" si seul le mois l'est,
   "AAAA" si seule l'année l'est, chaîne vide "" si aucune date n'est lisible.
   Ne complète jamais un jour ou un mois que le document ne porte pas.
3. Indiquer la précision de cette date (date_precision) : "day", "month",
   "year" ou "unknown", en cohérence stricte avec document_date.
4. Rédiger un TITRE court et factuel (title), en français, sans ponctuation
   finale : la nature du document et son objet, par exemple
   "Contrat de travail à durée indéterminée" ou
   "Bulletin de paie du mois de janvier 2020".
5. Rédiger un RÉSUMÉ (summary) de deux ou trois phrases, en français : ce que
   le document établit ou relate — les faits, les demandes, les dates et les
   montants qu'il porte. Factuel : aucune appréciation juridique, aucune
   information absente du document. Ce résumé sert à décider quelles pièces
   une mission doit exploiter.
6. Lister les PARTIES nommées par le document (parties), avec pour chacune :
   - role : "emetteur" (qui écrit ou établit l'acte), "destinataire" (à qui il
     est adressé), "partie" (une partie au contrat ou au litige), "tiers"
     (un témoin, un médecin, un huissier, un organisme) ou "represente"
     (la personne représentée par un mandataire) ;
   - display_name : le nom tel qu'il figure dans le document, personne
     physique ou morale, sans civilité superflue.
   N'invente aucune partie. Si le document ne nomme personne, renvoie [].
7. Évaluer ta CONFIANCE globale (confidence) entre 0 et 1. Sois sévère : un
   document tronqué, illisible, sans date ou dont le type est ambigu doit
   recevoir une confiance basse. Une confiance basse est utile — elle appelle
   la relecture de l'avocat.
</tasks>

<output>
Un unique document JSON, conforme exactement au schéma imposé, sans texte
avant ni après, sans bloc de code, sans commentaire.
</output>`

// ---------------------------------------------------------------------------
// The step
// ---------------------------------------------------------------------------

func init() {
	// The LLM pool: slow but idle. It must not occupy the single OCR or
	// render slot, and it must not make a cheap db transition wait (§4.2).
	runner.Register(StepKindQualifyPiece, qualifyPieceStep, runner.RegisterOptions{
		Pool:        types.StepKindLLM,
		MaxAttempts: 3,
	})
}

func qualifyPieceStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in QualifyPieceInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	if in.PieceID == "" {
		return nil, runner.Permanent("missing_piece_id", "a qualify_piece step needs a piece_id")
	}

	repo := u.ForTenantID(ctx, sc.TenantID)
	out, err := QualifyPiece(ctx, repo, sc.DossierID, in, u.CallRef{
		TenantID:      sc.TenantID,
		RunID:         sc.Run.ID,
		RunStepID:     sc.Step.ID,
		StepAttemptID: sc.Attempt.ID,
	})
	if err != nil {
		return nil, err
	}

	// Low confidence is surfaced to the thread, never swallowed. The payload
	// carries the score and the kind — both shapes — and no document content.
	if out.NeedsReview {
		_ = sc.Emit(types.EventStepProgress, map[string]any{
			"piece_id":     out.PieceID,
			"piece_kind":   out.PieceKind,
			"confidence":   out.Confidence,
			"needs_review": true,
		})
	}
	return out, nil
}

// QualifyPiece qualifies one piece and writes the result. It is the whole step
// minus the runner, so it is callable from a test, from a re-qualification
// endpoint, or from a future batch job.
//
// dossierID, when non-empty, is an additional predicate: a step belonging to
// one dossier may not qualify a piece of another, even inside the same tenant.
func QualifyPiece(ctx context.Context, repo *u.TenantRepo, dossierID string, in QualifyPieceInput, ref u.CallRef) (QualifyPieceOutput, error) {
	var out QualifyPieceOutput

	q := repo.Pieces().Where("id = ?", in.PieceID)
	if dossierID != "" {
		q = q.Where("dossier_id = ?", dossierID)
	}
	var piece types.Piece
	if err := q.Take(&piece).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, runner.Permanent("piece_not_found", "the piece does not exist in this dossier")
		}
		qualifyLog.Errorf("Piece lookup failed (piece=%s): %v", in.PieceID, err)
		return out, runner.Retry("db_error", "piece lookup failed")
	}

	text, err := qualifyText(repo, piece, in.Text)
	if err != nil {
		return out, err
	}

	cfg := config.Load()
	prompt := qualifyUserPrompt(piece.OriginalName, text)
	qualifyLog.Debugf("Qualifying piece (piece=%s dossier=%s model=%s text_chars=%d prompt_chars=%d)",
		piece.ID, piece.DossierID, cfg.ExtractionModel, len([]rune(text)), len(prompt))

	// Temperature 0: two runs over the same document should not disagree
	// about what kind of document it is.
	zero := 0.0
	var answer qualification
	err = u.ChatJSON(ctx, u.ChatOpts{
		Model:         cfg.ExtractionModel,
		System:        qualifySystemPrompt,
		User:          prompt,
		Temperature:   &zero,
		MaxTokens:     1500,
		Purpose:       types.LLMPurposeQualify,
		TenantID:      ref.TenantID,
		RunID:         ref.RunID,
		RunStepID:     ref.RunStepID,
		StepAttemptID: ref.StepAttemptID,
	}, qualifySchema(), &answer)
	if err != nil {
		return out, qualifyFailure(repo, piece, err)
	}

	normalizeQualification(&answer)
	documentDate, precision := parseDocumentDate(answer.DocumentDate, answer.DatePrecision)
	answer.DatePrecision = precision
	needsReview := answer.Confidence < qualifyReviewThreshold ||
		answer.PieceKind == PieceKindAutre ||
		precision == types.DatePrecisionUnknown
	answer.Meta = &qualificationMeta{
		ExtractionVersion: QualifyExtractionVersion,
		Model:             cfg.ExtractionModel,
		NeedsReview:       needsReview,
		QualifiedAt:       time.Now().Unix(),
	}

	extraction, err := json.Marshal(answer)
	if err != nil {
		return out, runner.Permanent("unencodable_extraction", "the qualification could not be encoded")
	}

	// The row update and the party replacement commit together: a piece is
	// never visible with a new kind and a previous run's parties.
	parties := make([]types.PiecePartie, 0, len(answer.Parties))
	for _, p := range answer.Parties {
		parties = append(parties, types.PiecePartie{
			ID:             uuid.NewString(),
			PieceID:        piece.ID,
			Role:           p.Role,
			DisplayName:    p.DisplayName,
			NormalizedName: NormalizePartyName(p.DisplayName),
		})
	}
	err = repo.Tx(func(tx *u.TenantRepo) error {
		if err := tx.Pieces().Where("id = ?", piece.ID).Updates(map[string]any{
			"piece_kind":         answer.PieceKind,
			"title":              answer.Title,
			"summary":            answer.Summary,
			"document_date":      documentDate,
			"date_precision":     precision,
			"confidence":         answer.Confidence,
			"extraction":         types.JSONB(extraction),
			"extraction_version": QualifyExtractionVersion,
			// proposed, never approved: approval is the lawyer's act (§7.1).
			"classification_state": types.PieceStateProposed,
		}).Error; err != nil {
			return err
		}
		// Replace, don't append: a replayed attempt must not double the
		// parties of a piece (§4.3).
		if err := tx.PieceParties().Where("piece_id = ?", piece.ID).
			Delete(&types.PiecePartie{}).Error; err != nil {
			return err
		}
		if len(parties) == 0 {
			return nil
		}
		return tx.Create(&parties)
	})
	if err != nil {
		qualifyLog.Errorf("Qualification write failed (piece=%s): %v", piece.ID, err)
		return out, runner.Retry("db_error", "qualification could not be stored")
	}

	out = QualifyPieceOutput{
		PieceID:       piece.ID,
		PieceKind:     answer.PieceKind,
		DocumentDate:  documentDate,
		DatePrecision: precision,
		PartyCount:    len(parties),
		Confidence:    answer.Confidence,
		NeedsReview:   needsReview,
	}
	if needsReview {
		qualifyLog.Warnf("Qualification needs review (piece=%s kind=%s precision=%s confidence=%.2f)",
			piece.ID, out.PieceKind, precision, answer.Confidence)
	}
	qualifyLog.Infof("Piece qualified (piece=%s dossier=%s kind=%s precision=%s parties=%d confidence=%.2f)",
		piece.ID, piece.DossierID, out.PieceKind, precision, out.PartyCount, answer.Confidence)
	return out, nil
}

// qualifyFailure maps a gateway error onto a step error, and records a
// permanent failure on the piece so the lawyer sees a piece that could not be
// qualified rather than one that silently stayed "pending".
func qualifyFailure(repo *u.TenantRepo, piece types.Piece, err error) error {
	switch {
	case errors.Is(err, u.ErrLLMNotConfigured):
		// Configuration, not input: nothing on the piece changes.
		return runner.Permanent("llm_not_configured", "the LLM gateway is not configured")
	case errors.Is(err, u.ErrSchemaInvalid), errors.Is(err, u.ErrRefused), errors.Is(err, u.ErrTruncated):
		markPieceFailed(repo, piece.ID)
		return runner.Permanent("qualification_invalid", "the model did not return a usable qualification")
	case u.IsRetryable(err):
		return runner.Retry("gateway_error", "the LLM gateway call failed")
	default:
		markPieceFailed(repo, piece.ID)
		return runner.Permanent("qualification_failed", "the piece could not be qualified")
	}
}

func markPieceFailed(repo *u.TenantRepo, pieceID string) {
	if err := repo.Pieces().Where("id = ?", pieceID).
		Update("classification_state", types.PieceStateFailed).Error; err != nil {
		qualifyLog.Errorf("Could not mark piece failed (piece=%s): %v", pieceID, err)
	}
}

// qualifyText returns the text to qualify: the ingestion step's own extraction
// when it passed one, otherwise the piece's stored chunks.
func qualifyText(repo *u.TenantRepo, piece types.Piece, provided string) (string, error) {
	if text := strings.TrimSpace(provided); text != "" {
		return text, nil
	}
	var chunks []types.PieceChunk
	if err := repo.PieceChunks().Where("piece_id = ?", piece.ID).
		Order("ordinal ASC").Find(&chunks).Error; err != nil {
		qualifyLog.Errorf("Chunk lookup failed (piece=%s): %v", piece.ID, err)
		return "", runner.Retry("db_error", "piece text lookup failed")
	}
	parts := make([]string, 0, len(chunks))
	for _, ch := range chunks {
		parts = append(parts, ch.Text)
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		// Not retryable: another identical attempt finds the same absence.
		// Slice 1 is text-layer PDFs only (§7.1(3)); a scan needs OCR first.
		return "", runner.Permanent("piece_text_unavailable", "no extracted text is available for this piece")
	}
	return text, nil
}

// qualifyUserPrompt carries EVERY variable byte: the filename and the text.
// Nothing here is ever logged (§7.4).
func qualifyUserPrompt(originalName, text string) string {
	runes := []rune(text)
	truncated := false
	if len(runes) > qualifyMaxTextRunes {
		runes = runes[:qualifyMaxTextRunes]
		truncated = true
	}
	var b strings.Builder
	b.WriteString("<piece>\n<nom_de_fichier>")
	b.WriteString(originalName)
	b.WriteString("</nom_de_fichier>\n<texte>\n")
	b.WriteString(string(runes))
	b.WriteString("\n</texte>\n")
	if truncated {
		b.WriteString("<note>Le texte a été tronqué : seul le début du document est fourni.</note>\n")
	}
	b.WriteString("</piece>")
	return b.String()
}

// normalizeQualification repairs what the schema cannot express: a kind
// outside the vocabulary, an out-of-range confidence, a blank or duplicated
// party. A provider honouring an enum is a courtesy, not a guarantee.
func normalizeQualification(q *qualification) {
	if !IsPieceKind(q.PieceKind) {
		qualifyLog.Warnf("Model returned an out-of-vocabulary piece kind — falling back to %q", PieceKindAutre)
		q.PieceKind = PieceKindAutre
	}
	q.Title = strings.TrimSpace(q.Title)
	q.Summary = strings.TrimSpace(q.Summary)
	if q.Confidence < 0 {
		q.Confidence = 0
	}
	if q.Confidence > 1 {
		q.Confidence = 1
	}

	seen := map[string]bool{}
	kept := q.Parties[:0]
	for _, p := range q.Parties {
		p.DisplayName = strings.TrimSpace(p.DisplayName)
		if p.DisplayName == "" {
			continue
		}
		if !isPartyRole(p.Role) {
			p.Role = types.PartyRoleParty
		}
		key := p.Role + "|" + NormalizePartyName(p.DisplayName)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, p)
	}
	q.Parties = kept
}

func isPartyRole(role string) bool {
	for _, r := range partyRoles {
		if r == role {
			return true
		}
	}
	return false
}

// parseDocumentDate turns the model's ISO string into unix seconds plus the
// precision the string actually supports.
//
// The STRING is the evidence, so its shape wins over the declared precision: a
// model that returns "2020-03" and claims "day" has not read a day, and a
// fabricated 1st of March in a bordereau is a factual error in a document
// filed with a court. A date with no legible year is no date at all.
func parseDocumentDate(raw, declared string) (*int64, string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, types.DatePrecisionUnknown
	}
	layouts := []struct {
		layout    string
		precision string
	}{
		{"2006-01-02", types.DatePrecisionDay},
		{"2006-01", types.DatePrecisionMonth},
		{"2006", types.DatePrecisionYear},
	}
	for _, l := range layouts {
		t, err := time.ParseInLocation(l.layout, s, time.UTC)
		if err != nil {
			continue
		}
		if declared != "" && declared != l.precision {
			qualifyLog.Debugf("Declared date precision %q disagrees with the value's shape — using %q",
				declared, l.precision)
		}
		unix := t.Unix()
		return &unix, l.precision
	}
	qualifyLog.Warnf("Unparseable document date (len=%d) — recorded as unknown", len(s))
	return nil, types.DatePrecisionUnknown
}

// NormalizePartyName is the reconciliation key for piece_parties: accents
// folded, punctuation dropped, whitespace collapsed, lower-cased, and the
// common French civilities and company forms removed. "M. Jean-Luc Moreau" and
// "MOREAU Jean Luc" both become "jean luc moreau" — close enough to group two
// mentions of the same person across a dossier, and cheap enough to index.
//
// It is a KEY, not a display value: display_name keeps the document's own
// wording, which is what a bordereau must show.
func NormalizePartyName(name string) string {
	folded := make([]rune, 0, len(name))
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if repl, ok := accentFolding[r]; ok {
			folded = append(folded, []rune(repl)...)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			folded = append(folded, r)
			continue
		}
		folded = append(folded, ' ')
	}
	words := strings.Fields(string(folded))
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if partyNoiseWords[w] {
			continue
		}
		kept = append(kept, w)
	}
	out := strings.Join(kept, " ")
	if len(out) > 512 {
		out = out[:512]
	}
	return out
}

// accentFolding covers the French repertoire plus the ligatures. A rune table
// beats pulling in a transform chain for fifteen characters.
var accentFolding = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a",
	'ç': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'ö': "o", 'õ': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u",
	'ý': "y", 'ÿ': "y",
	'œ': "oe", 'æ': "ae", 'ß': "ss",
}

// partyNoiseWords are the civilities and company forms that carry no identity.
// Dropping them is what makes "Maître Durand" and "Durand" the same key.
var partyNoiseWords = map[string]bool{
	"m": true, "mr": true, "mme": true, "mlle": true, "monsieur": true,
	"madame": true, "mademoiselle": true, "me": true, "maitre": true,
	"dr": true, "docteur": true,
	"sa": true, "sas": true, "sasu": true, "sarl": true, "eurl": true,
	"sci": true, "sncf": true, "scp": true, "selarl": true, "societe": true,
}
