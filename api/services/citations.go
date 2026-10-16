package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var citationsLog = u.NewLogger("services.citations")

// Citations — the resolve-or-refuse core (docs/PLAN.md §6.4 and §13).
//
// This file exists because a fabricated citation in a filed pleading
// prejudices the client, misleads the court, creates liability and breaches
// competence, prudence, independence and diligence. It is the most important
// safety property in the product, so it is written as a REFUSAL, not as a
// score:
//
//  1. NO CITATION IS EVER GENERATED FROM MODEL MEMORY ALONE. A reference the
//     model emits must name a source that the retrieval step actually put in
//     front of it. A reference that resolves in the database but was never
//     retrieved is refused too — otherwise a model that happens to guess a
//     real version id has laundered a memory into a citation.
//  2. Every citation binds to an IMMUTABLE retrieved version AND to an exact
//     supporting quote. A citation without a quote is not a citation.
//  3. The quote must actually occur in the stored source text. A plausible
//     paraphrase is a hallucination with better manners.
//  4. The three verification states are never collapsed into one green badge:
//     `retrieved` (the machine found it), `machine_checked` (the quote and the
//     version still match the source at export time), `lawyer_verified` (a
//     human took responsibility). Nothing in this file ever writes the third.
//
// Failing any of 1–3 REFUSES THE DOCUMENT. There is no "drop the bad citation
// and render the rest": the sentence it supported would then be an unsourced
// assertion of law, which is exactly the artefact we must never produce.

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

var (
	// ErrCitationMalformed — the reference is not "legal:<id>" / "piece:<id>".
	ErrCitationMalformed = errors.New("référence de citation malformée")
)

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// SourceRecord is the stored source a reference resolved to.
type SourceRecord struct {
	SourceType string // types.CitationSource*
	ID         string
	Label      string
	Text       string
	SHA256     string
	PageFrom   int
	PageTo     int
	// ValidFrom/ValidTo are a legal version's half-open validity interval;
	// nil bounds are open (a decision has neither).
	ValidFrom *int64
	ValidTo   *int64
}

// ErrSourceNotFound is what a resolver returns for an id it does not hold.
var ErrSourceNotFound = errors.New("source not found")

// ---------------------------------------------------------------------------
// Quotes
// ---------------------------------------------------------------------------

// minQuoteRunes is the shortest passage we will accept as support. A three-word
// "quote" occurs in almost any legal text by accident, so a citation resting on
// one proves nothing.
const minQuoteRunes = 24

// QuoteOccursIn reports whether quote actually appears in source.
//
// The comparison folds case, accents-preserving Unicode normalisation is left
// to the ingestion side, and it collapses the differences a PDF text layer
// invents — typographic apostrophes, non-breaking spaces, soft hyphens, line
// wrapping. It does NOT tolerate reordered or paraphrased words: those are the
// hallucinations this function exists to catch.
func QuoteOccursIn(quote, source string) bool {
	q := normalizeForQuoteMatch(quote)
	s := normalizeForQuoteMatch(source)
	if q == "" || s == "" {
		return false
	}
	return strings.Contains(s, q)
}

func normalizeForQuoteMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch r {
		case '­': // soft hyphen, inserted by PDF line wrapping
			continue
		case '’', 'ʼ', '‘': // typographic apostrophes
			r = '\''
		case '“', '”', '«', '»':
			r = '"'
		case '‐', '‑', '‒', '–', '—':
			r = '-'
		case ' ', ' ', ' ':
			r = ' '
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteRune(' ')
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Export-time revalidation (§6.4)
// ---------------------------------------------------------------------------

// This legacy pass compares against the stored immutable version, and
// RevalidationReport says so ("stored_snapshot") rather than implying an
// official round-trip happened: DILA is explicit that the opposable text is
// the authenticated Légifrance publication, not our cache. Agent citations
// are revalidated against the official source itself by the final gate
// (citation_sources.go).

// CitationDrift is one citation whose source no longer matches what was frozen.
type CitationDrift struct {
	CitationID string `json:"citation_id"`
	ClaimKey   string `json:"claim_key"`
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id"`
	Reason     string `json:"reason"`
}

// RevalidationReport is what the export path returns to the verification
// screen.
type RevalidationReport struct {
	Checked int             `json:"checked"`
	Matched int             `json:"matched"`
	Drifted []CitationDrift `json:"drifted"`
	// Against says what the citations were compared with: always
	// "stored_snapshot" here. A resolving URL is not correctness and neither
	// is a cache hit — say which one happened.
	Against string `json:"against"`
}

// RevalidateCitations rechecks every citation attached to a set of runs
// immediately before an export is published.
//
// It NEVER rewrites a quote and NEVER swaps a cited version for its current
// wording: the dispute may concern an earlier period, so a changed article is
// evidence to show the lawyer, not a correction to apply. A citation that
// still matches is promoted `retrieved` → `machine_checked`; one that does not
// is flagged `drifted`. `lawyer_verified` is never written here and never
// downgraded silently — it is re-flagged as drifted only when the source truly
// moved, because a human who verified stale bytes must be told.
func RevalidateCitations(ctx context.Context, repo *u.TenantRepo, runIDs []string) (RevalidationReport, error) {
	report := RevalidationReport{Against: "stored_snapshot"}
	if repo == nil || len(runIDs) == 0 {
		return report, nil
	}
	var rows []types.Citation
	// Agent citations (bound to an evidence span) are checked by the agent
	// gate (verification.go) and bound at publication; this pass covers the
	// legacy legal:/piece: rows only.
	if err := repo.Citations().Where("run_id IN ? AND evidence_span_id IS NULL", runIDs).Find(&rows).Error; err != nil {
		return report, err
	}
	if len(rows) == 0 {
		return report, nil
	}
	resolver := &dbSourceResolver{tenantID: repo.TenantID()}

	for _, cit := range rows {
		id, ok := citationSourceID(cit)
		if !ok {
			report.Drifted = append(report.Drifted, CitationDrift{
				CitationID: cit.ID, ClaimKey: cit.ClaimKey, SourceType: cit.SourceType,
				Reason: "citation sans source identifiable",
			})
			continue
		}
		report.Checked++

		rec, err := resolver.Resolve(ctx, cit.SourceType, id)
		if err != nil {
			report.Drifted = append(report.Drifted, CitationDrift{
				CitationID: cit.ID, ClaimKey: cit.ClaimKey, SourceType: cit.SourceType, SourceID: id,
				Reason: "source introuvable au moment de l'export",
			})
			markCitation(repo, cit, types.CitationDrifted)
			continue
		}

		switch {
		case cit.SourceSHA256 != "" && cit.SourceSHA256 != rec.SHA256:
			report.Drifted = append(report.Drifted, CitationDrift{
				CitationID: cit.ID, ClaimKey: cit.ClaimKey, SourceType: cit.SourceType, SourceID: id,
				Reason: "le texte de la source a changé depuis la rédaction",
			})
			markCitation(repo, cit, types.CitationDrifted)
		case !QuoteOccursIn(cit.Quote, rec.Text):
			report.Drifted = append(report.Drifted, CitationDrift{
				CitationID: cit.ID, ClaimKey: cit.ClaimKey, SourceType: cit.SourceType, SourceID: id,
				Reason: "l'extrait cité n'est plus présent dans la source",
			})
			markCitation(repo, cit, types.CitationDrifted)
		default:
			report.Matched++
			if cit.VerificationState == types.CitationRetrieved || cit.VerificationState == types.CitationDrifted {
				markCitation(repo, cit, types.CitationMachineChecked)
			}
		}
	}
	sort.Slice(report.Drifted, func(i, j int) bool { return report.Drifted[i].ClaimKey < report.Drifted[j].ClaimKey })

	citationsLog.Infof("Citations revalidated (runs=%d checked=%d matched=%d drifted=%d against=%s)",
		len(runIDs), report.Checked, report.Matched, len(report.Drifted), report.Against)
	return report, nil
}

func citationSourceID(c types.Citation) (string, bool) {
	switch c.SourceType {
	case types.CitationSourceLegal:
		if c.LegalVersionID != nil && *c.LegalVersionID != "" {
			return *c.LegalVersionID, true
		}
	case types.CitationSourcePiece:
		if c.PieceID != nil && *c.PieceID != "" {
			return *c.PieceID, true
		}
	}
	return "", false
}

func markCitation(repo *u.TenantRepo, cit types.Citation, state string) {
	if cit.VerificationState == state {
		return
	}
	if err := repo.Citations().Where("id = ?", cit.ID).
		Update("verification_state", state).Error; err != nil {
		citationsLog.Errorf("Citation state update failed (citation=%s state=%s): %v", cit.ID, state, err)
	}
}

// ---------------------------------------------------------------------------
// The database-backed resolver
// ---------------------------------------------------------------------------

// dbSourceResolver reads legal_versions (public, shared, no tenant column) and
// pieces (private — through the tenant repository, never a bare u.DB query).
type dbSourceResolver struct{ tenantID string }

func (r *dbSourceResolver) Resolve(ctx context.Context, sourceType, id string) (SourceRecord, error) {
	if u.DB == nil {
		return SourceRecord{}, u.ErrAuthNotConfigured
	}
	switch sourceType {
	case types.CitationSourceLegal:
		return r.resolveLegal(ctx, id)
	case types.CitationSourcePiece:
		return r.resolvePiece(ctx, id)
	default:
		return SourceRecord{}, ErrCitationMalformed
	}
}

func (r *dbSourceResolver) resolveLegal(ctx context.Context, id string) (SourceRecord, error) {
	var v types.LegalVersion
	if err := u.DB.WithContext(ctx).Where("id = ?", id).Take(&v).Error; err != nil {
		return SourceRecord{}, ErrSourceNotFound
	}
	var doc types.LegalDocument
	label := v.SourceVersion
	if err := u.DB.WithContext(ctx).Where("id = ?", v.LegalDocumentID).Take(&doc).Error; err == nil {
		label = firstNonEmpty(doc.CitationLabel, doc.CanonicalURL, label)
	}
	sum := v.TextSHA256
	if sum == "" {
		sum = sha256Hex(v.Text)
	}
	return SourceRecord{
		SourceType: types.CitationSourceLegal,
		ID:         v.ID,
		Label:      label,
		Text:       v.Text,
		SHA256:     sum,
		ValidFrom:  v.ValidFrom,
		ValidTo:    v.ValidTo,
	}, nil
}

// resolvePiece rebuilds the piece's text from its chunks, in ordinal order.
// Pieces carry no text column: the original bytes are never modified and the
// extracted text lives in piece_chunks.
func (r *dbSourceResolver) resolvePiece(ctx context.Context, id string) (SourceRecord, error) {
	if r.tenantID == "" {
		return SourceRecord{}, u.ErrNoTenant
	}
	repo := u.ForTenantID(ctx, r.tenantID)

	var p types.Piece
	if err := repo.Pieces().Where("id = ?", id).Take(&p).Error; err != nil {
		return SourceRecord{}, ErrSourceNotFound
	}
	var chunks []types.PieceChunk
	if err := repo.PieceChunks().Where("piece_id = ?", id).Order("ordinal ASC").Find(&chunks).Error; err != nil {
		return SourceRecord{}, err
	}
	if len(chunks) == 0 {
		// A piece with no extracted text cannot support a quote, and saying
		// "not found" here is what makes the refusal fire.
		return SourceRecord{}, ErrSourceNotFound
	}
	var b strings.Builder
	pageFrom, pageTo := 0, 0
	for i, ch := range chunks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(ch.Text)
		if pageFrom == 0 || (ch.PageFrom > 0 && ch.PageFrom < pageFrom) {
			pageFrom = ch.PageFrom
		}
		if ch.PageTo > pageTo {
			pageTo = ch.PageTo
		}
	}
	text := b.String()
	return SourceRecord{
		SourceType: types.CitationSourcePiece,
		ID:         p.ID,
		Label:      firstNonEmpty(p.FinalName, p.ProposedName, p.Title, p.OriginalName),
		Text:       text,
		SHA256:     sha256Hex(text),
		PageFrom:   pageFrom,
		PageTo:     pageTo,
	}, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Agent legal citations — the same refusal, extended
// ---------------------------------------------------------------------------
//
// An agent claim cites agent_evidence_spans, not "legal:<id>" references, but
// the rule above is unchanged and applies span by span: the span must be one
// this run retrieved, it must resolve to the exact stored version it names,
// its quote must occur in that version's text (minQuoteRunes, QuoteOccursIn),
// and the source hash must still match. On top of it the agent path checks
// what a dated dispute needs: the article version must be applicable to the
// statute period (half-open validity interval), and a decision must not
// postdate the case-law cutoff. Each failure is a BLOCKING issue — the
// document is refused, never rendered with the citation dropped.

// AgentLegalSource is the stored source an agent legal span resolved to.
type AgentLegalSource struct {
	Version  types.LegalVersion
	Document types.LegalDocument
}

// LegalTemporalBasis is what a legal span is checked against. Applicability
// is a half-open interval [From, To) in unix seconds, nil bounds open.
// CaseLawCutoff is the exclusive end of the cutoff day, month or year.
type LegalTemporalBasis struct {
	ApplicabilityFrom *int64
	ApplicabilityTo   *int64
	CaseLawCutoff     *int64
}

// LegalSpanIssue is one refusal of an agent legal span: an issue code and the
// server-written correction for the agent.
type LegalSpanIssue struct {
	Code       string
	Correction string
}

// CheckAgentLegalSpan resolves one legifrance/judilibre evidence span against
// its stored source and returns every refusal. src is nil when the span names
// no stored version, or one that no longer exists.
func CheckAgentLegalSpan(span types.AgentEvidenceSpan, src *AgentLegalSource, basis LegalTemporalBasis) []LegalSpanIssue {
	var out []LegalSpanIssue
	refuse := func(code, correction string) { out = append(out, LegalSpanIssue{Code: code, Correction: correction}) }

	if src == nil {
		refuse(types.IssueLegalUnresolved,
			"Cette source juridique ne résout vers aucune version enregistrée : relisez-la (read_law, read_decision) ou retirez la référence.")
		return out
	}
	isDecision := src.Document.Kind == legalKindDecision || src.Document.Source == types.SourceJudilibre
	if (span.SourceType == types.CitationSourceJudilibre) != isDecision {
		refuse(types.IssueLegalUnresolved,
			"Le type de la source citée ne correspond pas à la source enregistrée : citez le passage tel que l'outil l'a retourné.")
		return out
	}

	var loc types.SourceLocator
	_ = json.Unmarshal(span.Locator, &loc) // an unreadable locator fails the identity checks below
	switch span.SourceType {
	case types.CitationSourceLegifrance:
		if loc.Kind != types.LocatorLegislation ||
			(loc.VersionID != "" && loc.VersionID != src.Version.ID) ||
			(loc.ArticleID != "" && loc.ArticleID != src.Document.SourceID && loc.ArticleID != src.Version.SourceVersion) {
			refuse(types.IssueLegalUnresolved,
				"La localisation de l'article ne correspond pas à la version enregistrée : citez la version exacte retournée par read_law.")
		}
	case types.CitationSourceJudilibre:
		if loc.Kind != types.LocatorDecision ||
			(loc.DecisionID != "" && loc.DecisionID != src.Document.SourceID && loc.DecisionID != src.Version.ID) {
			refuse(types.IssueLegalUnresolved,
				"La localisation de la décision ne correspond pas à la décision enregistrée : citez le passage retourné par read_decision.")
		}
	}

	sum := src.Version.TextSHA256
	if sum == "" {
		sum = sha256Hex(src.Version.Text)
	}
	if span.SourceSHA256 != sum {
		refuse(types.IssueSourceMutated,
			"Le texte enregistré de cette source a changé depuis sa lecture : relisez-la avant de la citer.")
	}

	quote := strings.TrimSpace(span.Quote)
	if len([]rune(quote)) < minQuoteRunes || !QuoteOccursIn(quote, src.Version.Text) {
		refuse(types.IssueLegalQuoteNotFound,
			"Le passage cité n'apparaît pas dans le texte de la version enregistrée : citez un extrait exact d'au moins une phrase.")
	}

	if isDecision {
		if basis.CaseLawCutoff != nil {
			if src.Version.DecisionDate == nil {
				refuse(types.IssueCaselawAfterCutoff,
					"La date de cette décision est inconnue : elle ne peut pas être rattachée à la date limite de jurisprudence demandée.")
			} else if *src.Version.DecisionDate >= *basis.CaseLawCutoff {
				refuse(types.IssueCaselawAfterCutoff,
					"Cette décision est postérieure à la date limite de jurisprudence demandée : retirez-la ou cherchez une décision antérieure.")
			}
		}
		return out
	}

	if src.Version.State == types.LegalVersionAnnulled {
		refuse(types.IssueLegalVersionNotInForce,
			"Cette version a été annulée : elle n'est applicable à aucune période.")
	} else if !versionApplicable(src.Version, basis) {
		refuse(types.IssueLegalVersionNotInForce,
			"Cette version de l'article n'était pas en vigueur pendant la période applicable : citez la version applicable (search_law avec la date), sans la remplacer par le texte actuel.")
	}
	return out
}

// versionApplicable is the half-open validity test of §6.4,
// valid_from <= t AND (valid_to IS NULL OR t < valid_to), extended to a
// period: the version must be in force for at least part of it, so a dispute
// spanning an amendment can cite both versions. A version with no recorded
// start cannot be shown applicable — the retrieval filter treats it the same.
func versionApplicable(v types.LegalVersion, basis LegalTemporalBasis) bool {
	if v.ValidFrom == nil {
		return false
	}
	if basis.ApplicabilityTo != nil && *v.ValidFrom >= *basis.ApplicabilityTo {
		return false
	}
	if v.ValidTo != nil && basis.ApplicabilityFrom != nil && *basis.ApplicabilityFrom >= *v.ValidTo {
		return false
	}
	return true
}

// LegalReference is a legal reference written in prose — an article, a
// numbered text, a decision — found by FindLegalReferences. Start/End are
// code-point offsets in the scanned text; Key is the normalised identity
// matched against the cited sources (LegalSourceKeys).
type LegalReference struct {
	Kind    string // article | text | decision
	Key     string
	Display string
	Start   int
	End     int
}

var (
	reRefCodeArticle = regexp.MustCompile(`\b([lrd])\.?\s?(\d{2,4}(?:-\d+)+)\b`)
	reRefNumArticle  = regexp.MustCompile(`\bart(?:icle)?s?\.?\s+(\d{1,4}(?:-\d+)*)(?:\s+(?:du|de la|de l'))?\s+(?:code|c\.\s?(?:civ|trav|com|pen|proc|consom|cons|ass|secu|env|urb)|cpc|cpp|cgi|constitution)`)
	reRefText        = regexp.MustCompile(`\b(loi|decret|ordonnance|arrete)\s+n\s?[°o]\.?\s*(\d{2,4}-\d{1,5})`)
	reRefPourvoi     = regexp.MustCompile(`\bn\s?[°o]\.?\s*(?:[a-z]\s)?(\d{2}-\d{2}\.\d{3})`)
	reRefRG          = regexp.MustCompile(`\brg\s*n?\s?[°o]?\.?\s*(\d{2}/\d{3,5})`)
	reRefECLI        = regexp.MustCompile(`\becli:[a-z]{2}:[a-z]+:\d{4}:[a-z0-9.]+`)
	reRefCassDate    = regexp.MustCompile(`\b(?:cass\.?|cour de cassation)[^;\n]{0,40}?\b(1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(\d{4})`)
	reLabelArticle   = regexp.MustCompile(`\bart\.?\s*([lrd]\.?\s?)?(\d{1,4}(?:-\d+)*)`)

	// Decisions cited the way lawyers write them, each ending on its date
	// (the last three groups: day, month, year). A chamber without « Cass. »:
	// « Soc., 25 septembre 2024 », « Civ. 1re, 3 mai 2018 ».
	reRefChamberDate = regexp.MustCompile(`\b(?:soc|com|crim|civ\.?\s*(?:1re|1ere|2e|2eme|3e|3eme|[123])|ass\.?\s*plen|ch\.?\s*mixte|chambre\s+(?:sociale|commerciale|criminelle|civile|mixte))\.?\s*,?\s*(?:du\s+)?` + refDatePattern)
	// « CA Paris, 12 mars 2023 », « cour d'appel de Lyon, 3 mai 2021 ».
	// « CA » must be upper case in the original: folded, « ça » reads « ca ».
	reRefAppealDate = regexp.MustCompile(`\b(?:ca\s+[a-z][a-z\-' ]{1,30}?|cour d'appel d(?:e\s+|')[a-z][a-z\-' ]{1,30}?)\s*,\s*(?:[^,;\n\d]{1,20},\s*)?(?:du\s+)?` + refDatePattern)
	// « CE, 10 mars 2020 », « CE, ass., 10 mars 2020 », « Conseil d'État, … »;
	// « CE » upper case in the original (folded, the pronoun « ce »).
	reRefCEDate = regexp.MustCompile(`\b(?:ce|conseil d'etat)\s*,\s*(?:[^,;\n\d]{1,30},\s*)?(?:du\s+)?` + refDatePattern)
	// European courts: « CJUE, 9 nov. 2023 », « CEDH, 5 sept. 2017 ».
	reRefEuropeDate = regexp.MustCompile(`\b(?:cjue|cjce|cedh|cour edh|cour europeenne des droits de l'homme|cour de justice de l'union europeenne)\s*,?\s*(?:[^,;\n\d]{1,30},\s*)?(?:du\s+)?` + refDatePattern)
	// « Cons. const., déc. n° 2018-761 QPC » or with its date.
	reRefConstNum  = regexp.MustCompile(`\b(?:cons\.?\s*const\.?|conseil constitutionnel)\s*,?\s*(?:dec(?:ision)?\.?\s*)?n\s?[°o]\.?\s*(\d{2,4}-\d{1,4})`)
	reRefConstDate = regexp.MustCompile(`\b(?:cons\.?\s*const\.?|conseil constitutionnel)\s*,?\s*(?:dec(?:ision)?\.?\s*,?\s*)?(?:du\s+)?` + refDatePattern)
	// A text identified by its date: « loi du 6 juillet 1989 », « loi n°
	// 89-462 du 6 juillet 1989 », « décret du … ».
	reRefTextDate = regexp.MustCompile(`\b(loi|decret)(?:\s+n\s?[°o]\.?\s*\d{2,4}-\d{1,5})?\s+du\s+` + refDatePattern)
	// An article of a text other than a code: « article 9 de la loi du … »,
	// « article 8 de la CEDH », « article 6 de la Convention européenne ».
	reRefTextArticle = regexp.MustCompile(`\bart(?:icle)?s?\.?\s+(\d{1,4}(?:-\d+)*)(?:\s*(?:§|al\.?|alinea)\s*\d+)?\s+(?:de la|du|de l')\s*(?:loi|decret|ordonnance|cedh|convention europeenne|convention de sauvegarde|traite|tfue|charte|declaration des droits|ddhc|directive)\b`)
	// An article by anaphora: « l'article 1104 du même code », « dudit code ».
	reRefSameCode = regexp.MustCompile(`\bart(?:icle)?s?\.?\s+([lrd]\.?\s?)?(\d{1,4}(?:-\d+)*)\s+(?:du meme code|dudit code|du code precite|du code susvise|de ce code)`)
)

// refDatePattern is a cited decision's or text's date: day, month, year.
const refDatePattern = `(1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(\d{4})`

// refDateKey reads the date groups that end a match (day, month, year).
func refDateKey(t string, m []int) (string, bool) {
	n := len(m)
	dayS, monS, yearS := t[m[n-6]:m[n-5]], t[m[n-4]:m[n-3]], t[m[n-2]:m[n-1]]
	d := 1
	if dayS != "1er" {
		d, _ = strconv.Atoi(dayS)
	}
	y, _ := strconv.Atoi(yearS)
	mo := frenchMonthIndex(monS)
	if !validDay(y, mo, d) {
		return "", false
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, mo, d), true
}

// FindLegalReferences scans prose for legal-reference forms, so a citation
// printed as ordinary text cannot escape the checks because the model left
// out a citation field. A contract clause ("l'article 5 du contrat") is not a
// legal reference and is not matched.
func FindLegalReferences(text string) []LegalReference {
	t := foldForMatch(text)
	var out []LegalReference
	add := func(kind, key string, start, end int) {
		s, e := runeOffset(t, start), runeOffset(t, end)
		out = append(out, LegalReference{Kind: kind, Key: key, Display: substrRunes(text, s, e), Start: s, End: e})
	}
	for _, m := range reRefCodeArticle.FindAllStringSubmatchIndex(t, -1) {
		add("article", articleKey(t[m[2]:m[3]], t[m[4]:m[5]]), m[0], m[1])
	}
	for _, m := range reRefNumArticle.FindAllStringSubmatchIndex(t, -1) {
		add("article", articleKey("", t[m[2]:m[3]]), m[0], m[1])
	}
	for _, m := range reRefText.FindAllStringSubmatchIndex(t, -1) {
		add("text", t[m[4]:m[5]], m[0], m[1])
	}
	for _, m := range reRefPourvoi.FindAllStringSubmatchIndex(t, -1) {
		add("decision", digitsOnly(t[m[2]:m[3]]), m[0], m[1])
	}
	for _, m := range reRefRG.FindAllStringSubmatchIndex(t, -1) {
		add("decision", digitsOnly(t[m[2]:m[3]]), m[0], m[1])
	}
	for _, m := range reRefECLI.FindAllStringIndex(t, -1) {
		add("decision", strings.ToUpper(t[m[0]:m[1]]), m[0], m[1])
	}
	for _, m := range reRefCassDate.FindAllStringSubmatchIndex(t, -1) {
		d := 1
		if t[m[2]:m[3]] != "1er" {
			d, _ = strconv.Atoi(t[m[2]:m[3]])
		}
		y, _ := strconv.Atoi(t[m[6]:m[7]])
		add("decision", fmt.Sprintf("date:%04d-%02d-%02d", y, frenchMonthIndex(t[m[4]:m[5]]), d), m[0], m[1])
	}
	// upper reports whether the original text at a match starts with the
	// given upper-case abbreviation (« CA », « CE »).
	upper := func(start int, abbr string) bool {
		s := runeOffset(t, start)
		return substrRunes(text, s, s+len(abbr)) == abbr
	}
	for _, spec := range []struct {
		re   *regexp.Regexp
		abbr string // the upper-case form a two-letter court must have
	}{
		{reRefChamberDate, ""}, {reRefAppealDate, "CA"}, {reRefCEDate, "CE"},
		{reRefEuropeDate, ""}, {reRefConstDate, ""},
	} {
		for _, m := range spec.re.FindAllStringSubmatchIndex(t, -1) {
			if spec.abbr != "" && len(t[m[0]:]) > 2 && t[m[0]:m[0]+2] == strings.ToLower(spec.abbr) && !upper(m[0], spec.abbr) {
				continue
			}
			if key, ok := refDateKey(t, m); ok {
				add("decision", "date:"+key, m[0], m[1])
			}
		}
	}
	for _, m := range reRefConstNum.FindAllStringSubmatchIndex(t, -1) {
		add("decision", digitsOnly(t[m[2]:m[3]]), m[0], m[1])
	}
	for _, m := range reRefTextDate.FindAllStringSubmatchIndex(t, -1) {
		if key, ok := refDateKey(t, m); ok {
			add("text", t[m[2]:m[3]]+":"+key, m[0], m[1])
		}
	}
	for _, m := range reRefTextArticle.FindAllStringSubmatchIndex(t, -1) {
		add("article", articleKey("", t[m[2]:m[3]]), m[0], m[1])
	}
	for _, m := range reRefSameCode.FindAllStringSubmatchIndex(t, -1) {
		prefix := ""
		if m[2] >= 0 {
			prefix = t[m[2]:m[3]]
		}
		add("article", articleKey(prefix, t[m[4]:m[5]]), m[0], m[1])
	}
	return dedupeLegalReferences(out)
}

// dedupeLegalReferences drops a reference that overlaps an earlier one with
// the same key — « Cass. soc., 25 septembre 2024 » is found by two forms.
func dedupeLegalReferences(refs []LegalReference) []LegalReference {
	out := refs[:0]
	for _, r := range refs {
		dup := false
		for _, kept := range out {
			if kept.Key == r.Key && r.Start < kept.End && kept.Start < r.End {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, r)
		}
	}
	return out
}

// LegalSourceKeys is the set of reference keys a stored legal source answers
// to: its article number (citation label and source metadata), its pourvoi
// and RG numbers, its ECLI and its decision date.
func LegalSourceKeys(src AgentLegalSource) map[string]bool {
	keys := map[string]bool{}
	for _, m := range reLabelArticle.FindAllStringSubmatch(foldForMatch(src.Document.CitationLabel), -1) {
		keys[articleKey(m[1], m[2])] = true
	}
	for _, r := range FindLegalReferences(src.Document.CitationLabel) {
		keys[r.Key] = true
	}
	var meta map[string]any
	if len(src.Version.SourceMeta) > 0 && json.Unmarshal(src.Version.SourceMeta, &meta) == nil {
		if num, ok := meta["article_number"].(string); ok && num != "" {
			keys[articleKey("", num)] = true
		}
		if nums, ok := meta["numbers"].([]any); ok {
			for _, n := range nums {
				if s, ok := n.(string); ok && digitsOnly(s) != "" {
					keys[digitsOnly(s)] = true
				}
			}
		}
	}
	if src.Version.ECLI != "" {
		keys[strings.ToUpper(src.Version.ECLI)] = true
	}
	if src.Version.DecisionDate != nil {
		keys["date:"+time.Unix(*src.Version.DecisionDate, 0).UTC().Format("2006-01-02")] = true
	}
	return keys
}

// articleKey normalises "L. 1234-1", "l1234-1" and "1240" into "L1234-1" and
// "1240".
func articleKey(prefix, num string) string {
	p := strings.ToUpper(strings.NewReplacer(" ", "", ".", "").Replace(prefix))
	return p + strings.ToUpper(strings.NewReplacer(" ", "", ".", "").Replace(num))
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// runeOffset converts a byte offset in s into a code-point offset.
func runeOffset(s string, byteOff int) int {
	return utf8.RuneCountInString(s[:byteOff])
}

func substrRunes(s string, start, end int) string {
	r := []rune(s)
	start = max(start, 0)
	end = min(end, len(r))
	if start >= end {
		return ""
	}
	return string(r[start:end])
}

// LocateQuote finds quote in source under the normalisation QuoteOccursIn
// uses and returns its half-open code-point range in the ORIGINAL source, so
// a passage can be highlighted exactly where it stands.
func LocateQuote(source, quote string) (start, end int, ok bool) {
	normSrc, orig := normalizeWithOffsets(source)
	normQ, _ := normalizeWithOffsets(quote)
	if len(normQ) == 0 || len(normSrc) == 0 {
		return 0, 0, false
	}
	hay := string(normSrc)
	idx := strings.Index(hay, string(normQ))
	if idx < 0 {
		return 0, 0, false
	}
	first := utf8.RuneCountInString(hay[:idx])
	return orig[first], orig[first+len(normQ)-1] + 1, true
}

// normalizeWithOffsets is normalizeForQuoteMatch that also keeps, for every
// output rune, the code-point index of the source rune it came from.
func normalizeWithOffsets(s string) ([]rune, []int) {
	out := make([]rune, 0, len(s))
	orig := make([]int, 0, len(s))
	space, spaceAt, i := false, 0, -1
	for _, r := range s {
		i++
		switch r {
		case '­':
			continue
		case '’', 'ʼ', '‘':
			r = '\''
		case '“', '”', '«', '»':
			r = '"'
		case '‐', '‑', '‒', '–', '—':
			r = '-'
		case ' ', ' ', ' ':
			r = ' '
		}
		if unicode.IsSpace(r) {
			if !space {
				spaceAt = i
			}
			space = true
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
			orig = append(orig, spaceAt)
		}
		space = false
		out = append(out, unicode.ToLower(r))
		orig = append(orig, i)
	}
	return out, orig
}
