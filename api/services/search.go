package services

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var searchLog = u.NewLogger("services.search")

// SearchResponseBody is what POST /api/search returns.
//
// It is a superset of types.SearchResponse on the wire — same `results` and
// `reranked` keys, same per-hit keys — plus three things the Sources panel
// cannot work without: which strategy actually ran, the as-of date the hits
// were filtered against, and the source/licence attribution. Licence Ouverte
// 2.0 permits commercial reuse WITH attribution, so attribution is a product
// requirement, not a footer (§13).
type SearchResponseBody struct {
	Results  []Hit  `json:"results"`
	Reranked bool   `json:"reranked"`
	Strategy string `json:"strategy"`
	Corpus   string `json:"corpus"`
	// AsOfDate is the date the applicable text was judged in force at — the
	// date of the facts, not today (§6.4).
	AsOfDate int64 `json:"as_of_date"`
	// Candidates is how many fused candidates the ranking was drawn from.
	Candidates  int                 `json:"candidates"`
	Attribution []SourceAttribution `json:"attribution"`
}

// SourceAttribution is one visible source credit. Nomos indexes a CACHE: DILA is
// explicit that the opposable text is the authenticated Légifrance
// publication, and Judilibre disclaims completeness and freshness (§5.1), so
// the notice says where the text came from rather than implying it is the
// authority of record.
type SourceAttribution struct {
	Source      string `json:"source"`
	LicenseCode string `json:"license_code,omitempty"`
	Notice      string `json:"notice"`
	Hits        int    `json:"hits"`
}

// Search runs hybrid retrieval (docs/PLAN.md §6.2).
//
// PUBLIC corpus, per corpus in ONE statement: vector top-50 (HNSW,
// halfvec_cosine_ops) + websearch_to_tsquery('french', …) top-50, fused by RRF
// (1/(60+rank), NO score normalisation), then rerank the top ~40 with the
// configured rerank model and keep 8–12. unaccent + the built-in `french`
// configuration handle morphology.
//
// PRIVATE corpus: NO HNSW. Filter on tenant_id + dossier_id, then score
// exactly. A dossier is hundreds to a few thousand chunks, so exact is fast,
// and it avoids the multitenant ANN failure where HNSW returns globally-near
// neighbours the tenant filter then throws away (§6.1). A private search
// without a dossier scope is a bug, not a broad search — reject it.
//
// Two things this endpoint MUST NOT claim (§6.3): RRF is a candidate
// generator and a reranker optimises topical relevance. Neither establishes
// whether a decision was overruled, distinguished, limited or superseded.
// Surface candidates with their jurisdiction, formation, publication level,
// solution and dates; never assert that the law changed. And when the rerank
// pass could not run, say so in SearchResponse.Reranked rather than passing
// RRF order off as reranked.
//
// The strategy is switchable per request with ?strategy=vector|fts|rrf|
// rrf_rerank so the relevance harness scores the SAME code path the product
// runs. Default: rrf_rerank.
func Search(c *gin.Context, body types.SearchRequest) (int, any) {
	query := strings.TrimSpace(body.Query)
	if query == "" {
		return http.StatusBadRequest, gin.H{"detail": "Query is required"}
	}

	corpus := strings.TrimSpace(body.Corpus)
	if corpus == "" {
		corpus = types.CorpusPublic
	}
	if corpus != types.CorpusPublic && corpus != types.CorpusPrivate && corpus != types.CorpusBoth {
		return http.StatusBadRequest, gin.H{"detail": "Corpus must be public, private or both"}
	}

	opts := DefaultRetrievalOptions()
	strategy, err := ParseStrategy(c.Query("strategy"))
	if err != nil {
		return http.StatusBadRequest, gin.H{"detail": "Unknown strategy: " + c.Query("strategy")}
	}
	opts.Strategy = strategy
	opts.AsOfDate = body.AsOfDate
	if body.Limit > 0 {
		opts.TopK = body.Limit
	}

	req := RetrievalRequest{Query: query, Corpus: corpus, Options: opts}

	// The private corpus is reachable only through the tenant-scoped
	// repository, and only for a dossier this caller may read: resolving the
	// dossier through the tenant predicate is what makes another firm's
	// dossier id a 404 instead of a leak.
	if corpus != types.CorpusPublic {
		repo, status, errBody := tenantRepo(c)
		if repo == nil {
			return status, errBody
		}
		if body.DossierID == nil || strings.TrimSpace(*body.DossierID) == "" {
			return http.StatusBadRequest, gin.H{"detail": "A private search must name a dossier"}
		}
		d, status, errBody := loadDossier(repo, strings.TrimSpace(*body.DossierID))
		if d == nil {
			return status, errBody
		}
		if !canAccessDossier(c, repo, *d, types.ActionRead) {
			return http.StatusForbidden, gin.H{"detail": "Forbidden"}
		}
		req.Repo, req.DossierID = repo, d.ID
	}

	res, err := Retrieve(c.Request.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, u.ErrLLMNotConfigured):
			return http.StatusServiceUnavailable, gin.H{"detail": "LLM gateway not configured"}
		case errors.Is(err, u.ErrAuthNotConfigured):
			return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
		case errors.Is(err, ErrPrivateNeedsDossier):
			return http.StatusBadRequest, gin.H{"detail": "A private search must name a dossier"}
		case errors.Is(err, ErrEmptyQuery), errors.Is(err, ErrUnknownStrategy), errors.Is(err, ErrUnknownCorpus):
			return http.StatusBadRequest, gin.H{"detail": err.Error()}
		}
		searchLog.Errorf("Search failed (corpus=%s strategy=%s): %v", corpus, opts.Strategy, err)
		return http.StatusBadGateway, gin.H{"detail": "Search failed"}
	}

	searchLog.Infof("Search served (corpus=%s strategy=%s hits=%d reranked=%t query_length=%d)",
		corpus, res.Strategy, len(res.Hits), res.Reranked, len(query))

	return http.StatusOK, SearchResponseBody{
		Results:     res.Hits,
		Reranked:    res.Reranked,
		Strategy:    res.Strategy,
		Corpus:      corpus,
		AsOfDate:    res.AsOfDate,
		Candidates:  res.Candidates,
		Attribution: sourceAttributionFor(res.Hits),
	}
}

// sourceLabels name the open-data sources in the words their licences use.
var sourceLabels = map[string]string{
	types.SourceLegifrance: "Légifrance (DILA)",
	types.SourceJudilibre:  "Judilibre (Cour de cassation)",
	types.SourceDILA:       "DILA — données ouvertes",
	types.SourceLegiHF:     "Jeu de données LEGI (AgentPublic/legi)",
}

// licenseNotices carry the attribution the licence actually requires.
var licenseNotices = map[string]string{
	"LO-2.0":     "Licence Ouverte 2.0 (Etalab) — réutilisation autorisée avec mention de la source",
	"LO-1.0":     "Licence Ouverte 1.0 (Etalab) — réutilisation autorisée avec mention de la source",
	"ETALAB-2.0": "Licence Ouverte 2.0 (Etalab) — réutilisation autorisée avec mention de la source",
}

// cacheCaveat is the line that keeps a cached copy from being mistaken for the
// authority of record (§5.1).
const cacheCaveat = " — copie indexée ; le texte opposable est la publication authentifiée de la source"

// sourceAttributionFor builds one credit per (source, licence) present in the hits.
func sourceAttributionFor(hits []Hit) []SourceAttribution {
	type key struct{ source, license string }
	counts := map[key]int{}
	for _, h := range hits {
		if h.Corpus != types.CorpusPublic || h.Source == "" {
			continue
		}
		counts[key{h.Source, h.LicenseCode}]++
	}
	out := make([]SourceAttribution, 0, len(counts))
	for k, n := range counts {
		label := sourceLabels[k.source]
		if label == "" {
			label = k.source
		}
		notice := licenseNotices[strings.ToUpper(k.license)]
		if notice == "" {
			notice = "Réutilisation soumise à la licence de la source"
		}
		out = append(out, SourceAttribution{
			Source:      label,
			LicenseCode: k.license,
			Notice:      notice + cacheCaveat,
			Hits:        n,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].Source < out[j].Source
	})
	return out
}
