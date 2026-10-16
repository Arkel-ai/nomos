// Package services — hybrid legal retrieval (docs/PLAN.md §6.1–§6.4).
//
// WHAT THIS PACKAGE'S RETRIEVAL LAYER IS, STATED PLAINLY (§6.3):
//
//	RRF IS A CANDIDATE GENERATOR AND THE RERANKER IS A RELEVANCE OPTIMISER.
//	NEITHER OF THEM ESTABLISHES THAT A DECISION WAS OVERRULED, DISTINGUISHED,
//	LIMITED OR SUPERSEDED.
//
// A revirement de jurisprudence is frequently carried by a later decision that
// is lexically awkward, differently worded and not in the top 50 of either
// arm; and a reranker rewards topical similarity, not precedential direction.
// So nothing in this file may be described — in code, in a comment, in a log
// line or in the UI — as detecting revirements. What supports that question is
// jurisdiction, formation, publication level, solution and dates, exact
// citation matching, source-provided visa/rapprochements, a forward search for
// later decisions citing a candidate, and the typed, provenance-carrying rows
// of legal_relations. This layer surfaces candidates with the metadata a
// lawyer needs to judge authority. It never asserts that the law changed.
//
// The two corpora are deliberately different systems (§6.1):
//
//   - PUBLIC (legal_chunks): one statement per query — vector top-N over the
//     HNSW index (halfvec_cosine_ops) plus lexical top-N over the GIN index
//     (an OR-of-lexemes tsquery over 'french'+unaccent, ranked by ts_rank_cd) —
//     fused by reciprocal rank fusion, 1/(k+rank), with NO score
//     normalisation, then reranked.
//   - PRIVATE (piece_chunks): NO HNSW, on purpose. Filter on
//     tenant_id + dossier_id FIRST, then score exactly. A shared approximate
//     index returns globally-near neighbours that the tenant filter then
//     discards; a dossier is hundreds to a few thousand chunks, so exact is
//     both correct and fast. Every private read goes through the mandatory
//     tenant-scoped repository — never a bare utils.DB query.
package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var retrievalLog = u.NewLogger("services.retrieval")

// ---------------------------------------------------------------------------
// Knobs
// ---------------------------------------------------------------------------

// Retrieval strategies. They exist as an explicit, switchable set because the
// relevance harness scores each configuration against the same query set: a
// fusion that is not measurable against its own single arms is a claim, not a
// result.
const (
	StrategyVector    = "vector"     // vector arm only
	StrategyFTS       = "fts"        // french full-text arm only
	StrategyRRF       = "rrf"        // both arms, reciprocal rank fusion
	StrategyRRFRerank = "rrf_rerank" // RRF then cross-encoder rerank — the default
)

// Strategies is the switchable set, in the order the harness should report.
var Strategies = []string{StrategyVector, StrategyFTS, StrategyRRF, StrategyRRFRerank}

const (
	// defaultArmDepth is the top-N taken from EACH arm before fusion (§6.2).
	defaultArmDepth = 50
	// defaultFuseDepth is how many fused candidates reach the reranker.
	defaultFuseDepth = 40
	// defaultTopK is what survives — §6.2 says keep 8–12.
	defaultTopK = 10
	// defaultArmWeightVector / defaultArmWeightFTS weight the two arms inside
	// the fusion. Plain RRF weights every arm equally, which silently assumes
	// they are comparably good. On this corpus they are not: the relevance
	// harness measures the dense arm at nDCG@10 0.835 and the lexical arm at
	// 0.380, and equal-weight fusion of the two lands at 0.673 — BELOW the
	// dense arm alone. A weaker arm cannot be allowed to outvote a stronger
	// one simply by showing up. These weights are a measured default, not a
	// principle: re-measure with api/eval before changing them, and expect the
	// right ratio to move once the corpus is bigger than a few hundred articles.
	defaultArmWeightVector = 1.0
	defaultArmWeightFTS    = 0.4

	// defaultRRFK is the k of 1/(k+rank). 60 is the constant from the
	// original RRF paper and the one §6.2 fixes.
	defaultRRFK = 60.0
	// maxTopK bounds what one request may ask for.
	maxTopK = 50

	// ftsRankFloor is the minimum ts_rank_cd a chunk must reach to enter the
	// lexical arm. It exists because that arm ORs its terms (see
	// publicFTSCTE): without a floor, a chunk matching one common lexeme —
	// "travail", "contrat" — would occupy a slot that RRF then rewards. The
	// value is deliberately low: the job is to cut the long tail of
	// single-common-word hits, not to second-guess the ranking. Raise it only
	// with a relevance measurement in hand (see api/eval).
	ftsRankFloor = 0.02
)

// legalVersionStateActive is the state retrieval filters on by default.
// "Default to the state in force" (§6.4) means this value; a version in any
// other state is only reachable by asking for it explicitly.
//
// It MUST stay identical to what the corpus loader writes, which is why both
// sides now go through types.NormalizeLegalVersionState rather than each
// spelling the string themselves. They previously disagreed — the loader wrote
// "vigueur" and this filter asked for "active" — and the only symptom was that
// POST /api/search returned nothing at all, on a corpus that was fully loaded.
const legalVersionStateActive = types.LegalVersionInForce

var (
	// ErrEmptyQuery — retrieval with no query is a caller bug.
	ErrEmptyQuery = errors.New("retrieval: empty query")
	// ErrPrivateNeedsDossier — a private search without a dossier scope is a
	// bug, not a broad search (§6.1).
	ErrPrivateNeedsDossier = errors.New("retrieval: a private search must name a dossier")
	// ErrUnknownStrategy — the caller asked for a strategy that does not exist.
	ErrUnknownStrategy = errors.New("retrieval: unknown strategy")
	// ErrUnknownCorpus — corpus must be public, private or both.
	ErrUnknownCorpus = errors.New("retrieval: unknown corpus")
)

// RetrievalOptions is every knob, in one place, so the relevance harness can
// score one configuration against another without touching this file.
type RetrievalOptions struct {
	// Strategy is one of the Strategies constants. Empty means the default.
	Strategy string

	// ArmDepth is the top-N taken from each arm before fusion.
	ArmDepth int
	// FuseDepth is how many fused candidates are handed to the reranker.
	FuseDepth int
	// TopK is how many hits survive.
	TopK int
	// ArmWeightVector / ArmWeightFTS weight each arm's contribution to the
	// fused score. Zero means "use the measured default" (see the constants).
	ArmWeightVector float64
	ArmWeightFTS    float64

	// RRFK is the k of 1/(k+rank). No score normalisation happens anywhere:
	// that is the entire point of rank fusion.
	RRFK float64

	// EfSearch is pgvector's hnsw.ef_search for this query. It MUST be at
	// least ArmDepth: the index cannot return more candidates than ef_search,
	// so leaving it at the 40 default silently truncates a 50-deep arm.
	EfSearch int

	// AsOfDate is the matter date in unix seconds — the date of the FACTS,
	// not today (§6.4). Zero means now, which is only correct for a current
	// dispute. Versions are filtered on the half-open interval
	// valid_from <= d AND (valid_to IS NULL OR d < valid_to).
	AsOfDate int64
	// States restricts legal_versions.state. Nil means the in-force state
	// only; pass an explicit list to reach superseded or abrogated text.
	States []string
	// Sources restricts legal_documents.source (legifrance, judilibre, …).
	// Nil means every source.
	Sources []string
	// Kinds restricts legal_documents.kind (article, decision, …). Nil means
	// every kind. It is what separates a statute search from a case-law
	// search — two questions with different authority and different failure
	// modes, which is why the note-de-synthèse workflow runs them as separate
	// steps rather than fusing them into one ranked list where a corpus with
	// no case law at all would silently look like a corpus with weak case law.
	Kinds []string

	// EmbeddingModel / RerankModel: empty means the configured default.
	// The embedding model is not freely switchable — the dimension is baked
	// into the halfvec(1024) column — but the knob has to be visible.
	EmbeddingModel string
	RerankModel    string
}

// DefaultRetrievalOptions is the shipped configuration: RRF over both arms,
// then rerank, ten hits, as of today.
func DefaultRetrievalOptions() RetrievalOptions {
	return RetrievalOptions{
		Strategy:  StrategyRRFRerank,
		ArmDepth:  defaultArmDepth,
		FuseDepth: defaultFuseDepth,
		TopK:      defaultTopK,
		RRFK:      defaultRRFK,
	}
}

// ParseStrategy validates a caller-supplied strategy name.
func ParseStrategy(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return StrategyRRFRerank, nil
	}
	for _, known := range Strategies {
		if s == known {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownStrategy, s)
}

func (o RetrievalOptions) normalized() RetrievalOptions {
	if o.Strategy == "" {
		o.Strategy = StrategyRRFRerank
	}
	if o.ArmDepth <= 0 {
		o.ArmDepth = defaultArmDepth
	}
	if o.TopK <= 0 {
		o.TopK = defaultTopK
	}
	if o.TopK > maxTopK {
		o.TopK = maxTopK
	}
	if o.FuseDepth <= 0 {
		o.FuseDepth = defaultFuseDepth
	}
	if o.FuseDepth < o.TopK {
		o.FuseDepth = o.TopK
	}
	if o.RRFK <= 0 {
		o.RRFK = defaultRRFK
	}
	if o.ArmWeightVector <= 0 {
		o.ArmWeightVector = defaultArmWeightVector
	}
	if o.ArmWeightFTS <= 0 {
		o.ArmWeightFTS = defaultArmWeightFTS
	}
	if o.EfSearch < o.ArmDepth {
		// Two arms' worth of graph exploration: enough that the temporal and
		// source predicates can discard candidates without starving the arm.
		o.EfSearch = 2 * o.ArmDepth
	}
	if o.AsOfDate <= 0 {
		o.AsOfDate = time.Now().Unix()
	}
	if len(o.States) == 0 {
		o.States = []string{legalVersionStateActive}
	}
	return o
}

func (o RetrievalOptions) usesVector() bool {
	return o.Strategy != StrategyFTS
}

func (o RetrievalOptions) usesFTS() bool {
	return o.Strategy != StrategyVector
}

func (o RetrievalOptions) usesRerank() bool {
	return o.Strategy == StrategyRRFRerank
}

// ---------------------------------------------------------------------------
// Hits
// ---------------------------------------------------------------------------

// Hit is types.SearchHit plus the metadata a lawyer needs to judge AUTHORITY,
// which the wire type alone does not carry (§6.3, §6.4): which court, which
// formation, how it was published, what it decided, when, under which ECLI or
// pourvoi number, and for which period the text was in force.
//
// Score is whatever ordered the final list — the rerank score when the rerank
// pass ran, the RRF score otherwise. RRFScore, VectorRank and FTSRank are
// always the raw fusion inputs, so the harness can attribute a result to an
// arm instead of guessing.
type Hit struct {
	types.SearchHit

	Source           string `json:"source,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Jurisdiction     string `json:"jurisdiction,omitempty"`
	Formation        string `json:"formation,omitempty"`
	PublicationLevel string `json:"publication_level,omitempty"`
	Solution         string `json:"solution,omitempty"`
	DecisionDate     *int64 `json:"decision_date,omitempty"`
	ECLI             string `json:"ecli,omitempty"`
	Pourvoi          string `json:"pourvoi,omitempty"`
	VersionState     string `json:"version_state,omitempty"`

	RRFScore    float64 `json:"rrf_score"`
	VectorRank  int     `json:"vector_rank"`
	FTSRank     int     `json:"fts_rank"`
	VectorScore float64 `json:"vector_score"`
	FTSScore    float64 `json:"fts_score"`
}

// RetrievalRequest is one search. Repo and DossierID are mandatory for the
// private corpus and ignored for the public one.
type RetrievalRequest struct {
	Query     string
	Corpus    string
	Repo      *u.TenantRepo
	DossierID string
	Options   RetrievalOptions
	// PieceIDs restricts the private arms to these pieces of the dossier.
	// Nil means the whole dossier; a non-nil empty slice is an explicit
	// empty subset and matches nothing — the two must never be confused.
	PieceIDs []string
}

// RetrievalResult reports what ran as well as what came back. Reranked is not
// cosmetic: when the gateway is unavailable the RRF order is returned as-is
// and no caller may present it as reranked.
type RetrievalResult struct {
	Hits       []Hit
	Strategy   string
	Reranked   bool
	AsOfDate   int64
	Candidates int
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// Retrieve runs one configuration end to end. It is the single entry point for
// both POST /api/search and the relevance harness — a harness that scores a
// different code path scores nothing.
func Retrieve(ctx context.Context, req RetrievalRequest) (RetrievalResult, error) {
	opts := req.Options.normalized()
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return RetrievalResult{}, ErrEmptyQuery
	}
	if _, err := ParseStrategy(opts.Strategy); err != nil {
		return RetrievalResult{}, err
	}
	corpus := req.Corpus
	if corpus == "" {
		corpus = types.CorpusPublic
	}
	wantPublic := corpus == types.CorpusPublic || corpus == types.CorpusBoth
	wantPrivate := corpus == types.CorpusPrivate || corpus == types.CorpusBoth
	if !wantPublic && !wantPrivate {
		return RetrievalResult{}, fmt.Errorf("%w: %s", ErrUnknownCorpus, corpus)
	}
	if wantPrivate && (req.Repo == nil || req.DossierID == "") {
		return RetrievalResult{}, ErrPrivateNeedsDossier
	}
	if u.DB == nil {
		return RetrievalResult{}, u.ErrAuthNotConfigured
	}

	// One embedding for the whole request, reused by both corpora.
	var qv *pgvector.HalfVector
	if opts.usesVector() {
		vecs, err := u.Embed(ctx, opts.EmbeddingModel, []string{query})
		if err != nil {
			return RetrievalResult{}, fmt.Errorf("embed query: %w", err)
		}
		v := pgvector.NewHalfVector(vecs[0])
		qv = &v
	}

	var candidates []Hit
	if wantPublic {
		hits, err := publicCandidates(ctx, query, qv, opts)
		if err != nil {
			return RetrievalResult{}, err
		}
		candidates = append(candidates, hits...)
	}
	if wantPrivate {
		hits, err := privateCandidates(ctx, req.Repo, req.DossierID, req.PieceIDs, query, qv, opts)
		if err != nil {
			return RetrievalResult{}, err
		}
		candidates = append(candidates, hits...)
	}

	// Across corpora the RRF scores are comparable because they are produced
	// by the same formula with the same k — which is exactly why RRF fuses
	// ranks and never normalises scores.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].RRFScore > candidates[j].RRFScore
	})
	if len(candidates) > opts.FuseDepth {
		candidates = candidates[:opts.FuseDepth]
	}
	total := len(candidates)

	reranked := false
	if opts.usesRerank() && len(candidates) > 0 {
		ordered, err := rerankCandidates(ctx, query, candidates, opts)
		if err != nil {
			// Degrade to RRF order and SAY SO. Passing fusion order off as
			// reranked order would be a lie the UI cannot detect.
			retrievalLog.Warnf("Rerank unavailable, falling back to RRF order (candidates=%d): %v", len(candidates), err)
		} else {
			candidates, reranked = ordered, true
		}
	}

	if len(candidates) > opts.TopK {
		candidates = candidates[:opts.TopK]
	}
	for i := range candidates {
		if !reranked {
			candidates[i].Score = candidates[i].RRFScore
			candidates[i].RerankRank = 0
		}
	}

	retrievalLog.Infof("Retrieval done (strategy=%s corpus=%s candidates=%d hits=%d reranked=%t as_of=%d)",
		opts.Strategy, corpus, total, len(candidates), reranked, opts.AsOfDate)

	return RetrievalResult{
		Hits:       candidates,
		Strategy:   opts.Strategy,
		Reranked:   reranked,
		AsOfDate:   opts.AsOfDate,
		Candidates: total,
	}, nil
}

// ---------------------------------------------------------------------------
// Public corpus — one statement, two arms, RRF
// ---------------------------------------------------------------------------

// publicRow is the flat shape the fused statement returns.
type publicRow struct {
	ChunkID          uint64
	Text             string
	LegalVersionID   string
	ValidFrom        *int64
	ValidTo          *int64
	DecisionDate     *int64
	Formation        string
	PublicationLevel string
	Solution         string
	ECLI             string `gorm:"column:ecli"`
	VersionState     string
	Pourvoi          string
	Source           string
	Kind             string
	Jurisdiction     string
	CanonicalURL     string `gorm:"column:canonical_url"`
	CitationLabel    string
	LicenseCode      string
	RRFScore         float64 `gorm:"column:rrf_score"`
	VectorRank       int
	FTSRank          int `gorm:"column:fts_rank"`
	VectorScore      float64
	FTSScore         float64 `gorm:"column:fts_score"`
}

// publicVectorCTE is the ANN arm.
//
// The ordering and the LIMIT sit in the INNER subquery, alone: that shape is
// what lets the planner answer it with an ordered index scan over
// idx_legal_chunks_embedding (hnsw, halfvec_cosine_ops) and stop at
// @arm_depth rows. Wrapping row_number() around the join directly instead
// makes the window's ORDER BY sort the whole joined relation and the index
// goes unused — a retrieval-speed bug that no relevance measurement can see.
const publicVectorCTE = `
vec AS (
  SELECT chunk_id,
         row_number() OVER (ORDER BY dist) AS rnk,
         (1 - dist)::float8 AS score,
         'vector'::text AS arm
    FROM (
      SELECT c.id AS chunk_id, (c.embedding <=> CAST(@qv AS halfvec)) AS dist
        FROM legal_chunks c
        JOIN legal_versions v ON v.id = c.legal_version_id
        JOIN legal_documents d ON d.id = v.legal_document_id
       WHERE c.embedding IS NOT NULL
         %[1]s
       ORDER BY c.embedding <=> CAST(@qv AS halfvec)
       LIMIT @arm_depth
    ) t
)`

// publicFTSCTE is the lexical arm, matched against the stored tsvector through
// idx_legal_chunks_fts (gin) and ranked by ts_rank_cd. unaccent is applied to
// the query exactly as it was applied to the text at write time, so "conge
// paye" and "congé payé" behave identically. Same inner-subquery shape: the GIN
// index answers the @@ predicate, and only the surviving rows are ranked.
//
// The tsquery is built with OR, not AND. This arm previously used
// websearch_to_tsquery, which ANDs every term — and the relevance harness
// measured what that costs: recall@10 0.125, and 0.000 in the `contrat`
// category, because a lawyer's natural-language query runs 10–20 tokens and one
// out-of-vocabulary token empties the whole conjunction:
//
//	websearch_to_tsquery('french', unaccent('durée de la période d''essai en CDI'))
//	  → 'dure' & 'period' & 'essai' & 'cdi'  → 0 rows
//	  → drop the single token "cdi"          → 5 rows
//
// Fusing a near-empty arm cannot add recall, it only reshuffles the dense
// ordering, which is why rrf (0.809 nDCG) scored BELOW vector alone (0.835).
// ftsQueryExpr rebuilds the query as the OR of its own stemmed lexemes, so a
// partial match still ranks and ts_rank_cd — which rewards term density and
// proximity — does the discriminating. ftsRankFloor then drops the long tail of
// one-common-word matches that OR semantics would otherwise admit.
const publicFTSCTE = `
fts AS (
  SELECT chunk_id,
         row_number() OVER (ORDER BY rank_cd DESC, chunk_id) AS rnk,
         rank_cd AS score,
         'fts'::text AS arm
    FROM (
      SELECT c.id AS chunk_id,
             ts_rank_cd(c.search_vector, ` + ftsQueryExpr + `)::float8 AS rank_cd
        FROM legal_chunks c
        JOIN legal_versions v ON v.id = c.legal_version_id
        JOIN legal_documents d ON d.id = v.legal_document_id
       WHERE c.search_vector @@ ` + ftsQueryExpr + `
         %[1]s
       ORDER BY rank_cd DESC, c.id
       LIMIT @arm_depth
    ) t
   WHERE rank_cd >= @fts_floor
)`

// privateFTSQuery is ftsQueryExpr for the private dossier arm, which is built
// through the gorm query builder and therefore needs a positional placeholder
// rather than a named one. Same reasoning: a lawyer's phrasing must not have to
// match every stored lexeme for the arm to return anything.
const privateFTSQuery = `(SELECT string_agg(l.lexeme, ' | ')::tsquery FROM unnest(to_tsvector('french', unaccent(?))) AS l)`

// ftsQueryExpr is the OR-of-lexemes tsquery described on publicFTSCTE. It
// stems the query through the same 'french' configuration and unaccent used at
// write time, then joins the resulting lexemes with '|'. A query that stems to
// nothing (punctuation, stopwords only) yields NULL, and `@@ NULL` is NULL, so
// the arm returns no rows rather than erroring.
const ftsQueryExpr = `(
           SELECT string_agg(l.lexeme, ' | ')::tsquery
             FROM unnest(to_tsvector('french', unaccent(@q ))) AS l
         )`

// publicFuseTail is the fusion itself: score = sum(1 / (k + rank)) over the
// arms a chunk appeared in. No normalisation, no weighting — ranks only.
const publicFuseTail = `
fused AS (
  SELECT chunk_id,
         SUM((CASE arm WHEN 'vector' THEN CAST(@w_vector AS float8) ELSE CAST(@w_fts AS float8) END) / (@rrf_k + rnk))::float8 AS rrf_score,
         COALESCE(MAX(rnk) FILTER (WHERE arm = 'vector'), 0) AS vector_rank,
         COALESCE(MAX(rnk) FILTER (WHERE arm = 'fts'), 0) AS fts_rank,
         COALESCE(MAX(score) FILTER (WHERE arm = 'vector'), 0)::float8 AS vector_score,
         COALESCE(MAX(score) FILTER (WHERE arm = 'fts'), 0)::float8 AS fts_score
    FROM arms
   GROUP BY chunk_id
)
SELECT c.id AS chunk_id,
       c.text AS text,
       c.legal_version_id AS legal_version_id,
       v.valid_from AS valid_from,
       v.valid_to AS valid_to,
       v.decision_date AS decision_date,
       v.formation AS formation,
       v.publication_level AS publication_level,
       v.solution AS solution,
       v.ecli AS ecli,
       v.state AS version_state,
       COALESCE(v.source_meta ->> 'pourvoi', v.source_meta ->> 'numero_pourvoi', '') AS pourvoi,
       d.source AS source,
       d.kind AS kind,
       d.jurisdiction AS jurisdiction,
       d.canonical_url AS canonical_url,
       d.citation_label AS citation_label,
       d.license_code AS license_code,
       f.rrf_score AS rrf_score,
       f.vector_rank AS vector_rank,
       f.fts_rank AS fts_rank,
       f.vector_score AS vector_score,
       f.fts_score AS fts_score
  FROM fused f
  JOIN legal_chunks c ON c.id = f.chunk_id
  JOIN legal_versions v ON v.id = c.legal_version_id
  JOIN legal_documents d ON d.id = v.legal_document_id
 ORDER BY f.rrf_score DESC, c.id
 LIMIT @fuse_depth`

func publicCandidates(ctx context.Context, query string, qv *pgvector.HalfVector, opts RetrievalOptions) ([]Hit, error) {
	args := map[string]any{
		"q":          query,
		"arm_depth":  opts.ArmDepth,
		"fuse_depth": opts.FuseDepth,
		"rrf_k":      opts.RRFK,
		"fts_floor":  ftsRankFloor,
		"w_vector":   opts.ArmWeightVector,
		"w_fts":      opts.ArmWeightFTS,
	}
	filter := publicFilterSQL(opts, args)

	var ctes []string
	var arms []string
	if opts.usesVector() && qv != nil {
		args["qv"] = *qv
		ctes = append(ctes, fmt.Sprintf(publicVectorCTE, filter))
		arms = append(arms, "SELECT chunk_id, rnk, score, arm FROM vec")
	}
	if opts.usesFTS() {
		ctes = append(ctes, fmt.Sprintf(publicFTSCTE, filter))
		arms = append(arms, "SELECT chunk_id, rnk, score, arm FROM fts")
	}
	if len(arms) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownStrategy, opts.Strategy)
	}

	sqlText := "WITH " + strings.Join(ctes, ",\n") +
		",\narms AS (\n  " + strings.Join(arms, "\n  UNION ALL\n  ") + "\n),\n" +
		strings.TrimPrefix(publicFuseTail, "\n")

	rows := []publicRow{}
	db := u.DB.WithContext(ctx)
	var err error
	if opts.usesVector() {
		// hnsw.ef_search is a GUC, so it can only be scoped to a statement by
		// scoping it to a transaction. Without it the index returns at most
		// 40 rows and a 50-deep arm is silently truncated — a retrieval bug
		// that looks like a relevance problem.
		err = db.Transaction(func(tx *gorm.DB) error {
			if e := tx.Exec(fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", opts.EfSearch)).Error; e != nil {
				return e
			}
			return tx.Raw(sqlText, args).Scan(&rows).Error
		})
	} else {
		err = db.Raw(sqlText, args).Scan(&rows).Error
	}
	if err != nil {
		retrievalLog.Errorf("Public retrieval failed (strategy=%s): %v", opts.Strategy, err)
		return nil, err
	}

	hits := make([]Hit, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, Hit{
			SearchHit: types.SearchHit{
				Corpus:         types.CorpusPublic,
				ChunkID:        r.ChunkID,
				Text:           r.Text,
				Score:          r.RRFScore,
				LegalVersionID: r.LegalVersionID,
				CitationLabel:  r.CitationLabel,
				CanonicalURL:   r.CanonicalURL,
				LicenseCode:    r.LicenseCode,
				ValidFrom:      r.ValidFrom,
				ValidTo:        r.ValidTo,
			},
			Source:           r.Source,
			Kind:             r.Kind,
			Jurisdiction:     r.Jurisdiction,
			Formation:        r.Formation,
			PublicationLevel: r.PublicationLevel,
			Solution:         r.Solution,
			DecisionDate:     r.DecisionDate,
			ECLI:             r.ECLI,
			Pourvoi:          r.Pourvoi,
			VersionState:     r.VersionState,
			RRFScore:         r.RRFScore,
			VectorRank:       r.VectorRank,
			FTSRank:          r.FTSRank,
			VectorScore:      r.VectorScore,
			FTSScore:         r.FTSScore,
		})
	}
	retrievalLog.Debugf("Public arms fused (strategy=%s candidates=%d arm_depth=%d ef_search=%d)",
		opts.Strategy, len(hits), opts.ArmDepth, opts.EfSearch)
	return hits, nil
}

// publicFilterSQL is the temporal + provenance predicate, shared by both arms.
//
// The applicable rule is the one in force at the date of the FACTS (§6.4), so
// the interval is half-open — valid_from <= d AND (valid_to IS NULL OR
// d < valid_to) — and a version whose interval is NULL (a decision has no
// interval; its authority is contextual) passes.
func publicFilterSQL(opts RetrievalOptions, args map[string]any) string {
	var b strings.Builder
	b.WriteString(" AND (v.valid_from IS NULL OR v.valid_from <= @as_of )")
	b.WriteString("\n     AND (v.valid_to IS NULL OR @as_of < v.valid_to )")
	args["as_of"] = opts.AsOfDate

	if len(opts.States) > 0 {
		b.WriteString("\n     AND v.state IN (" + namedList("state", opts.States, args) + ")")
	}
	if len(opts.Sources) > 0 {
		b.WriteString("\n     AND d.source IN (" + namedList("source", opts.Sources, args) + ")")
	}
	if len(opts.Kinds) > 0 {
		b.WriteString("\n     AND d.kind IN (" + namedList("kind", opts.Kinds, args) + ")")
	}
	return b.String()
}

// namedList expands a slice into individually named placeholders. GORM's
// named-argument builder binds one value per name and does not expand a slice
// inside IN (…), so the names are generated instead.
func namedList(prefix string, values []string, args map[string]any) string {
	parts := make([]string, 0, len(values))
	for i, v := range values {
		name := fmt.Sprintf("%s_%d", prefix, i)
		args[name] = v
		parts = append(parts, "@"+name+" ")
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Private corpus — tenant filter first, then exact scoring
// ---------------------------------------------------------------------------

type privateRow struct {
	ChunkID  uint64 `gorm:"column:id"`
	PieceID  string
	PageFrom int
	PageTo   int
	Text     string
	Score    float64
}

// privateCandidates scores a dossier's chunks. Both arms are separate
// statements here — unlike the public path — because each one is built from
// the tenant-scoped repository, which writes the tenant predicate itself and
// offers no unscoped handle. Statement count is negotiable; the predicate is
// not, and a hand-written cross-tenant CTE is exactly the missed clause §6.1
// warns about. The tables involved are small by construction.
func privateCandidates(ctx context.Context, repo *u.TenantRepo, dossierID string, pieceIDs []string, query string, qv *pgvector.HalfVector, opts RetrievalOptions) ([]Hit, error) {
	if repo == nil || dossierID == "" {
		return nil, ErrPrivateNeedsDossier
	}
	if pieceIDs != nil && len(pieceIDs) == 0 {
		return nil, nil
	}
	// scope is applied to BOTH arms, after the tenant and dossier predicates.
	scope := func(q *gorm.DB) *gorm.DB {
		q = q.Where("dossier_id = ?", dossierID)
		if pieceIDs != nil {
			q = q.Where("piece_id IN ?", pieceIDs)
		}
		return q
	}

	ranked := map[uint64]*Hit{}
	order := []uint64{}

	add := func(rows []privateRow, arm string) {
		for i, r := range rows {
			rank := i + 1
			h, ok := ranked[r.ChunkID]
			if !ok {
				h = &Hit{SearchHit: types.SearchHit{
					Corpus:   types.CorpusPrivate,
					ChunkID:  r.ChunkID,
					Text:     r.Text,
					PieceID:  r.PieceID,
					PageFrom: r.PageFrom,
					PageTo:   r.PageTo,
				}}
				ranked[r.ChunkID] = h
				order = append(order, r.ChunkID)
			}
			h.RRFScore += 1.0 / (opts.RRFK + float64(rank))
			switch arm {
			case StrategyVector:
				h.VectorRank, h.VectorScore = rank, r.Score
			case StrategyFTS:
				h.FTSRank, h.FTSScore = rank, r.Score
			}
		}
	}

	if opts.usesVector() && qv != nil {
		rows := []privateRow{}
		// No HNSW on piece_chunks, on purpose (§6.1): the tenant + dossier
		// predicate runs FIRST and what remains is scored exactly.
		if err := scope(repo.PieceChunks().
			Select("id, piece_id, page_from, page_to, text, (1 - (embedding <=> CAST(? AS halfvec)))::float8 AS score", *qv)).
			Where("embedding IS NOT NULL").
			Order("score DESC, id").
			Limit(opts.ArmDepth).
			Scan(&rows).Error; err != nil {
			retrievalLog.Errorf("Private vector scan failed (dossier=%s): %v", dossierID, err)
			return nil, err
		}
		add(rows, StrategyVector)
	}

	if opts.usesFTS() {
		rows := []privateRow{}
		if err := scope(repo.PieceChunks().
			Select("id, piece_id, page_from, page_to, text, ts_rank_cd(search_vector, "+privateFTSQuery+")::float8 AS score", query)).
			Where("search_vector @@ "+privateFTSQuery, query).
			Order("score DESC, id").
			Limit(opts.ArmDepth).
			Scan(&rows).Error; err != nil {
			retrievalLog.Errorf("Private lexical scan failed (dossier=%s): %v", dossierID, err)
			return nil, err
		}
		add(rows, StrategyFTS)
	}

	hits := make([]Hit, 0, len(order))
	for _, id := range order {
		h := ranked[id]
		h.Score = h.RRFScore
		hits = append(hits, *h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].RRFScore > hits[j].RRFScore })
	if len(hits) > opts.FuseDepth {
		hits = hits[:opts.FuseDepth]
	}
	retrievalLog.Debugf("Private candidates scored (dossier=%s tenant=%s candidates=%d)",
		dossierID, repo.TenantID(), len(hits))
	return hits, nil
}

// ---------------------------------------------------------------------------
// Rerank
// ---------------------------------------------------------------------------

// rerankCandidates reorders the fused candidates with the cross-encoder and
// stamps Score / RerankRank. It optimises TOPICAL relevance and says nothing
// about precedential direction (§6.3).
func rerankCandidates(ctx context.Context, query string, candidates []Hit, opts RetrievalOptions) ([]Hit, error) {
	docs := make([]string, len(candidates))
	for i, h := range candidates {
		docs[i] = h.Text
	}
	scored, err := u.Rerank(ctx, opts.RerankModel, query, docs, opts.TopK)
	if err != nil {
		return nil, err
	}
	if len(scored) == 0 {
		return nil, errors.New("rerank returned no results")
	}
	out := make([]Hit, 0, len(scored))
	for i, s := range scored {
		h := candidates[s.Index]
		h.Score = s.Score
		h.RerankRank = i + 1
		out = append(out, h)
	}
	return out, nil
}
