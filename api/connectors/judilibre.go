package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var judilibreLog = u.NewLogger("connectors.judilibre")

// Judilibre is the Cour de cassation's open case-law API on PISTE (§5.1):
// /search, /decision, /export and /taxonomy. Licence Ouverte 2.0 — commercial
// reuse is permitted WITH ATTRIBUTION, and the licence does not lift
// personal-data or third-party-IP obligations.
//
// JUDILIBRE'S OWN TERMS DISCLAIM COMPLETENESS, FRESHNESS AND RELIABILITY. It
// is a LEAD SOURCE, NOT AN AUTHORITY OF RECORD (§5.1). Concretely, for this
// codebase that means three things:
//
//   - what we index is a cache, and a citation is only trustworthy once it has
//     been revalidated against the official source before export (§6.4);
//   - an absent decision is not evidence that no such decision exists, so the
//     product must never present a Judilibre result set as exhaustive;
//   - the metadata this connector records — publication level, solution,
//     formation — is what actually carries precedential weight, because RRF and
//     reranking detect topical similarity and NEVER a revirement (§6.3).
//
// Two hard product bans that constrain what may be built on this connector
// (§13): no judge ranking, judge-outcome prediction or any analytics whose
// object or effect is the comparison of named magistrates (COJ art. L. 111-13
// sanctions reusing judges' and clerks' identity data for exactly that), and
// no feature intended to reverse the pseudonymisation or enrich party
// identities.
type Judilibre struct {
	piste   *piste
	baseURL string
}

// NewJudilibre builds the client. Empty credentials still return a usable
// value whose calls report ErrJudilibreNotConfigured → 503
// {"detail": "Judilibre (PISTE) not configured"}.
func NewJudilibre(o Options) *Judilibre {
	base := o.BaseURL
	if base == "" {
		base = DefaultJudilibreBaseURL
	}
	return &Judilibre{piste: newPiste(o, "openid"), baseURL: strings.TrimRight(base, "/")}
}

// Configured reports whether PISTE credentials are present.
func (j *Judilibre) Configured() bool { return j.piste.configured() }

// DecisionQuery scopes a /search or /export call. The MVP scope of §5.3 is
// Cass. chambre sociale, published, 2018→ — Jurisdiction "cc", Chamber "soc",
// Publication a published level. Leaving those empty is an unscoped fetch of
// the whole corpus, which §5.3 says will not fit; callers set them.
type DecisionQuery struct {
	Query        string
	Jurisdiction []string
	Chamber      []string
	// Publication filters on the publication level ("b", "r", "l", "c"),
	// which is the strongest cheap proxy for authority.
	Publication []string
	DateStart   time.Time
	DateEnd     time.Time
	PageSize    int
	Page        int
	// ResolveReferences asks the API for the visa / rapprochements blocks —
	// the source-provided relation metadata §6.3 needs, and the only kind of
	// relation that is provenance "source" rather than an LLM lead.
	ResolveReferences bool
}

// Decision is one pseudonymised decision, carrying every field §6.4 requires
// for a citable case-law version: decision date, publication date,
// jurisdiction, formation, publication level, solution, ECLI and pourvoi
// number, plus the source revision the version was read at.
//
// A decision has NO validity interval — unlike legislation, its authority is
// contextual, not an interval — which is why legal_versions leaves
// valid_from/valid_to null for these and relies on this metadata instead.
type Decision struct {
	ID           string   `json:"id"`
	Source       string   `json:"source"`
	Jurisdiction string   `json:"jurisdiction"`
	Chamber      string   `json:"chamber"`
	Formation    string   `json:"formation"`
	Number       string   `json:"number"`
	Numbers      []string `json:"numbers"`
	ECLI         string   `json:"ecli"`
	Publication  []string `json:"publication"`
	Solution     string   `json:"solution"`
	SolutionAlt  string   `json:"solution_alt"`
	Type         string   `json:"type"`
	Text         string   `json:"text"`
	Summary      string   `json:"summary"`
	Themes       []string `json:"themes"`
	Visa         []struct {
		Title string `json:"title"`
	} `json:"visa"`
	Rapprochements []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"rapprochements"`

	// DecisionDate and UpdateDate are ISO dates as the API returns them.
	DecisionDate string `json:"decision_date"`
	// PublicationDate is when the decision entered the open dataset. It is
	// NOT the decision date and the two must never be conflated (§6.4).
	PublicationDate string `json:"publication_date"`
	UpdateDate      string `json:"update_date"`
}

// SourceRevision is the value recorded as legal_versions.source_version. It is
// the API's own revision marker (update_date), falling back to the decision
// date, so re-ingesting an unchanged decision converges on the same row via
// uniq(legal_document_id, source_version) instead of duplicating it.
func (d Decision) SourceRevision() string {
	for _, v := range []string{d.UpdateDate, d.PublicationDate, d.DecisionDate} {
		if v != "" {
			return v
		}
	}
	return d.ID
}

// PublicationLevel is the highest publication marker present, in decreasing
// authority: "r" (Rapport annuel) > "b" (Bulletin) > "l" (Lettre de chambre) >
// "c" (Communiqué) > "n" (non publié). It is metadata, never a claim about
// whether the decision is still good law.
func (d Decision) PublicationLevel() string {
	rank := map[string]int{"r": 4, "b": 3, "l": 2, "c": 1}
	// The export returns the French labels ("Publié au Bulletin"), the search
	// filters take the codes ("b"). Both are accepted here: reading only the
	// codes marked every real decision "non publié", which is the one piece of
	// metadata that stands in for authority.
	labels := []struct {
		prefix string
		code   string
	}{
		{"publié au rapport", "r"},
		{"publie au rapport", "r"},
		{"publié au bulletin", "b"},
		{"publie au bulletin", "b"},
		{"publié aux lettres", "l"},
		{"publie aux lettres", "l"},
		{"communiqué", "c"},
		{"communique", "c"},
	}

	best, bestRank := "n", 0
	consider := func(code string) {
		if r, ok := rank[code]; ok && r > bestRank {
			best, bestRank = code, r
		}
	}
	for _, p := range d.Publication {
		p = strings.ToLower(strings.TrimSpace(p))
		if _, ok := rank[p]; ok {
			consider(p)
			continue
		}
		for _, l := range labels {
			if strings.HasPrefix(p, l.prefix) {
				consider(l.code)
				break
			}
		}
	}
	return best
}

// Pourvoi is the pourvoi number a lawyer would cite ("21-11.111").
func (d Decision) Pourvoi() string {
	if d.Number != "" {
		return d.Number
	}
	if len(d.Numbers) > 0 {
		return d.Numbers[0]
	}
	return ""
}

// DecidedAt parses DecisionDate into unix seconds, or nil when absent.
func (d Decision) DecidedAt() *int64 { return parseISODate(d.DecisionDate) }

// PublishedAt parses PublicationDate into unix seconds, or nil when absent.
func (d Decision) PublishedAt() *int64 { return parseISODate(d.PublicationDate) }

func parseISODate(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			sec := t.UTC().Unix()
			return &sec
		}
	}
	return nil
}

// DecisionURL is where the decision can be read in public. Judilibre is an
// API over the Cour de cassation's open data, and the decision pages are the
// authoritative public rendering of what it returns — a citation the lawyer
// cannot open is a citation they cannot check (§6.4).
func DecisionURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://www.courdecassation.fr/decision/" + id
}

// SearchResponse is the trimmed GET /search answer.
type DecisionSearchResponse struct {
	Total   int        `json:"total"`
	Page    int        `json:"page"`
	Results []Decision `json:"results"`
}

// ExportBatch is one page of GET /export. NextBatch is the resumable cursor:
// it is what corpus_syncs.cursor stores, so a sync killed mid-run picks up
// where it stopped instead of restarting (§4.3).
//
// The API returns next_batch as the WHOLE query string of the next page
// ("jurisdiction=cc&…&batch=1&…"), not as a number — hence the string, and
// hence NextBatchNumber for callers that only want the position.
type ExportBatch struct {
	Batch     int        `json:"batch"`
	NextBatch string     `json:"next_batch"`
	Total     int        `json:"total"`
	Results   []Decision `json:"results"`
}

// NextBatchNumber extracts the batch position from NextBatch. It reports false
// when the export is exhausted or the cursor cannot be read, which the caller
// must treat as "done" rather than as "start again from zero".
func (b ExportBatch) NextBatchNumber() (int, bool) {
	if strings.TrimSpace(b.NextBatch) == "" {
		return 0, false
	}
	q, err := url.ParseQuery(b.NextBatch)
	if err != nil {
		judilibreLog.Warnf("Judilibre next_batch is not a query string: %q", b.NextBatch)
		return 0, false
	}
	n, err := strconv.Atoi(q.Get("batch"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Search runs GET /search. Use it for a targeted lookup; use Export for the
// scoped bulk sync — /search is capped and paginating it over a whole corpus
// is what /export exists to avoid.
func (j *Judilibre) Search(ctx context.Context, q DecisionQuery) (DecisionSearchResponse, error) {
	if !j.Configured() {
		return DecisionSearchResponse{}, ErrJudilibreNotConfigured
	}
	if strings.TrimSpace(q.Query) == "" {
		return DecisionSearchResponse{}, errors.New("judilibre: empty query")
	}
	v := q.values()
	v.Set("query", q.Query)
	if q.PageSize > 0 {
		v.Set("page_size", strconv.Itoa(q.PageSize))
	}
	if q.Page > 0 {
		v.Set("page", strconv.Itoa(q.Page))
	}

	var resp DecisionSearchResponse
	if err := j.piste.call(ctx, http.MethodGet, j.baseURL+"/search?"+v.Encode(), nil, &resp); err != nil {
		return DecisionSearchResponse{}, err
	}
	judilibreLog.Infof("Judilibre search returned %d/%d decisions (page=%d)", len(resp.Results), resp.Total, resp.Page)
	return resp, nil
}

// Decision runs GET /decision for one id, returning the full text and the
// source-provided visa / rapprochements metadata.
func (j *Judilibre) Decision(ctx context.Context, id string) (Decision, error) {
	if !j.Configured() {
		return Decision{}, ErrJudilibreNotConfigured
	}
	if strings.TrimSpace(id) == "" {
		return Decision{}, errors.New("judilibre: empty decision id")
	}
	v := url.Values{"id": {id}, "resolve_references": {"true"}}
	var d Decision
	if err := j.piste.call(ctx, http.MethodGet, j.baseURL+"/decision?"+v.Encode(), nil, &d); err != nil {
		return Decision{}, err
	}
	if d.ID == "" {
		return Decision{}, fmt.Errorf("judilibre: decision %s not found", id)
	}
	judilibreLog.Debugf("Judilibre decision fetched (id=%s chamber=%s publication=%s length=%d)",
		d.ID, d.Chamber, d.PublicationLevel(), len(d.Text))
	return d, nil
}

// Export runs GET /export, the paginated bulk path used by the scoped sync.
//
// batch is the page index; pass the previous batch's NextBatch to resume. It
// is deliberately the ONLY bulk entry point: §5.3 scopes the MVP corpus to
// published chambre sociale decisions from 2018, and an unscoped export of the
// full Judilibre corpus is the 80–150 GB the plan rules out.
func (j *Judilibre) Export(ctx context.Context, q DecisionQuery, batch int) (ExportBatch, error) {
	if !j.Configured() {
		return ExportBatch{}, ErrJudilibreNotConfigured
	}
	v := q.values()
	size := q.PageSize
	if size <= 0 {
		size = 100
	}
	v.Set("batch_size", strconv.Itoa(size))
	// `batch` is REQUIRED, not optional: omitting it on the first call is a
	// 400 ("Value of the batch parameter must be an integer greater or equal
	// than 0"), so the first page is batch 0 and not "no batch".
	if batch < 0 {
		batch = 0
	}
	v.Set("batch", strconv.Itoa(batch))

	var out ExportBatch
	if err := j.piste.call(ctx, http.MethodGet, j.baseURL+"/export?"+v.Encode(), nil, &out); err != nil {
		return ExportBatch{}, err
	}
	next, more := out.NextBatchNumber()
	if !more {
		next = -1
	}
	judilibreLog.Infof("Judilibre export batch %d: %d decisions (total=%d next_batch=%d)",
		out.Batch, len(out.Results), out.Total, next)
	return out, nil
}

// Taxonomy runs GET /taxonomy, which enumerates the API's controlled
// vocabularies (chambers, formations, publication levels, solutions). The
// loader uses it to validate a scope before a long export rather than
// discovering a typo after 40 000 rows.
func (j *Judilibre) Taxonomy(ctx context.Context, id, contextValue string) (map[string]any, error) {
	if !j.Configured() {
		return nil, ErrJudilibreNotConfigured
	}
	v := url.Values{}
	if id != "" {
		v.Set("id", id)
	}
	if contextValue != "" {
		v.Set("context_value", contextValue)
	}
	var out map[string]any
	if err := j.piste.call(ctx, http.MethodGet, j.baseURL+"/taxonomy?"+v.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (q DecisionQuery) values() url.Values {
	v := url.Values{}
	for _, s := range q.Jurisdiction {
		v.Add("jurisdiction", s)
	}
	for _, s := range q.Chamber {
		v.Add("chamber", s)
	}
	for _, s := range q.Publication {
		v.Add("publication", s)
	}
	if !q.DateStart.IsZero() {
		v.Set("date_start", q.DateStart.Format("2006-01-02"))
	}
	if !q.DateEnd.IsZero() {
		v.Set("date_end", q.DateEnd.Format("2006-01-02"))
	}
	if q.ResolveReferences {
		v.Set("resolve_references", "true")
	}
	return v
}

// EmploymentScope is the §5.3 MVP case-law scope: published Cour de cassation
// chambre sociale decisions from 2018 onward. Landmark older decisions are
// added by curation, not by widening this window — and no cours d'appel bulk,
// which multiplies volume for weaker, noisier authority.
func EmploymentScope() DecisionQuery {
	return DecisionQuery{
		Jurisdiction:      []string{"cc"},
		Chamber:           []string{"soc"},
		Publication:       []string{"b", "r", "l", "c"},
		DateStart:         time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
		PageSize:          100,
		ResolveReferences: true,
	}
}
