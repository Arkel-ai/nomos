package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var researchToolsLog = u.NewLogger("services.nomos_tools_research")

// Public research and compétence tools.
//
// Every query that leaves the cabinet (Légifrance, Judilibre, the web) goes
// through CheckPublicQuery first. Search results are CANDIDATES: identity,
// metadata and a short excerpt. Only read_law and read_decision return the
// exact text as evidence spans a claim may cite, each pinned to a stored,
// immutable legal_versions row. An unavailable source is reported as such in
// the observation, never as an empty result.

// Research bounds.
const (
	lawWindowRunes      = 16000
	decisionWindowRunes = 14000
	searchExcerptRunes  = 400
	maxSearchPage       = 10
	searchPageSize      = 10
	corpusSearchTopK    = 8
	// webSearchMaxResults / webSearchMaxChars are the explicit result and
	// character limits of the isolated web request (§6.4).
	webSearchMaxResults = 5
	webSearchMaxChars   = 3000
	webSummaryRunes     = 1500
	webRequestTimeout   = 120 * time.Second
)

var (
	reLegiArticleID = regexp.MustCompile(`^LEGIARTI\d{12}$`)
	reJudilibreID   = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	reISODay        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// ---------------------------------------------------------------------------
// Connectors
// ---------------------------------------------------------------------------

var (
	pisteOnce       sync.Once
	agentLegifrance *connectors.Legifrance
	agentJudilibre  *connectors.Judilibre
)

// pisteClients are built once per process so the OAuth token is reused
// across calls.
func pisteClients() (*connectors.Legifrance, *connectors.Judilibre) {
	pisteOnce.Do(func() {
		cfg := config.Load()
		agentLegifrance = connectors.NewLegifrance(pisteOptions(cfg, cfg.LegifranceBaseURL))
		agentJudilibre = connectors.NewJudilibre(pisteOptions(cfg, cfg.JudilibreBaseURL))
	})
	return agentLegifrance, agentJudilibre
}

func legifranceReady() bool { lf, _ := pisteClients(); return lf != nil && lf.Configured() }
func judilibreReady() bool  { _, jl := pisteClients(); return jl != nil && jl.Configured() }

// corpusReady: the stored corpus needs the database and the embeddings
// gateway for its vector arm.
func corpusReady() bool { return u.DB != nil && u.LLMConfigured() }

// sourceStatus is how each source fared, so "nothing found" and "not asked"
// can never be confused.
type sourceStatus struct {
	Source  string `json:"source"`
	Status  string `json:"status"` // ok | empty | unavailable | error
	Results int    `json:"results"`
	Detail  string `json:"detail,omitempty"`
}

func connectorFailure(err error) (detail string, retryable bool) {
	var api *connectors.APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("erreur %d de la source", api.Status), api.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "délai dépassé", true
	}
	return "source injoignable", true
}

// ---------------------------------------------------------------------------
// Dates
// ---------------------------------------------------------------------------

// parseISODay reads YYYY-MM-DD as a UTC day.
func parseISODay(s string) (time.Time, error) {
	if !reISODay.MatchString(s) {
		return time.Time{}, badArgs("date attendue au format AAAA-MM-JJ")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, badArgs("date invalide : %s", s)
	}
	return t.UTC(), nil
}

// partialDateBounds is the first and last day a partial date can denote.
func partialDateBounds(d *types.PartialDate) (from, to time.Time, ok bool) {
	if d == nil || d.Year == 0 {
		return time.Time{}, time.Time{}, false
	}
	switch d.Precision {
	case types.DatePrecisionDay:
		t := time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC)
		return t, t, true
	case types.DatePrecisionMonth:
		t := time.Date(d.Year, time.Month(d.Month), 1, 0, 0, 0, 0, time.UTC)
		return t, t.AddDate(0, 1, -1), true
	case types.DatePrecisionYear:
		return time.Date(d.Year, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(d.Year, 12, 31, 0, 0, 0, 0, time.UTC), true
	}
	return time.Time{}, time.Time{}, false
}

// applicabilityDate resolves the statute applicability date: the argument,
// else the contract's day-precise boundary. There is no default to today: a
// version read "as of today" for older facts would then pass the very check
// it should fail, so the date of the facts is always stated.
func applicabilityDate(arg *string, c types.AgentTaskContract) (time.Time, string, error) {
	if s := str(arg); s != "" {
		t, err := parseISODay(s)
		return t, "argument", err
	}
	if c.StatuteApplicability != nil {
		for _, d := range []*types.PartialDate{c.StatuteApplicability.From, c.StatuteApplicability.To} {
			if d != nil && d.Precision == types.DatePrecisionDay {
				from, _, ok := partialDateBounds(d)
				if ok {
					return from, "contract", nil
				}
			}
		}
	}
	return time.Time{}, "", badArgs("applicability_date est obligatoire : la date des faits en cause (AAAA-MM-JJ), " +
		"ou la date du jour seulement pour une question sur le droit actuel")
}

// caselawCutoff resolves the case-law cutoff: the argument, else the
// contract's (its last possible day), else none.
func caselawCutoff(arg *string, c types.AgentTaskContract) (*time.Time, error) {
	if s := str(arg); s != "" {
		t, err := parseISODay(s)
		if err != nil {
			return nil, err
		}
		return &t, nil
	}
	if _, to, ok := partialDateBounds(c.CaseLawCutoff); ok {
		return &to, nil
	}
	return nil, nil
}

func isoDay(sec *int64) string {
	if sec == nil {
		return ""
	}
	return time.Unix(*sec, 0).UTC().Format("2006-01-02")
}

// inForceAt applies the half-open validity interval.
func inForceAt(v types.LegalVersion, at time.Time) bool {
	t := at.Unix()
	if v.ValidFrom != nil && *v.ValidFrom > t {
		return false
	}
	if v.ValidTo != nil && t >= *v.ValidTo {
		return false
	}
	return true
}

// historicalStates widens the state filter for a past applicability date: an
// article "modifié" today was the text in force then, and the half-open
// interval does the selection.
func historicalStates(at time.Time) []string {
	if time.Since(at) < 24*time.Hour {
		return nil
	}
	return []string{types.LegalVersionInForce, types.LegalVersionModified, types.LegalVersionRepealed}
}

// gatePublicQuery runs the confidentiality gate and turns a rejection into
// its observation. ok=false means the outcome is final.
func gatePublicQuery(ctx context.Context, x ToolExecution, tool, query string, act types.AgentActivityObject) (ToolOutcome, bool, error) {
	lawyer, err := publicQueryLawyerText(ctx, x)
	if err != nil {
		return ToolOutcome{}, false, fmt.Errorf("public query gate: %w", err)
	}
	verdict, err := CheckPublicQuery(ctx, x.Repo(ctx), x.Contract.Scope.DossierID, query, lawyer...)
	if err != nil {
		// The deny-list could not be built: the query must not leave.
		return ToolOutcome{}, false, fmt.Errorf("public query gate: %w", err)
	}
	if verdict.Safe() {
		return ToolOutcome{}, true, nil
	}
	researchToolsLog.Infof("Public query refused (tool=%s run=%s call=%s categories=%s)",
		tool, x.RunID, x.CallID, strings.Join(sortedCategories(verdict), ","))
	return failure(x, tool, types.ToolCodeUnsafePublicQuery, false,
		"Cette requête ne peut pas quitter le cabinet : elle contient des éléments propres au dossier. "+
			"Reformulez-la en termes juridiques généraux, sans nom, date, montant, coordonnée ni extrait de pièce. "+
			"Le contrôle de confidentialité ne peut pas être levé.",
		map[string]any{"category": sortedCategories(verdict)[0], "categories": sortedCategories(verdict), "matched": verdict.Matched},
		act), false, nil
}

// publicQueryLawyerText is what the lawyer typed for this turn — the
// instruction, the thread's messages, the run's answers — whose names and
// dates must not leave the cabinet either.
func publicQueryLawyerText(ctx context.Context, x ToolExecution) ([]string, error) {
	repo := x.Repo(ctx)
	texts := []string{x.Contract.Instruction}
	if x.ConversationID != nil {
		var msgs []string
		if err := repo.Messages().Where("conversation_id = ? AND role = ?", *x.ConversationID, types.MessageRoleUser).
			Order("created_at DESC").Limit(200).Pluck("content", &msgs).Error; err != nil {
			return nil, err
		}
		texts = append(texts, msgs...)
	}
	var answers []types.RunQuestion
	if err := repo.RunQuestions().Where("run_id = ? AND state = ?", x.RunID, types.QuestionStateAnswered).
		Find(&answers).Error; err != nil {
		return nil, err
	}
	for _, q := range answers {
		texts = append(texts, jsonStrings(q.Answer)...)
	}
	return texts, nil
}

// reCodeName is the shape of search_law's code filter: the name of a code,
// nothing else.
var reCodeName = regexp.MustCompile(`(?i)^code\s+[\p{L}' \-]{2,80}$`)

// publicQueryArg validates the shape of a public query argument.
func publicQueryArg(q string) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", badArgs("query est obligatoire")
	}
	if utf8.RuneCountInString(q) > types.MaxWebQueryRunes {
		return "", badArgs("query dépasse %d caractères", types.MaxWebQueryRunes)
	}
	if strings.ContainsAny(q, "\r\n") {
		return "", badArgs("query doit tenir sur une ligne")
	}
	return q, nil
}

func pageArg(p *int) (int, error) {
	if p == nil {
		return 1, nil
	}
	if *p < 1 || *p > maxSearchPage {
		return 0, badArgs("page doit être entre 1 et %d", maxSearchPage)
	}
	return *p, nil
}

// ---------------------------------------------------------------------------
// Legal versions: the stored, immutable text a citation rests on
// ---------------------------------------------------------------------------

// storeLegalVersion makes a fetched official text durable without ever
// rewriting a stored one: the same identity and hash converges on the
// existing row; different text under the same identity becomes a separate
// version whose official identifier stays in the metadata. Public tables,
// shared across tenants.
//
// A version this call inserted is indexed like the corpus loader indexes it
// (retrieval header, chunks, embeddings), so an on-demand read and a sync
// leave the same rows behind and later searches can find it.
func storeLegalVersion(ctx context.Context, doc types.LegalDocument, v types.LegalVersion, sectionPath []string) (types.LegalVersion, error) {
	db := u.DB.WithContext(ctx)
	var existing types.LegalVersion
	err := db.Where("legal_document_id = ? AND source_version = ?", doc.ID, v.SourceVersion).Take(&existing).Error
	switch {
	case err == nil && existing.TextSHA256 == v.TextSHA256:
		return existing, nil
	case err == nil:
		v.SourceVersion = v.SourceVersion + "@" + v.TextSHA256[:16]
		v.ID = derivedID(doc.ID, v.SourceVersion)
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return types.LegalVersion{}, err
	}
	if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "source"}, {Name: "source_id"}}, DoNothing: true}).
		Create(&doc).Error; err != nil {
		return types.LegalVersion{}, err
	}
	res := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "legal_document_id"}, {Name: "source_version"}}, DoNothing: true}).
		Create(&v)
	if res.Error != nil {
		return types.LegalVersion{}, res.Error
	}
	var stored types.LegalVersion
	if err := db.Where("legal_document_id = ? AND source_version = ?", doc.ID, v.SourceVersion).Take(&stored).Error; err != nil {
		return types.LegalVersion{}, err
	}
	if stored.TextSHA256 != v.TextSHA256 {
		return types.LegalVersion{}, fmt.Errorf("legal version %s: stored text differs from the fetched text", stored.ID)
	}
	if res.RowsAffected == 1 {
		indexLegalVersion(ctx, doc, stored, sectionPath)
	}
	return stored, nil
}

// indexLegalVersion chunks and embeds a newly stored version. A failure only
// costs findability — the version itself is stored and citable — so it is
// logged, not returned.
func indexLegalVersion(ctx context.Context, doc types.LegalDocument, v types.LegalVersion, sectionPath []string) {
	var pending []pendingChunk
	for i, chunk := range chunkText(retrievalTextFor(doc.CitationLabel, sectionPath, v.Text)) {
		pending = append(pending, pendingChunk{versionID: v.ID, ordinal: i, text: chunk})
	}
	embedded := embedChunks(ctx, pending)
	if err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return upsertChunks(tx, pending) }); err != nil {
		researchToolsLog.Warnf("On-demand legal version not indexed (version=%s chunks=%d): %v", v.ID, len(pending), err)
		return
	}
	researchToolsLog.Debugf("On-demand legal version indexed (version=%s chunks=%d embedded=%d)", v.ID, len(pending), embedded)
}

// codeAbbreviations are the usual citation forms.
var codeAbbreviations = map[string]string{
	"code du travail":                         "C. trav.",
	"code civil":                              "C. civ.",
	"code de commerce":                        "C. com.",
	"code pénal":                              "C. pén.",
	"code de procédure civile":                "C. proc. civ.",
	"code de procédure pénale":                "C. proc. pén.",
	"code de la sécurité sociale":             "CSS",
	"code de la consommation":                 "C. consom.",
	"code général des impôts":                 "CGI",
	"code de l'organisation judiciaire":       "COJ",
	"code des procédures civiles d'exécution": "CPCE",
}

func articleLabel(textTitle, num string) string {
	num = strings.TrimSpace(num)
	title := strings.TrimSpace(textTitle)
	if abbrev, ok := codeAbbreviations[strings.ToLower(title)]; ok {
		return abbrev + ", art. " + num
	}
	if title == "" {
		return "art. " + num
	}
	return title + ", art. " + num
}

// storeLegifranceArticle converts a fetched article into its stored version.
func storeLegifranceArticle(ctx context.Context, a connectors.Article) (types.LegalVersion, types.LegalDocument, error) {
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return types.LegalVersion{}, types.LegalDocument{}, errEmptyOfficialText
	}
	cid := firstNonEmpty(a.CID, a.ID)
	var textTitle string
	if len(a.Context.TitreTxt) > 0 {
		textTitle = a.Context.TitreTxt[0].Titre
	}
	doc := types.LegalDocument{
		ID:            derivedID(types.SourceLegifrance, cid),
		Source:        types.SourceLegifrance,
		SourceID:      cid,
		Kind:          legalKindArticle,
		Jurisdiction:  "fr",
		CanonicalURL:  connectors.CanonicalURL(a.ID),
		CitationLabel: articleLabel(textTitle, a.Num),
		LicenseCode:   connectors.LicenceOuverte2,
	}
	validFrom, validTo := a.Validity()
	meta, _ := json.Marshal(map[string]any{
		"article_number": a.Num,
		"text_title":     textTitle,
		"official_id":    a.ID,
		"license":        connectors.LicenceOuverte2,
		"attribution":    "Légifrance — Direction de l'information légale et administrative (DILA)",
		"source_url":     connectors.CanonicalURL(a.ID),
		"retrieved_by":   "agent",
	})
	v := types.LegalVersion{
		ID:              derivedID(doc.ID, a.ID),
		LegalDocumentID: doc.ID,
		SourceVersion:   a.ID,
		Text:            text,
		TextSHA256:      sha256Hex(text),
		ValidFrom:       validFrom,
		ValidTo:         validTo,
		State:           types.NormalizeLegalVersionState(a.Etat),
		SourceMeta:      types.JSONB(meta),
	}
	stored, err := storeLegalVersion(ctx, doc, v, nil)
	return stored, doc, err
}

// storeJudilibreDecision converts a fetched decision exactly as the corpus
// loader does, so an on-demand read and a sync converge on one row.
func storeJudilibreDecision(ctx context.Context, d connectors.Decision) (types.LegalVersion, types.LegalDocument, error) {
	text := strings.TrimSpace(d.Text)
	if text == "" {
		return types.LegalVersion{}, types.LegalDocument{}, errEmptyOfficialText
	}
	doc := types.LegalDocument{
		ID:            derivedID(types.SourceJudilibre, d.ID),
		Source:        types.SourceJudilibre,
		SourceID:      d.ID,
		Kind:          legalKindDecision,
		Jurisdiction:  decisionJurisdiction(d),
		CanonicalURL:  connectors.DecisionURL(d.ID),
		CitationLabel: decisionCitationLabel(d),
		LicenseCode:   connectors.LicenceOuverte2,
	}
	v := types.LegalVersion{
		ID:               derivedID(doc.ID, d.SourceRevision()),
		LegalDocumentID:  doc.ID,
		SourceVersion:    d.SourceRevision(),
		Text:             text,
		TextSHA256:       sha256Hex(text),
		DecisionDate:     d.DecidedAt(),
		PublicationDate:  d.PublishedAt(),
		Formation:        d.Formation,
		PublicationLevel: d.PublicationLevel(),
		Solution:         firstNonEmpty(d.Solution, d.SolutionAlt),
		ECLI:             d.ECLI,
		State:            types.LegalVersionInForce,
		SourceMeta:       judilibreMeta(d),
	}
	stored, err := storeLegalVersion(ctx, doc, v, decisionRetrievalPath(d))
	return stored, doc, err
}

var errEmptyOfficialText = errors.New("the official source returned no text")

// loadLegalVersion reads a stored version and its document.
func loadLegalVersion(ctx context.Context, id string) (types.LegalVersion, types.LegalDocument, error) {
	var v types.LegalVersion
	if err := u.DB.WithContext(ctx).Where("id = ?", id).Take(&v).Error; err != nil {
		return v, types.LegalDocument{}, err
	}
	var d types.LegalDocument
	err := u.DB.WithContext(ctx).Where("id = ?", v.LegalDocumentID).Take(&d).Error
	return v, d, err
}

// officialArticleID is the LEGIARTI identity of a stored article version.
func officialArticleID(v types.LegalVersion) string {
	for _, cand := range []string{strings.SplitN(v.SourceVersion, "@", 2)[0], metaString(v.SourceMeta, "official_id")} {
		if reLegiArticleID.MatchString(cand) {
			return cand
		}
	}
	return ""
}

func metaString(raw types.JSONB, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// ---------------------------------------------------------------------------
// search_law
// ---------------------------------------------------------------------------

type searchLawArgs struct {
	types.ToolArgs
	Query             string  `json:"query"`
	ApplicabilityDate *string `json:"applicability_date"`
	TextType          *string `json:"text_type"`
	Code              *string `json:"code"`
	Page              *int    `json:"page"`
}

type lawCandidate struct {
	Source              string `json:"source"`
	LegalVersionID      string `json:"legal_version_id,omitempty"`
	LegifranceArticleID string `json:"legifrance_article_id,omitempty"`
	Label               string `json:"label"`
	TextTitle           string `json:"text_title,omitempty"`
	State               string `json:"state,omitempty"`
	ValidFrom           string `json:"valid_from,omitempty"`
	ValidTo             string `json:"valid_to,omitempty"`
	Excerpt             string `json:"excerpt,omitempty"`
}

func searchLawTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolSearchLaw, types.ToolClassExternal, true, false,
			"Recherche des articles de loi ou de code (corpus du cabinet et Légifrance) applicables à une date donnée. "+
				"Renvoie des candidats avec leur identité de version ; lisez-les avec read_law avant de les citer. "+
				"La requête quitte le cabinet : termes juridiques généraux uniquement, sans élément du dossier.",
			map[string]any{
				"query":              strSchema("Requête juridique générale, une ligne, 200 caractères au plus."),
				"applicability_date": nullable(strSchema("Date d'applicabilité du texte (AAAA-MM-JJ) : celle des faits en cause, ou celle du jour pour le droit actuel. Obligatoire, sauf si le contrat de tâche la fixe.")),
				"text_type":          nullable(enumSchema("Codes (défaut) ou lois et décrets.", "code", "loi_decret")),
				"code":               nullable(strSchema("Nom exact d'un code pour filtrer, ex. « Code du travail ».")),
				"page":               nullable(intSchema("Page de résultats Légifrance, 1 à 10.")),
			}),
		available: func() bool { return corpusReady() || legifranceReady() },
		exec:      execSearchLaw,
	}
}

func execSearchLaw(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectQuery, Label: "Recherche Légifrance"}
	var a searchLawArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolSearchLaw, err, act)
	}
	query, err := publicQueryArg(a.Query)
	if err != nil {
		return invalid(x, types.ToolSearchLaw, err, act)
	}
	act.Label = query
	at, basis, err := applicabilityDate(a.ApplicabilityDate, x.Contract)
	if err != nil {
		return invalid(x, types.ToolSearchLaw, err, act)
	}
	page, err := pageArg(a.Page)
	if err != nil {
		return invalid(x, types.ToolSearchLaw, err, act)
	}
	fond := "CODE_DATE"
	if str(a.TextType) == "loi_decret" {
		fond = "LODA_DATE"
	}
	act.Detail = "Légifrance · applicable au " + at.Format("02/01/2006")
	if out, ok, err := gatePublicQuery(ctx, x, types.ToolSearchLaw, query, act); !ok {
		return out, err
	}
	// The code filter leaves the cabinet with the query: a code's name, and
	// through the same gate.
	if code := str(a.Code); code != "" {
		if !reCodeName.MatchString(code) {
			return invalid(x, types.ToolSearchLaw, badArgs("code doit être le nom d'un code, ex. « Code du travail »"), act)
		}
		if out, ok, err := gatePublicQuery(ctx, x, types.ToolSearchLaw, code, act); !ok {
			return out, err
		}
	}

	var candidates []lawCandidate
	var statuses []sourceStatus
	retryable := false

	if corpusReady() && page == 1 {
		st := sourceStatus{Source: "corpus"}
		opts := DefaultRetrievalOptions()
		opts.TopK = corpusSearchTopK
		opts.Kinds = []string{legalKindArticle}
		opts.AsOfDate = at.Unix()
		opts.States = historicalStates(at)
		res, err := Retrieve(u.WithCallRef(ctx, callRef(x)), RetrievalRequest{Query: query, Corpus: types.CorpusPublic, Options: opts})
		if err != nil {
			st.Status, st.Detail = "error", "recherche dans le corpus indisponible"
			retryable = true
			researchToolsLog.Warnf("Corpus law search failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
		} else {
			for _, h := range res.Hits {
				candidates = append(candidates, lawCandidate{
					Source: "corpus", LegalVersionID: h.LegalVersionID, Label: h.CitationLabel,
					State: h.VersionState, ValidFrom: isoDay(h.ValidFrom), ValidTo: isoDay(h.ValidTo),
					Excerpt: truncateRunes(strings.TrimSpace(h.Text), searchExcerptRunes),
				})
			}
			st.Results = len(res.Hits)
			st.Status = statusOf(len(res.Hits))
		}
		statuses = append(statuses, st)
	} else if !corpusReady() {
		statuses = append(statuses, sourceStatus{Source: "corpus", Status: "unavailable"})
	}

	if legifranceReady() {
		st := sourceStatus{Source: "legifrance"}
		lf, _ := pisteClients()
		res, err := lf.Search(ctx, connectors.SearchQuery{
			Terms: query, Fond: fond, CodeName: str(a.Code), PageSize: searchPageSize, Page: page, AtDate: at,
		})
		if err != nil {
			detail, retry := connectorFailure(err)
			st.Status, st.Detail = "error", detail
			retryable = retryable || retry
			researchToolsLog.Warnf("Légifrance search failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
		} else {
			for _, r := range res.Results {
				candidates = append(candidates, lawCandidate{
					Source: "legifrance", LegifranceArticleID: r.ID, Label: articleLabel(r.TextTitle, r.Num),
					TextTitle: r.TextTitle, State: types.NormalizeLegalVersionState(r.Etat),
				})
			}
			st.Results = len(res.Results)
			st.Status = statusOf(len(res.Results))
		}
		statuses = append(statuses, st)
	} else {
		statuses = append(statuses, sourceStatus{Source: "legifrance", Status: "unavailable"})
	}

	n := len(candidates)
	if n == 0 && allFailed(statuses) {
		return failure(x, types.ToolSearchLaw, types.ToolCodeConnectorFailure, retryable,
			"aucune source de législation n'a répondu", map[string]any{"sources": statuses}, act), nil
	}
	researchToolsLog.Infof("Law search done (run=%s call=%s results=%d basis=%s)", x.RunID, x.CallID, n, basis)
	out := success(x, types.ToolSearchLaw, n == 0, map[string]any{
		"applicability_date":  at.Format("2006-01-02"),
		"applicability_basis": basis,
		"sources":             statuses,
		"candidates":          candidates,
		"note":                "Candidats de recherche : lisez l'article avec read_law avant de le citer ou de vous fonder dessus.",
	}, act)
	out.Counts = counts(intPtr(n), nil)
	return out, nil
}

func statusOf(n int) string {
	if n == 0 {
		return "empty"
	}
	return "ok"
}

func allFailed(statuses []sourceStatus) bool {
	for _, s := range statuses {
		if s.Status == "ok" || s.Status == "empty" {
			return false
		}
	}
	return true
}

func callRef(x ToolExecution) u.CallRef {
	return u.CallRef{TenantID: x.TenantID, RunID: x.RunID, RunStepID: x.StepID, StepAttemptID: x.StepAttemptID}
}

// ---------------------------------------------------------------------------
// read_law
// ---------------------------------------------------------------------------

type readLawArgs struct {
	types.ToolArgs
	LegalVersionID      *string `json:"legal_version_id"`
	LegifranceArticleID *string `json:"legifrance_article_id"`
	ApplicabilityDate   *string `json:"applicability_date"`
	Cursor              *string `json:"cursor"`
}

func readLawTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolReadLaw, types.ToolClassExternal, true, false,
			"Lit le texte exact d'une version d'article (identifiant renvoyé par search_law) et son intervalle de validité. "+
				"Renvoie un evidence_id citable. Indiquez exactement un identifiant.",
			map[string]any{
				"legal_version_id":      nullable(strSchema("legal_version_id d'un candidat du corpus.")),
				"legifrance_article_id": nullable(strSchema("Identifiant LEGIARTI d'un candidat Légifrance.")),
				"applicability_date":    nullable(strSchema("Date d'applicabilité à contrôler (AAAA-MM-JJ) : celle des faits en cause, ou celle du jour pour le droit actuel. Obligatoire, sauf si le contrat de tâche la fixe.")),
				"cursor":                nullable(strSchema("Curseur next_cursor d'une lecture précédente, pour un article long.")),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execReadLaw,
	}
}

func execReadLaw(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectLaw, Label: "Article de loi"}
	var a readLawArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolReadLaw, err, act)
	}
	versionID, articleID := str(a.LegalVersionID), str(a.LegifranceArticleID)
	if (versionID == "") == (articleID == "") {
		return invalid(x, types.ToolReadLaw, badArgs("indiquez exactement un identifiant : legal_version_id ou legifrance_article_id"), act)
	}
	at, basis, err := applicabilityDate(a.ApplicabilityDate, x.Contract)
	if err != nil {
		return invalid(x, types.ToolReadLaw, err, act)
	}

	var v types.LegalVersion
	var doc types.LegalDocument
	officialFetched := false
	if articleID != "" {
		if !reLegiArticleID.MatchString(articleID) {
			return invalid(x, types.ToolReadLaw, badArgs("legifrance_article_id doit être un identifiant LEGIARTI"), act)
		}
		if !legifranceReady() {
			return failure(x, types.ToolReadLaw, types.ToolCodeSourceUnavailable, false,
				"Légifrance n'est pas configuré ; lisez un candidat du corpus (legal_version_id)", nil, act), nil
		}
		lf, _ := pisteClients()
		art, err := lf.GetArticle(ctx, articleID)
		if err != nil {
			var api *connectors.APIError
			if errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusBadRequest) ||
				strings.Contains(err.Error(), "not found") {
				return failure(x, types.ToolReadLaw, types.ToolCodeUnknownReference, false, "article introuvable sur Légifrance", nil, act), nil
			}
			detail, retry := connectorFailure(err)
			researchToolsLog.Warnf("Légifrance article fetch failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
			return failure(x, types.ToolReadLaw, types.ToolCodeConnectorFailure, retry, "Légifrance : "+detail, nil, act), nil
		}
		v, doc, err = storeLegifranceArticle(u.WithCallRef(ctx, callRef(x)), art)
		if errors.Is(err, errEmptyOfficialText) {
			return failure(x, types.ToolReadLaw, types.ToolCodeSourceUnavailable, false, "Légifrance ne fournit aucun texte pour cet article", nil, act), nil
		}
		if err != nil {
			return ToolOutcome{}, err
		}
		officialFetched = true
	} else {
		v, doc, err = loadLegalVersion(ctx, versionID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(x, types.ToolReadLaw, types.ToolCodeUnknownReference, false, "version inconnue", nil, act), nil
		}
		if err != nil {
			return ToolOutcome{}, err
		}
		if doc.Kind != legalKindArticle {
			return invalid(x, types.ToolReadLaw, badArgs("cette version est une décision : utilisez read_decision"), act)
		}
	}
	act.Label = truncateRunes(doc.CitationLabel, 120)

	text := newSegmentedText("legislation", []PieceSegment{{ID: "art", Text: v.Text}})
	ranges, _, next, err := text.readWindow(lawCursor(str(a.Cursor)), lawWindowRunes)
	if err != nil {
		return invalid(x, types.ToolReadLaw, err, act)
	}
	asOf := at.Unix()
	var rows toolRows
	var passages []map[string]any
	for _, r := range ranges {
		loc := types.SourceLocator{Kind: types.LocatorLegislation, ArticleID: officialArticleID(v), VersionID: v.ID, Start: r.Start, End: r.End}
		span := newSpan(x, types.ToolReadLaw, types.CitationSourceLegifrance, v.ID, v.TextSHA256, loc, r.Text, &asOf)
		rows.spans = append(rows.spans, span)
		passages = append(passages, map[string]any{"evidence_id": span.ID, "start": r.Start, "end": r.End, "text": r.Text})
	}
	applicable := inForceAt(v, at)
	data := map[string]any{
		"legal_version_id":      v.ID,
		"article_id":            officialArticleID(v),
		"label":                 doc.CitationLabel,
		"canonical_url":         doc.CanonicalURL,
		"state":                 v.State,
		"valid_from":            isoDay(v.ValidFrom),
		"valid_to":              isoDay(v.ValidTo),
		"applicability_date":    at.Format("2006-01-02"),
		"applicability_basis":   basis,
		"applicable_at_date":    applicable,
		"fetched_from_official": officialFetched,
		"text_runes":            text.Segments[0].Runes,
		"passages":              passages,
	}
	if !applicable {
		data["warning"] = "Cette version n'était pas en vigueur à la date d'applicabilité : ne la citez pas pour ces faits ; cherchez la version applicable."
	}
	if next != "" {
		data["next_cursor"] = strings.TrimPrefix(next, "art")
	}
	out := withRows(success(x, types.ToolReadLaw, false, data, act), rows)
	out.Activity.Detail = validityDetail(v)
	if next != "" {
		out.Observation.NextCursor, out.Observation.Truncated = strings.TrimPrefix(next, "art"), true
	}
	researchToolsLog.Infof("Law read (run=%s call=%s version=%s runes=%d applicable=%t official=%t)",
		x.RunID, x.CallID, v.ID, text.Segments[0].Runes, applicable, officialFetched)
	return out, nil
}

// lawCursor maps the public "@offset" cursor onto the single segment.
func lawCursor(c string) string {
	if c == "" {
		return ""
	}
	if strings.HasPrefix(c, "@") {
		return "art" + c
	}
	return "art@" + c
}

func validityDetail(v types.LegalVersion) string {
	from, to := isoDay(v.ValidFrom), isoDay(v.ValidTo)
	switch {
	case from != "" && to != "":
		return "version du " + from + " au " + to
	case from != "":
		return "version en vigueur depuis le " + from
	}
	return "version " + v.State
}

// newSegmentedText wraps already-segmented public text so the piece reading
// window and cursor logic apply to it unchanged.
func newSegmentedText(format string, segs []PieceSegment) PieceText {
	t := PieceText{Format: format, State: PieceTextReady, Segments: segs}
	return t.finish()
}

// ---------------------------------------------------------------------------
// search_caselaw
// ---------------------------------------------------------------------------

type searchCaselawArgs struct {
	types.ToolArgs
	Query         string  `json:"query"`
	CutoffDate    *string `json:"cutoff_date"`
	Jurisdiction  *string `json:"jurisdiction"`
	Chamber       *string `json:"chamber"`
	PublishedOnly *bool   `json:"published_only"`
	Page          *int    `json:"page"`
}

type decisionCandidate struct {
	Source           string `json:"source"`
	LegalVersionID   string `json:"legal_version_id,omitempty"`
	JudilibreID      string `json:"judilibre_id,omitempty"`
	Label            string `json:"label"`
	DecisionDate     string `json:"decision_date,omitempty"`
	Jurisdiction     string `json:"jurisdiction,omitempty"`
	Chamber          string `json:"chamber,omitempty"`
	Formation        string `json:"formation,omitempty"`
	PublicationLevel string `json:"publication_level,omitempty"`
	Solution         string `json:"solution,omitempty"`
	ECLI             string `json:"ecli,omitempty"`
	Pourvoi          string `json:"pourvoi,omitempty"`
	Summary          string `json:"summary,omitempty"`
}

func searchCaselawTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolSearchCaselaw, types.ToolClassExternal, true, false,
			"Recherche de la jurisprudence (corpus du cabinet et Judilibre), avec une date limite explicite. "+
				"Renvoie des candidats et leurs métadonnées d'autorité ; lisez-les avec read_decision avant de les citer. "+
				"La requête quitte le cabinet : termes juridiques généraux uniquement. Un résultat vide ne prouve pas l'absence de jurisprudence.",
			map[string]any{
				"query":          strSchema("Requête juridique générale, une ligne, 200 caractères au plus."),
				"cutoff_date":    nullable(strSchema("Date limite (AAAA-MM-JJ) : aucune décision postérieure. Null : celle du contrat de tâche, sinon aucune.")),
				"jurisdiction":   nullable(enumSchema("cc : Cour de cassation, ca : cours d'appel.", "cc", "ca")),
				"chamber":        nullable(enumSchema("Chambre de la Cour de cassation.", "soc", "civ1", "civ2", "civ3", "comm", "crim", "mi", "pl")),
				"published_only": map[string]any{"type": []any{"boolean", "null"}, "description": "Seulement les décisions publiées (Bulletin, Rapport, Lettre, Communiqué)."},
				"page":           nullable(intSchema("Page de résultats Judilibre, 1 à 10.")),
			}),
		available: func() bool { return corpusReady() || judilibreReady() },
		exec:      execSearchCaselaw,
	}
}

func execSearchCaselaw(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectQuery, Label: "Recherche Judilibre"}
	var a searchCaselawArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolSearchCaselaw, err, act)
	}
	query, err := publicQueryArg(a.Query)
	if err != nil {
		return invalid(x, types.ToolSearchCaselaw, err, act)
	}
	act.Label = query
	cutoff, err := caselawCutoff(a.CutoffDate, x.Contract)
	if err != nil {
		return invalid(x, types.ToolSearchCaselaw, err, act)
	}
	page, err := pageArg(a.Page)
	if err != nil {
		return invalid(x, types.ToolSearchCaselaw, err, act)
	}
	act.Detail = "Judilibre"
	if cutoff != nil {
		act.Detail += " · jusqu'au " + cutoff.Format("02/01/2006")
	}
	if out, ok, err := gatePublicQuery(ctx, x, types.ToolSearchCaselaw, query, act); !ok {
		return out, err
	}

	var candidates []decisionCandidate
	var statuses []sourceStatus
	retryable := false
	discarded := 0

	if corpusReady() && page == 1 {
		st := sourceStatus{Source: "corpus"}
		opts := DefaultRetrievalOptions()
		opts.TopK = corpusSearchTopK
		opts.Kinds = []string{legalKindDecision}
		if cutoff != nil {
			opts.AsOfDate = cutoff.Unix()
		}
		res, err := Retrieve(u.WithCallRef(ctx, callRef(x)), RetrievalRequest{Query: query, Corpus: types.CorpusPublic, Options: opts})
		if err != nil {
			st.Status, st.Detail = "error", "recherche dans le corpus indisponible"
			retryable = true
			researchToolsLog.Warnf("Corpus caselaw search failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
		} else {
			for _, h := range res.Hits {
				if cutoff != nil && h.DecisionDate != nil && *h.DecisionDate > cutoff.Unix() {
					discarded++
					continue
				}
				if !caselawFilterMatches(h, a) {
					continue
				}
				candidates = append(candidates, decisionCandidate{
					Source: "corpus", LegalVersionID: h.LegalVersionID, Label: h.CitationLabel,
					DecisionDate: isoDay(h.DecisionDate), Jurisdiction: h.Jurisdiction, Formation: h.Formation,
					PublicationLevel: h.PublicationLevel, Solution: h.Solution, ECLI: h.ECLI, Pourvoi: h.Pourvoi,
					Summary: truncateRunes(strings.TrimSpace(h.Text), searchExcerptRunes),
				})
				st.Results++
			}
			st.Status = statusOf(st.Results)
		}
		statuses = append(statuses, st)
	} else if !corpusReady() {
		statuses = append(statuses, sourceStatus{Source: "corpus", Status: "unavailable"})
	}

	if judilibreReady() {
		st := sourceStatus{Source: "judilibre"}
		_, jl := pisteClients()
		q := connectors.DecisionQuery{Query: query, PageSize: searchPageSize, Page: page - 1}
		if j := str(a.Jurisdiction); j != "" {
			q.Jurisdiction = []string{j}
		}
		if ch := str(a.Chamber); ch != "" {
			q.Chamber = []string{ch}
		}
		if a.PublishedOnly != nil && *a.PublishedOnly {
			q.Publication = []string{"b", "r", "l", "c"}
		}
		if cutoff != nil {
			q.DateEnd = *cutoff
		}
		res, err := jl.Search(ctx, q)
		if err != nil {
			detail, retry := connectorFailure(err)
			st.Status, st.Detail = "error", detail
			retryable = retryable || retry
			researchToolsLog.Warnf("Judilibre search failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
		} else {
			for _, d := range res.Results {
				if cutoff != nil {
					if at := d.DecidedAt(); at != nil && *at > cutoff.Unix() {
						discarded++
						continue
					}
				}
				candidates = append(candidates, decisionCandidate{
					Source: "judilibre", JudilibreID: d.ID, Label: decisionCitationLabel(d),
					DecisionDate: isoDay(d.DecidedAt()), Jurisdiction: decisionJurisdiction(d), Chamber: d.Chamber,
					Formation: d.Formation, PublicationLevel: d.PublicationLevel(), Solution: firstNonEmpty(d.Solution, d.SolutionAlt),
					ECLI: d.ECLI, Pourvoi: d.Pourvoi(), Summary: truncateRunes(stripHTML(d.Summary), searchExcerptRunes),
				})
				st.Results++
			}
			st.Status = statusOf(st.Results)
		}
		statuses = append(statuses, st)
	} else {
		statuses = append(statuses, sourceStatus{Source: "judilibre", Status: "unavailable"})
	}

	n := len(candidates)
	if n == 0 && allFailed(statuses) {
		return failure(x, types.ToolSearchCaselaw, types.ToolCodeConnectorFailure, retryable,
			"aucune source de jurisprudence n'a répondu", map[string]any{"sources": statuses}, act), nil
	}
	data := map[string]any{
		"sources":    statuses,
		"candidates": candidates,
		"note": "Candidats de recherche : lisez la décision avec read_decision avant de la citer. " +
			"Ni le classement ni l'absence de résultat n'établissent qu'une solution est constante ou abandonnée.",
	}
	if cutoff != nil {
		data["cutoff_date"] = cutoff.Format("2006-01-02")
		data["discarded_after_cutoff"] = discarded
	}
	researchToolsLog.Infof("Caselaw search done (run=%s call=%s results=%d discarded=%d)", x.RunID, x.CallID, n, discarded)
	out := success(x, types.ToolSearchCaselaw, n == 0, data, act)
	out.Counts = counts(intPtr(n), nil)
	return out, nil
}

// caselawFilterMatches applies the jurisdiction filter to a corpus hit (the
// corpus does not filter by court itself).
func caselawFilterMatches(h Hit, a searchCaselawArgs) bool {
	switch str(a.Jurisdiction) {
	case "cc":
		return strings.Contains(strings.ToLower(h.Jurisdiction), "cassation")
	case "ca":
		return strings.Contains(strings.ToLower(h.Jurisdiction), "appel")
	}
	return true
}

// ---------------------------------------------------------------------------
// read_decision
// ---------------------------------------------------------------------------

type readDecisionArgs struct {
	types.ToolArgs
	LegalVersionID *string `json:"legal_version_id"`
	JudilibreID    *string `json:"judilibre_id"`
	CutoffDate     *string `json:"cutoff_date"`
	Cursor         *string `json:"cursor"`
}

func readDecisionTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolReadDecision, types.ToolClassExternal, true, false,
			"Lit le texte exact d'une décision (identifiant renvoyé par search_caselaw), paragraphe par paragraphe, "+
				"avec sa juridiction, sa date, son ECLI et son numéro de pourvoi. Chaque paragraphe a un evidence_id citable. "+
				"Indiquez exactement un identifiant.",
			map[string]any{
				"legal_version_id": nullable(strSchema("legal_version_id d'un candidat du corpus.")),
				"judilibre_id":     nullable(strSchema("Identifiant Judilibre d'un candidat.")),
				"cutoff_date":      nullable(strSchema("Date limite à contrôler (AAAA-MM-JJ). Null : celle du contrat de tâche.")),
				"cursor":           nullable(strSchema("Curseur next_cursor d'une lecture précédente (ex. « §12 »).")),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execReadDecision,
	}
}

func execReadDecision(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectDecision, Label: "Décision"}
	var a readDecisionArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolReadDecision, err, act)
	}
	versionID, judilibreID := str(a.LegalVersionID), str(a.JudilibreID)
	if (versionID == "") == (judilibreID == "") {
		return invalid(x, types.ToolReadDecision, badArgs("indiquez exactement un identifiant : legal_version_id ou judilibre_id"), act)
	}
	cutoff, err := caselawCutoff(a.CutoffDate, x.Contract)
	if err != nil {
		return invalid(x, types.ToolReadDecision, err, act)
	}

	var v types.LegalVersion
	var doc types.LegalDocument
	fetched := false
	if judilibreID != "" {
		if !reJudilibreID.MatchString(judilibreID) {
			return invalid(x, types.ToolReadDecision, badArgs("judilibre_id invalide"), act)
		}
		if !judilibreReady() {
			return failure(x, types.ToolReadDecision, types.ToolCodeSourceUnavailable, false,
				"Judilibre n'est pas configuré ; lisez un candidat du corpus (legal_version_id)", nil, act), nil
		}
		_, jl := pisteClients()
		d, err := jl.Decision(ctx, judilibreID)
		if err != nil {
			var api *connectors.APIError
			if errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusBadRequest) ||
				strings.Contains(err.Error(), "not found") {
				return failure(x, types.ToolReadDecision, types.ToolCodeUnknownReference, false, "décision introuvable sur Judilibre", nil, act), nil
			}
			detail, retry := connectorFailure(err)
			researchToolsLog.Warnf("Judilibre decision fetch failed (run=%s call=%s): %v", x.RunID, x.CallID, err)
			return failure(x, types.ToolReadDecision, types.ToolCodeConnectorFailure, retry, "Judilibre : "+detail, nil, act), nil
		}
		v, doc, err = storeJudilibreDecision(u.WithCallRef(ctx, callRef(x)), d)
		if errors.Is(err, errEmptyOfficialText) {
			return failure(x, types.ToolReadDecision, types.ToolCodeSourceUnavailable, false, "Judilibre ne fournit pas le texte de cette décision", nil, act), nil
		}
		if err != nil {
			return ToolOutcome{}, err
		}
		fetched = true
	} else {
		v, doc, err = loadLegalVersion(ctx, versionID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(x, types.ToolReadDecision, types.ToolCodeUnknownReference, false, "version inconnue", nil, act), nil
		}
		if err != nil {
			return ToolOutcome{}, err
		}
		if doc.Kind != legalKindDecision {
			return invalid(x, types.ToolReadDecision, badArgs("cette version est un article : utilisez read_law"), act)
		}
	}
	act.Label = truncateRunes(doc.CitationLabel, 120)

	var segs []PieceSegment
	n := 0
	for _, line := range strings.Split(v.Text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n++
		segs = append(segs, PieceSegment{ID: "§" + strconv.Itoa(n), Paragraph: n, Text: strings.TrimRight(line, "\r")})
	}
	text := newSegmentedText("decision", segs)
	ranges, _, next, err := text.readWindow(str(a.Cursor), decisionWindowRunes)
	if err != nil {
		return invalid(x, types.ToolReadDecision, err, act)
	}
	var asOf *int64
	if cutoff != nil {
		c := cutoff.Unix()
		asOf = &c
	}
	decisionID := doc.SourceID
	var rows toolRows
	var paragraphs []map[string]any
	for _, r := range ranges {
		loc := types.SourceLocator{Kind: types.LocatorDecision, DecisionID: decisionID, Paragraph: r.Seg.Paragraph, Start: r.Start, End: r.End}
		span := newSpan(x, types.ToolReadDecision, types.CitationSourceJudilibre, v.ID, v.TextSHA256, loc, r.Text, asOf)
		rows.spans = append(rows.spans, span)
		paragraphs = append(paragraphs, map[string]any{"evidence_id": span.ID, "paragraph": r.Seg.Paragraph, "start": r.Start, "end": r.End, "text": r.Text})
	}
	data := map[string]any{
		"legal_version_id":  v.ID,
		"decision_id":       decisionID,
		"label":             doc.CitationLabel,
		"canonical_url":     doc.CanonicalURL,
		"jurisdiction":      doc.Jurisdiction,
		"formation":         v.Formation,
		"decision_date":     isoDay(v.DecisionDate),
		"publication_level": v.PublicationLevel,
		"solution":          v.Solution,
		"ecli":              v.ECLI,
		"paragraphs_total":  len(segs),
		"paragraphs":        paragraphs,
		"fetched_now":       fetched,
	}
	if cutoff != nil {
		data["cutoff_date"] = cutoff.Format("2006-01-02")
		if v.DecisionDate != nil && *v.DecisionDate > cutoff.Unix() {
			data["after_cutoff"] = true
			data["warning"] = "Décision postérieure à la date limite : elle ne peut pas fonder l'analyse demandée."
		}
	}
	if next != "" {
		data["next_cursor"] = next
	}
	out := withRows(success(x, types.ToolReadDecision, false, data, act), rows)
	if len(ranges) > 0 {
		out.Activity.Detail = fmt.Sprintf("§ %d–%d sur %d", ranges[0].Seg.Paragraph, ranges[len(ranges)-1].Seg.Paragraph, len(segs))
	}
	if next != "" {
		out.Observation.NextCursor, out.Observation.Truncated = next, true
	}
	researchToolsLog.Infof("Decision read (run=%s call=%s version=%s paragraphs=%d/%d fetched=%t)",
		x.RunID, x.CallID, v.ID, len(ranges), len(segs), fetched)
	return out, nil
}

// ---------------------------------------------------------------------------
// search_web — the isolated request (§6.4, owner decision 2)
// ---------------------------------------------------------------------------

type searchWebArgs struct {
	types.ToolArgs
	Query string `json:"query"`
}

func searchWebTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolSearchWeb, types.ToolClassExternal, true, false,
			"Recherche en sources ouvertes (une recherche, quelques pages avec leur extrait). Pour s'orienter, jamais pour fonder : "+
				"une page web n'est pas une source juridique. Seuls les extraits renvoyés sont citables, avec la mention de leur origine. "+
				"La requête quitte le cabinet : termes juridiques généraux uniquement, sans aucun élément du dossier.",
			map[string]any{
				"query": strSchema("Requête générale, une ligne, 200 caractères au plus."),
			}),
		available: func() bool { return u.DB != nil && webSearchConfigured() },
		exec:      execSearchWeb,
	}
}

func webSearchConfigured() bool { return config.Load().OpenRouterAPIKey != "" }

// webSearchModel is the model of the isolated request: the drafter (or the
// planner), never the legacy ":online" default — the server tool does the
// searching, the suffix would add a second, unbounded one.
func webSearchModel() string {
	cfg := config.Load()
	model := firstNonEmpty(cfg.DrafterModel, cfg.PlannerModel)
	return strings.TrimSuffix(model, ":online")
}

// webSearchBody is the WHOLE outbound request: a static system prompt, the
// validated public query, and one bounded web search. No history, no dossier,
// no other tool.
func webSearchBody(model, query string) map[string]any {
	routing := map[string]any{"data_collection": "deny"}
	if allow := strings.TrimSpace(os.Getenv("OPENROUTER_PROVIDER_ALLOWLIST")); allow != "" {
		var only []string
		for _, p := range strings.Split(allow, ",") {
			if p = strings.TrimSpace(p); p != "" {
				only = append(only, p)
			}
		}
		routing["only"], routing["allow_fallbacks"] = only, false
	}
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": webSearchToolPrompt},
			{"role": "user", "content": "<requete>" + query + "</requete>"},
		},
		"tools": []map[string]any{{
			"type": "openrouter:web_search",
			"parameters": map[string]any{
				"engine":            "perplexity",
				"max_results":       webSearchMaxResults,
				"max_total_results": webSearchMaxResults,
				"max_uses":          1,
				"max_characters":    webSearchMaxChars,
			},
		}},
		"max_tool_calls": 1,
		"max_tokens":     1500,
		"provider":       routing,
		"usage":          map[string]any{"include": true},
	}
}

const webSearchToolPrompt = `<role>
Tu es documentaliste juridique. Tu exécutes UNE recherche en sources ouvertes
sur la requête fournie et tu rapportes ce que les pages trouvées disent.
</role>

<tasks>
1. Effectue une seule recherche web sur la requête, telle quelle.
2. Résume en quelques lignes ce que les pages consultées établissent, en
   attribuant chaque élément à sa page.
3. N'ajoute rien de mémoire : aucune référence, aucun article, aucune date qui
   ne figure pas dans les pages consultées.
</tasks>

<output>
Texte brut en français, dix lignes au plus, sans titres. Si les pages ne
permettent pas de répondre, dis-le en une phrase.
</output>`

type webCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content     string `json:"content"`
			Annotations []struct {
				Type        string          `json:"type"`
				URLCitation json.RawMessage `json:"url_citation"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int             `json:"prompt_tokens"`
		CompletionTokens     int             `json:"completion_tokens"`
		Cost                 float64         `json:"cost"`
		ServerToolUse        json.RawMessage `json:"server_tool_use"`
		ServerToolUseDetails json.RawMessage `json:"server_tool_use_details"`
	} `json:"usage"`
}

type urlCitation struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

var webSearchHTTP = &http.Client{Timeout: webRequestTimeout}

// webResult is one returned page as the agent sees it.
type webResult struct {
	WebSourceID string `json:"web_source_id"`
	EvidenceID  string `json:"evidence_id,omitempty"`
	URL         string `json:"url"`
	Host        string `json:"host"`
	Title       string `json:"title,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
	Citable     bool   `json:"citable"`
}

func execSearchWeb(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectQuery, Label: "Recherche en sources ouvertes", Detail: "Sources ouvertes"}
	var a searchWebArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolSearchWeb, err, act)
	}
	query, err := publicQueryArg(a.Query)
	if err != nil {
		return invalid(x, types.ToolSearchWeb, err, act)
	}
	act.Label = query
	if x.Budget.WebSearchesLeft <= 0 {
		return failure(x, types.ToolSearchWeb, types.ToolCodeBudgetExhausted, false,
			fmt.Sprintf("le budget de %d recherches web de cette tâche est épuisé", types.MaxAgentWebSearches), nil, act), nil
	}
	if out, ok, err := gatePublicQuery(ctx, x, types.ToolSearchWeb, query, act); !ok {
		return out, err
	}
	reserved, err := reserveTurnBudget(ctx, x, "web_searches", types.MaxAgentWebSearches)
	if err != nil {
		return ToolOutcome{}, err
	}
	if !reserved {
		return failure(x, types.ToolSearchWeb, types.ToolCodeBudgetExhausted, false,
			fmt.Sprintf("le budget de %d recherches web de cette tâche est épuisé", types.MaxAgentWebSearches), nil, act), nil
	}

	model := webSearchModel()
	comp, status, err := dispatchWebSearch(ctx, x, model, query)
	if errors.Is(err, errWebAccounting) {
		// Nothing was dispatched: the run cannot spend money it cannot
		// attribute.
		return ToolOutcome{}, err
	}
	if err != nil {
		retryable := status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
		researchToolsLog.Warnf("Web search failed (run=%s call=%s status=%d): %v", x.RunID, x.CallID, status, err)
		return failure(x, types.ToolSearchWeb, types.ToolCodeConnectorFailure, retryable, "la recherche en ligne a échoué", nil, act), nil
	}

	var rows toolRows
	var results []webResult
	seen := map[string]bool{}
	summary := ""
	if len(comp.Choices) > 0 {
		summary = truncateRunes(strings.TrimSpace(comp.Choices[0].Message.Content), webSummaryRunes)
		for _, ann := range comp.Choices[0].Message.Annotations {
			if ann.Type != "url_citation" {
				continue
			}
			var c urlCitation
			if json.Unmarshal(ann.URLCitation, &c) != nil {
				continue
			}
			canonical, host, ok := canonicalWebURL(c.URL)
			content := strings.TrimSpace(c.Content)
			key := canonical + "\x00" + sha256Hex(content)
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			src := types.AgentWebSource{
				RunID:         x.RunID,
				URL:           canonical,
				URLSHA256:     sha256Hex(canonical),
				Host:          host,
				Title:         truncateRunes(strings.TrimSpace(c.Title), 300),
				Content:       content,
				ContentSHA256: sha256Hex(content),
				Extent:        types.WebExtentExcerpt,
				Annotation:    types.JSONB(ann.URLCitation),
				ToolCallID:    x.CallID,
			}
			src.ID = agentRowID("ws_", x.RunID, src.URLSHA256, src.ContentSHA256)
			rows.web = append(rows.web, src)
			res := webResult{WebSourceID: src.ID, URL: canonical, Host: host, Title: src.Title, Excerpt: content}
			if content != "" {
				loc := types.SourceLocator{Kind: types.LocatorWeb, URL: canonical, Start: 0, End: utf8.RuneCountInString(content)}
				span := newSpan(x, types.ToolSearchWeb, types.CitationSourceWeb, src.ID, src.ContentSHA256, loc, content, nil)
				rows.spans = append(rows.spans, span)
				res.EvidenceID, res.Citable = span.ID, true
			}
			results = append(results, res)
		}
	}
	n := len(results)
	out := withRows(success(x, types.ToolSearchWeb, n == 0, map[string]any{
		"query":               query,
		"results":             results,
		"summary_not_citable": summary,
		"searches_left":       x.Budget.WebSearchesLeft - 1,
		"note": "Sources ouvertes : orientation uniquement. Seul un extrait renvoyé (evidence_id) peut être cité, comme source web, " +
			"jamais comme autorité juridique. Une page sans extrait est une piste, pas un appui. Le résumé n'est pas une source.",
	}, act), rows)
	out.Counts = counts(intPtr(n), nil)
	researchToolsLog.Infof("Web search done (run=%s call=%s model=%s results=%d)", x.RunID, x.CallID, comp.Model, n)
	return out, nil
}

// canonicalWebURL keeps http(s) URLs only, without fragment.
func canonicalWebURL(raw string) (string, string, bool) {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return "", "", false
	}
	p.Fragment = ""
	p.Host = strings.ToLower(p.Host)
	return p.String(), strings.TrimPrefix(p.Hostname(), "www."), true
}

// reserveTurnBudget takes one slot of a per-run budget on agent_turns BEFORE
// the paid call, so a retry consumes a slot too. false means the budget is
// spent (or the run has no agent turn).
func reserveTurnBudget(ctx context.Context, x ToolExecution, column string, ceiling int) (bool, error) {
	res := x.Repo(ctx).AgentTurns().
		Where("run_id = ? AND "+column+" < ?", x.RunID, ceiling).
		UpdateColumn(column, gorm.Expr(column+" + 1"))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// errWebAccounting means the llm_calls row could not be written before
// dispatch, so nothing was sent.
var errWebAccounting = errors.New("web search accounting could not be recorded")

// dispatchWebSearch sends the isolated request with durable accounting: the
// llm_calls row is written before dispatch (failure → no dispatch), and the
// usage and cost are recorded on it afterwards. One attempt, no
// hidden retry. status is the HTTP status, 0 when no response arrived.
func dispatchWebSearch(ctx context.Context, x ToolExecution, model, query string) (webCompletion, int, error) {
	cfg := config.Load()
	executionID := x.ExecutionID
	if executionID == "" {
		executionID = uuid.NewString()
	}
	call := types.LLMCall{
		ID:            uuid.NewString(),
		RunID:         nullableID(x.RunID),
		RunStepID:     nullableID(x.StepID),
		StepAttemptID: nullableID(x.StepAttemptID),
		ExecutionID:   executionID,
		Model:         model,
		Purpose:       types.LLMPurposeWebSearch,
		State:         types.LLMCallStateDispatched,
		UsageStatus:   types.UsageStatusUnknown,
		ActorID:       x.ActorID,
	}
	if err := x.Repo(ctx).Create(&call); err != nil {
		return webCompletion{}, 0, fmt.Errorf("%w: %v", errWebAccounting, err)
	}

	payload, _ := json.Marshal(webSearchBody(model, query))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.OpenRouterBaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		finishWebCall(x, call, 0, webCompletion{})
		return webCompletion{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)
	resp, err := webSearchHTTP.Do(req)
	if err != nil {
		finishWebCall(x, call, 0, webCompletion{})
		return webCompletion{}, 0, fmt.Errorf("web search request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode >= 400 {
		finishWebCall(x, call, resp.StatusCode, webCompletion{})
		return webCompletion{}, resp.StatusCode, fmt.Errorf("web search: status %d", resp.StatusCode)
	}
	var comp webCompletion
	if err := json.Unmarshal(body, &comp); err != nil {
		finishWebCall(x, call, resp.StatusCode, webCompletion{})
		return webCompletion{}, resp.StatusCode, fmt.Errorf("decode web search: %w", err)
	}
	finishWebCall(x, call, resp.StatusCode, comp)
	return comp, resp.StatusCode, nil
}

func nullableID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// finishWebCall reconciles the llm_calls row with the usage and the
// provider-reported cost, which includes the server-tool fees. Detached from
// the request context: the money is already spent.
func finishWebCall(x ToolExecution, call types.LLMCall, status int, comp webCompletion) {
	ok := status >= 200 && status < 400 && comp.ID != ""
	state := types.LLMCallStateFailed
	if ok {
		state = types.LLMCallStateSucceeded
	}
	updates := map[string]any{"state": state, "finished_at": time.Now().Unix()}
	if ok {
		updates["provider_request_id"] = comp.ID
		updates["returned_model"] = comp.Model
	}
	known := ok && comp.Usage != nil
	var details map[string]any
	if known {
		details = map[string]any{"endpoint": "agent." + types.ToolSearchWeb, "user_id": x.ActorID}
		if len(comp.Usage.ServerToolUseDetails) > 0 {
			details["server_tool_use_details"] = json.RawMessage(comp.Usage.ServerToolUseDetails)
		}
		if len(comp.Usage.ServerToolUse) > 0 {
			details["server_tool_use"] = json.RawMessage(comp.Usage.ServerToolUse)
		}
		usageJSON, _ := json.Marshal(details)
		updates["usage_status"] = types.UsageStatusKnown
		updates["prompt_tokens"] = comp.Usage.PromptTokens
		updates["completion_tokens"] = comp.Usage.CompletionTokens
		updates["cost_nano_usd"] = u.NanoUSD(comp.Usage.Cost)
		updates["usage_details"] = types.JSONB(usageJSON)
	}
	err := u.ForTenantID(context.Background(), x.TenantID).LLMCalls().Where("id = ?", call.ID).Updates(updates).Error
	if err != nil {
		researchToolsLog.Errorf("Could not reconcile the web search llm_call (run=%s call=%s): %v", x.RunID, x.CallID, err)
	}
}

// ---------------------------------------------------------------------------
// Compétences (owner decision 5)
// ---------------------------------------------------------------------------

type listCompetencesArgs struct {
	types.ToolArgs
}

func listCompetencesTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolListCompetences, types.ToolClassDB, true, false,
			"Liste les compétences (modèles d'actes du cabinet) disponibles. Une compétence ne s'applique que si l'avocat l'a choisie ; "+
				"pour un acte demandé en texte libre, proposez-la avec ask_lawyer.",
			map[string]any{}),
		available: func() bool { return u.DB != nil },
		exec:      execListCompetences,
	}
}

func execListCompetences(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectCompetence, Label: "Compétences disponibles"}
	var a listCompetencesArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolListCompetences, err, act)
	}
	repo := x.Repo(ctx)
	// The agent may read the library before the composer ever listed it.
	if err := ensureBuiltinSkills(repo); err != nil {
		return ToolOutcome{}, err
	}
	var total int64
	if err := repo.Skills().Where("state = ?", types.SkillStateActive).Count(&total).Error; err != nil {
		return ToolOutcome{}, err
	}
	var skills []types.Skill
	if err := repo.Skills().Where("state = ?", types.SkillStateActive).Order("name ASC").Limit(100).Find(&skills).Error; err != nil {
		return ToolOutcome{}, err
	}
	items := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		pinned := x.Contract.Competence != nil && x.Contract.Competence.SkillID == s.ID
		items = append(items, map[string]any{
			"slug": s.Slug, "name": s.Name, "description": truncateRunes(s.Description, 300),
			"current_version": s.CurrentVersion, "pinned_for_this_task": pinned,
		})
	}
	researchToolsLog.Debugf("Compétences listed (run=%s call=%s count=%d)", x.RunID, x.CallID, len(items))
	out := success(x, types.ToolListCompetences, len(items) == 0, map[string]any{
		"competences": items, "total": total,
	}, act)
	out.Counts = counts(intPtr(len(items)), intPtr(int(total)))
	return out, nil
}

type loadCompetenceArgs struct {
	types.ToolArgs
	Slug string `json:"slug"`
}

func loadCompetenceTool() AgentTool {
	return &agentTool{
		def: toolDef(types.ToolLoadCompetence, types.ToolClassDB, true, false,
			"Charge la version courante d'une compétence : sections, ton, clauses obligatoires et liste de contrôle, "+
				"à respecter comme contraintes de rédaction. Une référence juridique d'un modèle reste à rechercher et vérifier.",
			map[string]any{
				"slug": strSchema("Identifiant de la compétence (slug)."),
			}),
		available: func() bool { return u.DB != nil },
		exec:      execLoadCompetence,
	}
}

func execLoadCompetence(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
	act := types.AgentActivityObject{Kind: types.ActivityObjectCompetence, Label: "Compétence"}
	var a loadCompetenceArgs
	if err := decodeArgs(raw, &a); err != nil {
		return invalid(x, types.ToolLoadCompetence, err, act)
	}
	slug := strings.TrimSpace(a.Slug)
	if !skillSlugPattern.MatchString(slug) {
		return invalid(x, types.ToolLoadCompetence, badArgs("slug invalide"), act)
	}
	repo := x.Repo(ctx)
	if err := ensureBuiltinSkills(repo); err != nil {
		return ToolOutcome{}, err
	}
	var skill types.Skill
	err := repo.Skills().Where("slug = ? AND state <> ?", slug, types.SkillStateArchived).Take(&skill).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return failure(x, types.ToolLoadCompetence, types.ToolCodeUnknownReference, false, "compétence inconnue", nil, act), nil
	}
	if err != nil {
		return ToolOutcome{}, err
	}
	act.Label = truncateRunes(skill.Name, 120)
	version := skill.CurrentVersion
	if c := x.Contract.Competence; c != nil && c.SkillID == skill.ID {
		// The task pinned a version at intake; that one applies, even if
		// the compétence moved on since.
		version = c.Version
	}
	var sv types.SkillVersion
	err = repo.SkillVersions().Where("skill_id = ? AND version = ?", skill.ID, version).Take(&sv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return failure(x, types.ToolLoadCompetence, types.ToolCodeSourceUnavailable, false, "cette compétence n'a pas de version publiée", nil, act), nil
	}
	if err != nil {
		return ToolOutcome{}, err
	}
	var def skillDefinition
	if len(sv.Definition) > 0 {
		if err := json.Unmarshal(sv.Definition, &def); err != nil {
			return failure(x, types.ToolLoadCompetence, types.ToolCodeSourceUnavailable, false, "définition de compétence illisible", nil, act), nil
		}
	}
	researchToolsLog.Infof("Compétence loaded (run=%s call=%s skill=%s version=%d)", x.RunID, x.CallID, skill.ID, sv.Version)
	out := success(x, types.ToolLoadCompetence, false, map[string]any{
		"skill_id":          skill.ID,
		"skill_version_id":  sv.ID,
		"slug":              skill.Slug,
		"name":              skill.Name,
		"version":           sv.Version,
		"pinned":            x.Contract.Competence != nil && x.Contract.Competence.SkillID == skill.ID,
		"sections":          def.Sections,
		"tone":              def.Tone,
		"mandatory_clauses": def.MandatoryClauses,
		"checklist":         def.Checklist,
		"note": "Contraintes de rédaction. Une compétence n'élargit ni le périmètre du dossier ni les outils, et ne dispense d'aucune vérification ; " +
			"toute référence juridique qu'elle mentionne doit être lue (read_law, read_decision) avant d'être citée.",
	}, act)
	out.Activity.Detail = "version " + strconv.Itoa(sv.Version)
	return out, nil
}
