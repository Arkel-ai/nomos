// Tests for the legal-corpus connectors.
//
// Everything here runs OFFLINE against recorded JSON served by an
// httptest.Server, because that is the whole point of the injectable Options:
// a corpus connector that can only be tested with production credentials is a
// connector nobody tests. The one live-network test is opt-in behind
// NOMOS_NETWORK_TESTS=1 (or NOMOS_LIVE=1), and excluded under NOMOS_LIVE=0.
package connectors_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/fixtures"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
)

// ---------------------------------------------------------------------------
// Recorded fixtures. Trimmed to the fields the connectors read.
// ---------------------------------------------------------------------------

const tokenJSON = `{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`

// Captured from the PISTE sandbox, trimmed to the fields the connector reads.
// The shape is the point: /search paginates by TEXT VERSION, so the LEGIARTI
// ids live under results[].sections[].extracts[], the two results are the same
// code at two consolidation dates, and they repeat an article between them —
// which is what flatten() has to dedupe.
const legifranceSearchJSON = `{
  "totalResultNumber": 1942,
  "typePagination": "DEFAUT",
  "results": [
    {
      "titles": [{"id":"LEGITEXT000006072050_27-04-2022","cid":"LEGITEXT000006072050","title":"Code du travail"}],
      "nature": "code", "etat": "VIGUEUR", "origin": "LEGI",
      "sections": [
        {"id":"LEGISCTA000036212575","title":"Section 3 : Notification du <mark>licenciement</mark>","legalStatus":"VIGUEUR",
         "extracts":[{"id":"LEGIARTI000036212577","title":"R1232-13","num":"R1232-13","legalStatus":"VIGUEUR"}]},
        {"id":"LEGISCTA000006198534","title":"Paragraphe 3 : <mark>Licenciements</mark> successifs.","legalStatus":"VIGUEUR",
         "extracts":[{"id":"LEGIARTI000035643922","title":"L1233-26","num":"L1233-26","legalStatus":"VIGUEUR"},
                     {"id":"LEGIARTI000035643913","title":"L1233-27","num":"L1233-27","legalStatus":"VIGUEUR"}]}
      ]
    },
    {
      "titles": [{"id":"LEGITEXT000006072050_28-04-2022","cid":"LEGITEXT000006072050","title":"Code du travail"}],
      "nature": "code", "etat": "VIGUEUR", "origin": "LEGI",
      "sections": [
        {"id":"LEGISCTA000036212575","title":"Section 3 : Notification du <mark>licenciement</mark>","legalStatus":"VIGUEUR",
         "extracts":[{"id":"LEGIARTI000036212577","title":"R1232-13","num":"R1232-13","legalStatus":"VIGUEUR"}]}
      ]
    }
  ]
}`

// Captured from the PISTE sandbox and cut down to two sections. It keeps what
// the parser has to survive: nesting, an article body that is HTML rather than
// text, a table (the indemnity scales are tables), padded pathTitle entries,
// and LEGI's 2999-01-01 "no end date" sentinel.
const legifrancePartJSON = `{
  "id": "LEGITEXT000006072050_19-08-2026",
  "cid": "LEGITEXT000006072050",
  "title": "Code du travail",
  "modifDate": "2026-08-19",
  "sections": [
    {
      "title": "Partie l\u00e9gislative",
      "articles": [],
      "sections": [
        {
          "title": "Chapitre II : Licenciement pour motif personnel",
          "articles": [
            {"id":"LEGIARTI000019071126","cid":"LEGIARTI000006901109","num":"L1232-1","etat":"VIGUEUR",
             "pathTitle":["Partie l\u00e9gislative  ","Chapitre II : Licenciement pour motif personnel ",""],
             "content":"<p>Tout licenciement pour motif personnel est motiv\u00e9.</p><p>Il est justifi\u00e9 par une cause r\u00e9elle et s\u00e9rieuse.</p>",
             "nota":"<p>Une note.</p>","dateDebut":1214524800000,"dateFin":32472144000000},
            {"id":"LEGIARTI000006901110","cid":"LEGIARTI000006901110","num":"L1232-2","etat":"ABROGE",
             "pathTitle":["Partie l\u00e9gislative  "],"content":"<p>Abrog\u00e9.</p>","dateDebut":1214524800000,"dateFin":1400000000000}
          ],
          "sections": []
        }
      ]
    },
    {
      "title": "Partie r\u00e9glementaire",
      "sections": [],
      "articles": [
        {"id":"LEGIARTI000035644687","cid":"LEGIARTI000018483188","num":"R1234-1","etat":"VIGUEUR",
         "pathTitle":["Partie r\u00e9glementaire ","Section 1 : Indemnit\u00e9 de licenciement"],
         "content":"<p>Bar\u00e8me :</p><table><tbody><tr><th>Anciennet\u00e9</th><th>Indemnit\u00e9</th></tr><tr><td>1 an</td><td>1/4 de mois</td></tr></tbody></table>",
         "dateDebut":1506297600000,"dateFin":32472144000000}
      ]
    }
  ]
}`

const legifranceArticleJSON = `{
  "article": {
    "id": "LEGIARTI000019071126",
    "cid": "LEGIARTI000006901109",
    "num": "L1232-1",
    "etat": "VIGUEUR",
    "texte": "Tout licenciement pour motif personnel est motivé dans les conditions définies par le présent chapitre. Il est justifié par une cause réelle et sérieuse.",
    "dateDebut": 1214524800000,
    "dateFin": 32472144000000
  }
}`

const judilibreDecisionJSON = `{
  "id": "5fca0f7bd7d1c0b8f1a2b3c4",
  "source": "jurinet",
  "jurisdiction": "cc",
  "chamber": "soc",
  "formation": "fp",
  "number": "21-11.111",
  "numbers": ["21-11.111"],
  "ecli": "ECLI:FR:CCASS:2022:SO00123",
  "publication": ["c", "b", "r"],
  "solution": "Cassation partielle",
  "type": "arret",
  "decision_date": "2022-03-16",
  "publication_date": "2022-04-01",
  "update_date": "2022-04-12",
  "text": "LA COUR DE CASSATION, CHAMBRE SOCIALE, a rendu l'arrêt suivant."
}`

// ---------------------------------------------------------------------------
// The unconfigured contract: 503, never a panic
// ---------------------------------------------------------------------------

// PISTE credentials are empty in development and in CI. Every call must report
// the sentinel error whose message IS the 503 detail, and nothing may panic.
func TestPisteConnectorsDegradeWhenUnconfigured(t *testing.T) {
	ctx := context.Background()

	lf := connectors.NewLegifrance(connectors.Options{})
	if lf.Configured() {
		t.Fatal("Legifrance reports configured with empty credentials")
	}
	if _, err := lf.Search(ctx, connectors.SearchQuery{Terms: "licenciement", Fond: "CODE_DATE"}); err != connectors.ErrLegifranceNotConfigured {
		t.Fatalf("Search: got %v, want ErrLegifranceNotConfigured", err)
	}
	if _, err := lf.GetArticle(ctx, "LEGIARTI000019071126"); err != connectors.ErrLegifranceNotConfigured {
		t.Fatalf("GetArticle: got %v, want ErrLegifranceNotConfigured", err)
	}
	if got := connectors.ErrLegifranceNotConfigured.Error(); got != "Légifrance (PISTE) not configured" {
		t.Fatalf("503 detail drifted: %q", got)
	}

	jl := connectors.NewJudilibre(connectors.Options{})
	if jl.Configured() {
		t.Fatal("Judilibre reports configured with empty credentials")
	}
	for name, err := range map[string]error{
		"Search":   errOf2(jl.Search(ctx, connectors.DecisionQuery{Query: "licenciement"})),
		"Decision": errOf2(jl.Decision(ctx, "abc")),
		"Export":   errOf2(jl.Export(ctx, connectors.EmploymentScope(), 0)),
		"Taxonomy": errOf2(jl.Taxonomy(ctx, "chamber", "cc")),
	} {
		if err != connectors.ErrJudilibreNotConfigured {
			t.Fatalf("%s: got %v, want ErrJudilibreNotConfigured", name, err)
		}
	}
	if got := connectors.ErrJudilibreNotConfigured.Error(); got != "Judilibre (PISTE) not configured" {
		t.Fatalf("503 detail drifted: %q", got)
	}
}

// A partially-configured credential pair is unconfigured, not half-working.
func TestPartialCredentialsAreUnconfigured(t *testing.T) {
	if connectors.NewLegifrance(connectors.Options{ClientID: "id"}).Configured() {
		t.Fatal("client id alone must not count as configured")
	}
	if connectors.NewJudilibre(connectors.Options{ClientSecret: "secret"}).Configured() {
		t.Fatal("client secret alone must not count as configured")
	}
}

// ---------------------------------------------------------------------------
// Légifrance against recorded JSON
// ---------------------------------------------------------------------------

func TestLegifranceSearchAndGetArticle(t *testing.T) {
	var tokenCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenCalls, 1)
			writeJSON(w, http.StatusOK, tokenJSON)
		case "/search":
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("missing bearer token: %q", got)
			}
			writeJSON(w, http.StatusOK, legifranceSearchJSON)
		case "/consult/getArticle":
			writeJSON(w, http.StatusOK, legifranceArticleJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	lf := connectors.NewLegifrance(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})
	ctx := context.Background()

	res, err := lf.Search(ctx, connectors.SearchQuery{
		Terms: "licenciement cause réelle et sérieuse", Fond: "CODE_DATE", CodeName: "Code du travail",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Three distinct articles out of four extracts across two text versions:
	// LEGIARTI000036212577 appears in both and must be emitted once.
	if res.Total != 1942 || len(res.Results) != 3 {
		t.Fatalf("Search decoded wrong: total=%d results=%d %+v", res.Total, len(res.Results), res.Results)
	}
	first := res.Results[0]
	if first.ID != "LEGIARTI000036212577" || first.Num != "R1232-13" ||
		first.Etat != "VIGUEUR" || first.TextCID != "LEGITEXT000006072050" ||
		first.TextTitle != "Code du travail" {
		t.Fatalf("first hit decoded wrong: %+v", first)
	}
	for _, r := range res.Results {
		if r.ID == "" || r.Num == "" {
			t.Fatalf("hit missing the identity GetArticle needs: %+v", r)
		}
	}

	art, err := lf.GetArticle(ctx, "LEGIARTI000019071126")
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if art.Num != "L1232-1" || art.Etat != "VIGUEUR" || !strings.Contains(art.Text, "cause réelle et sérieuse") {
		t.Fatalf("GetArticle decoded wrong: %+v", art)
	}

	// Legal validity is the half-open interval of §6.4, and LEGI's
	// 2999-01-01 sentinel must normalise to "still in force", not to a date.
	from, to := art.Validity()
	if from == nil || *from != 1214524800 {
		t.Fatalf("ValidFrom: got %v, want 1214524800", from)
	}
	if to != nil {
		t.Fatalf("ValidTo: got %v, want nil for the open-ended sentinel", *to)
	}

	// One token for two calls: the client caches it until just before expiry.
	if n := atomic.LoadInt32(&tokenCalls); n != 1 {
		t.Fatalf("token exchanges: got %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Retry policy (§4.3)
// ---------------------------------------------------------------------------

// PISTE rate-limits per application, so 429 must be retried with backoff and
// the upstream's Retry-After must be honoured.
func TestRetriesOn429WithRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeJSON(w, http.StatusOK, tokenJSON)
			return
		}
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, `{"message":"rate limited"}`)
			return
		}
		writeJSON(w, http.StatusOK, legifranceArticleJSON)
	}))
	defer srv.Close()

	lf := connectors.NewLegifrance(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})
	start := time.Now()
	art, err := lf.GetArticle(context.Background(), "LEGIARTI000019071126")
	if err != nil {
		t.Fatalf("GetArticle after 429s: %v", err)
	}
	if art.Num != "L1232-1" {
		t.Fatalf("wrong article after retry: %+v", art)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("attempts: got %d, want 3", n)
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("retries did not back off: %s", d)
	}
}

// A 400 is schema-invalid input: retrying an identical request only spends
// quota to fail again.
func TestDoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeJSON(w, http.StatusOK, tokenJSON)
			return
		}
		atomic.AddInt32(&calls, 1)
		writeJSON(w, http.StatusBadRequest, `{"message":"bad request"}`)
	}))
	defer srv.Close()

	lf := connectors.NewLegifrance(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})
	if _, err := lf.GetArticle(context.Background(), "X"); err == nil {
		t.Fatal("expected an error on 400")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("attempts: got %d, want 1 (400 is not retryable)", n)
	}
}

// ---------------------------------------------------------------------------
// Judilibre
// ---------------------------------------------------------------------------

// Every field §6.4 demands on a decision must survive decoding: decision date,
// publication date, jurisdiction, formation, publication level, solution,
// ECLI, pourvoi number and the source revision.
func TestJudilibreDecisionCarriesRequiredMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeJSON(w, http.StatusOK, tokenJSON)
			return
		}
		writeJSON(w, http.StatusOK, judilibreDecisionJSON)
	}))
	defer srv.Close()

	jl := connectors.NewJudilibre(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})
	d, err := jl.Decision(context.Background(), "5fca0f7bd7d1c0b8f1a2b3c4")
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}

	if d.Jurisdiction != "cc" || d.Chamber != "soc" || d.Formation != "fp" {
		t.Fatalf("jurisdiction/formation lost: %+v", d)
	}
	if d.ECLI != "ECLI:FR:CCASS:2022:SO00123" || d.Pourvoi() != "21-11.111" {
		t.Fatalf("ecli/pourvoi lost: ecli=%q pourvoi=%q", d.ECLI, d.Pourvoi())
	}
	if d.Solution != "Cassation partielle" {
		t.Fatalf("solution lost: %q", d.Solution)
	}
	// "r" (Rapport annuel) outranks "b" and "c" whatever order they arrive in.
	if lvl := d.PublicationLevel(); lvl != "r" {
		t.Fatalf("publication level: got %q, want %q", lvl, "r")
	}
	if got, want := *d.DecidedAt(), mustDate(t, "2022-03-16"); got != want {
		t.Fatalf("decision_date: got %d, want %d", got, want)
	}
	if got, want := *d.PublishedAt(), mustDate(t, "2022-04-01"); got != want {
		t.Fatalf("publication_date: got %d, want %d", got, want)
	}
	// The source revision is what uniq(legal_document_id, source_version)
	// converges on, so it must be the API's own revision marker.
	if rev := d.SourceRevision(); rev != "2022-04-12" {
		t.Fatalf("source revision: got %q, want the update_date", rev)
	}
}

// /export is the paginated bulk path; next_batch is the resumable cursor
// corpus_syncs stores, and the scope must reach the wire as query parameters.
func TestJudilibreExportPaginatesAndScopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			writeJSON(w, http.StatusOK, tokenJSON)
			return
		}
		q := r.URL.Query()
		if q.Get("jurisdiction") != "cc" || q.Get("chamber") != "soc" {
			t.Errorf("scope missing from /export: %s", r.URL.RawQuery)
		}
		if q.Get("date_start") != "2018-01-01" {
			t.Errorf("date window missing: %s", q.Get("date_start"))
		}
		// `batch` is REQUIRED by the real API — omitting it is a 400, not a
		// default of zero. The fake enforces that, because a fake laxer than
		// the service it stands in for tests nothing.
		if !q.Has("batch") {
			t.Errorf("/export called without the mandatory batch parameter: %s", r.URL.RawQuery)
			writeJSON(w, http.StatusBadRequest, `{"errors":[{"msg":"Value of the batch parameter must be an integer greater or equal than 0.","param":"batch"}]}`)
			return
		}
		// next_batch is the WHOLE query string of the next page, as the API
		// returns it — not a number.
		switch q.Get("batch") {
		case "0":
			writeJSON(w, http.StatusOK, `{"batch":0,"next_batch":"jurisdiction=cc&chamber=soc&date_start=2018-01-01&batch_size=100&batch=1","total":2,"results":[`+judilibreDecisionJSON+`]}`)
		default:
			writeJSON(w, http.StatusOK, `{"batch":1,"next_batch":"","total":2,"results":[`+judilibreDecisionJSON+`]}`)
		}
	}))
	defer srv.Close()

	jl := connectors.NewJudilibre(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})
	ctx := context.Background()
	scope := connectors.EmploymentScope()

	first, err := jl.Export(ctx, scope, 0)
	if err != nil {
		t.Fatalf("Export batch 0: %v", err)
	}
	next, more := first.NextBatchNumber()
	if !more || next != 1 || len(first.Results) != 1 {
		t.Fatalf("batch 0 decoded wrong: next=%d more=%t %+v", next, more, first)
	}

	second, err := jl.Export(ctx, scope, next)
	if err != nil {
		t.Fatalf("Export batch 1: %v", err)
	}
	if _, more := second.NextBatchNumber(); more {
		t.Fatalf("the last batch must report no next batch, got %q", second.NextBatch)
	}
}

// The export returns publication levels as French labels while the search
// filters take codes. Reading only the codes marked every real decision "non
// publié" — the one field that stands in for authority.
func TestJudilibrePublicationLevelReadsBothVocabularies(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"labels, highest wins", []string{"Publié au Bulletin", "Publié aux Lettres de chambre"}, "b"},
		{"rapport annuel outranks bulletin", []string{"Publié au Bulletin", "Publié au Rapport annuel"}, "r"},
		{"lettres alone", []string{"Publié aux Lettres de chambre"}, "l"},
		{"codes still work", []string{"b", "c"}, "b"},
		{"unpublished", []string{}, "n"},
		{"unknown label", []string{"Diffusé sur le site"}, "n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := connectors.Decision{Publication: c.in}
			if got := d.PublicationLevel(); got != c.want {
				t.Fatalf("PublicationLevel(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The MVP case-law scope is employment law only (§5.3): if this ever widens,
// the corpus stops fitting.
func TestEmploymentScopeStaysNarrow(t *testing.T) {
	s := connectors.EmploymentScope()
	if len(s.Jurisdiction) != 1 || s.Jurisdiction[0] != "cc" {
		t.Fatalf("jurisdiction widened beyond the Cour de cassation: %v", s.Jurisdiction)
	}
	if len(s.Chamber) != 1 || s.Chamber[0] != "soc" {
		t.Fatalf("chamber widened beyond the chambre sociale: %v", s.Chamber)
	}
	if s.DateStart.Year() != 2018 {
		t.Fatalf("date window widened: %s", s.DateStart)
	}
}

// ---------------------------------------------------------------------------
// Hugging Face bootstrap
// ---------------------------------------------------------------------------

func TestHFPartitionsStayInEmploymentScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !strings.Contains(r.URL.Path, connectors.HFPinnedRevision) {
			t.Errorf("tree listing did not use the pinned revision: %s", r.URL.Path)
		}
		code := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		writeJSON(w, http.StatusOK, `[
		  {"type":"directory","path":"data/legi-latest/`+code+`/nested"},
		  {"type":"file","path":"data/legi-latest/`+code+`/README.md","size":10},
		  {"type":"file","path":"data/legi-latest/`+code+`/`+code+`_part_0.parquet","size":0,
		   "lfs":{"oid":"c3235020d4091b58","size":261671687}}
		]`)
	}))
	defer srv.Close()

	h := connectors.NewHFDataset(connectors.HFOptions{APIURL: srv.URL, CDNURL: srv.URL})
	parts, err := h.Partitions(context.Background())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(parts) != len(connectors.EmploymentPartitions) {
		t.Fatalf("got %d partitions, want %d (one per employment code)", len(parts), len(connectors.EmploymentPartitions))
	}
	for _, p := range parts {
		if !strings.HasSuffix(p.Path, ".parquet") {
			t.Fatalf("non-parquet entry leaked in: %s", p.Path)
		}
		if p.SHA256 == "" {
			t.Fatalf("partition %s has no content hash", p.Code)
		}
		if p.Size != 261671687 {
			t.Fatalf("partition %s size came from the wrong field: %d", p.Code, p.Size)
		}
	}
	// The scope is the §5.3 guard rail: all of LEGI is ~118 more partitions.
	if len(connectors.EmploymentPartitions) > 6 {
		t.Fatalf("employment scope widened to %d codes", len(connectors.EmploymentPartitions))
	}
}

func TestHFProbeParquetReadsOnlyTheFooter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=-8" {
			t.Errorf("probe downloaded more than the footer: Range=%q", r.Header.Get("Range"))
		}
		w.WriteHeader(http.StatusPartialContent)
		// 0x00001b4b little-endian, then the parquet magic.
		w.Write([]byte{0x4b, 0x1b, 0x00, 0x00, 'P', 'A', 'R', '1'})
	}))
	defer srv.Close()

	h := connectors.NewHFDataset(connectors.HFOptions{APIURL: srv.URL, CDNURL: srv.URL})
	probe, err := h.ProbeParquet(context.Background(), connectors.Partition{
		Path: "data/legi-latest/legi_code_du_travail/legi_code_du_travail_part_0.parquet",
		Code: "legi_code_du_travail", Size: 261671687,
	})
	if err != nil {
		t.Fatalf("ProbeParquet: %v", err)
	}
	if !probe.IsParquet || probe.FooterBytes != 0x1b4b {
		t.Fatalf("probe decoded wrong: %+v", probe)
	}
}

// The free datasets viewer refuses this dataset for its size. That is a
// supported outcome — the committed fixture corpus is the offline path — so it
// must surface as a named sentinel, not a generic failure.
func TestHFRowsUnavailableIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, `{"error":"Job has been terminated due to a temporary spike in resource usage."}`)
	}))
	defer srv.Close()

	h := connectors.NewHFDataset(connectors.HFOptions{RowsURL: srv.URL})
	_, err := h.Rows(context.Background(), 0, 10)
	if err == nil || !strings.Contains(err.Error(), connectors.ErrHFRowsUnavailable.Error()) {
		t.Fatalf("Rows: got %v, want ErrHFRowsUnavailable", err)
	}
}

// A pre-computed vector of the wrong width is REJECTED, never loaded: the
// dimension is baked into the halfvec column, so a mismatch silently poisons
// the corpus (§9.1(2)).
func TestHFRowVectorWidthIsEnforced(t *testing.T) {
	good := make([]float32, connectors.HFEmbeddingDims)
	good[0] = 0.5
	row := connectors.LegiRow{Embeddings: mustJSON(t, good), ChunkText: "x", ChunkXXH64: "abc"}
	v, err := row.Vector()
	if err != nil || len(v) != connectors.HFEmbeddingDims {
		t.Fatalf("Vector: %v (len=%d)", err, len(v))
	}

	bad := connectors.LegiRow{Embeddings: mustJSON(t, make([]float32, 768))}
	if _, err := bad.Vector(); err == nil {
		t.Fatal("a 768-dim vector must be rejected against halfvec(1024)")
	}
}

// Every loaded row is traceable: pinned revision + the dataset's own hash.
func TestHFRowScopeAndProvenance(t *testing.T) {
	in := connectors.LegiRow{FullTitle: "Code du travail", ChunkXXH64: "5c98b8711ec98a0f"}
	out := connectors.LegiRow{FullTitle: "Code rural et de la pêche maritime"}
	if !in.InEmploymentScope() {
		t.Fatal("Code du travail must be in scope")
	}
	if out.InEmploymentScope() {
		t.Fatal("Code rural must be out of scope — employment law only (§5.3)")
	}
	if h := in.ContentHash(connectors.HFPinnedRevision); h != connectors.HFPinnedRevision+":5c98b8711ec98a0f" {
		t.Fatalf("content hash lost its revision: %q", h)
	}
	if connectors.HFPinnedRevision == "" || connectors.HFPinnedRevision == "main" {
		t.Fatal("the dataset revision must be pinned to a commit, never to a moving branch")
	}
}

// ---------------------------------------------------------------------------
// The committed fixture corpus
// ---------------------------------------------------------------------------

// This is what makes the app and the relevance harness work with no network
// and no PISTE credentials, so its shape is a contract, not an implementation
// detail.
func TestFixtureCorpusIsUsableOffline(t *testing.T) {
	c, err := fixtures.CodeDuTravail()
	if err != nil {
		t.Fatalf("CodeDuTravail: %v", err)
	}
	if n := len(c.Articles); n < 150 || n > 300 {
		t.Fatalf("fixture holds %d articles, want 150–300", n)
	}

	// Licence Ouverte 2.0 permits commercial reuse WITH ATTRIBUTION, so a
	// fixture that lost its attribution is not redistributable (§13).
	if c.License.Code != "etalab-2.0" {
		t.Fatalf("licence code: %q", c.License.Code)
	}
	for name, v := range map[string]string{
		"attribution":     c.License.Attribution,
		"licence url":     c.License.URL,
		"source url":      c.License.SourceURL,
		"snapshot":        c.Source.Snapshot,
		"snapshot sha256": c.Source.SnapshotSHA256,
	} {
		if strings.TrimSpace(v) == "" {
			t.Fatalf("fixture header is missing its %s", name)
		}
	}

	seen := map[string]bool{}
	for _, a := range c.Articles {
		if a.Number == "" || a.DocumentID == "" || a.VersionID == "" {
			t.Fatalf("article without an identity: %+v", a)
		}
		if len(a.Text) < 40 {
			t.Fatalf("article %s has no usable text", a.Number)
		}
		if a.State != "VIGUEUR" {
			t.Fatalf("article %s is not in force (%s) — the fixture is the in-force subset", a.Number, a.State)
		}
		if a.ValidFrom == nil {
			t.Fatalf("article %s has no valid_from; §6.4 needs the half-open interval", a.Number)
		}
		if a.ValidTo != nil && *a.ValidTo <= *a.ValidFrom {
			t.Fatalf("article %s has an inverted validity interval", a.Number)
		}
		// uniq(source, source_id) is what makes the load idempotent; a
		// duplicated natural key in the fixture would defeat it.
		if seen[a.DocumentID] {
			t.Fatalf("duplicate document id in the fixture: %s", a.DocumentID)
		}
		seen[a.DocumentID] = true
	}

	// Employment law only. A handful of landmark articles that the relevance
	// harness will look for must actually be there.
	want := []string{"L1232-1", "L1235-3", "L1471-1", "L1152-1", "L1233-3"}
	have := map[string]bool{}
	for _, a := range c.Articles {
		have[a.Number] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Fatalf("fixture is missing C. trav., art. %s", n)
		}
	}
}

// ---------------------------------------------------------------------------
// Live network, opt-in
// ---------------------------------------------------------------------------

// Proves the public Hugging Face endpoints still answer at the pinned
// revision, that the employment partitions still exist, and that they still
// serve parquet. Opt-in because CI must not depend on huggingface.co.
//
//	NOMOS_NETWORK_TESTS=1 go test ./connectors/... -run Live -v
func TestLiveHuggingFaceEndpoints(t *testing.T) {
	testmode.Live(t, "Hugging Face live endpoints", "NOMOS_NETWORK_TESTS")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h := connectors.NewHFDataset(connectors.HFOptions{})
	head, err := h.HeadRevision(ctx)
	if err != nil {
		t.Fatalf("HeadRevision: %v", err)
	}
	t.Logf("dataset head=%s pinned=%s", head, h.Revision())

	parts, err := h.Partitions(ctx)
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	for _, p := range parts {
		probe, err := h.ProbeParquet(ctx, p)
		if err != nil {
			t.Fatalf("ProbeParquet %s: %v", p.Code, err)
		}
		if !probe.IsParquet {
			t.Fatalf("%s does not serve parquet", p.Code)
		}
		t.Logf("%s: %d bytes, sha256=%s, parquet footer=%d bytes", p.Code, p.Size, p.SHA256, probe.FooterBytes)
	}
}

// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func errOf2[T any](_ T, err error) error { return err }

func mustDate(t *testing.T, s string) int64 {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return d.UTC().Unix()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLegifranceLegiPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			writeJSON(w, http.StatusOK, tokenJSON)
		case "/consult/legiPart":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("legiPart body: %v", err)
			}
			if body["textId"] != "LEGITEXT000006072050" {
				t.Errorf("textId = %v, want the Code du travail", body["textId"])
			}
			if body["date"] == "" {
				t.Errorf("legiPart must send a consolidation date")
			}
			writeJSON(w, http.StatusOK, legifrancePartJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	lf := connectors.NewLegifrance(connectors.Options{
		ClientID: "id", ClientSecret: "secret",
		TokenURL: srv.URL + "/token", BaseURL: srv.URL,
	})

	tree, err := lf.LegiPart(context.Background(), "LEGITEXT000006072050", time.Time{})
	if err != nil {
		t.Fatalf("LegiPart: %v", err)
	}
	if tree.ID != "LEGITEXT000006072050_19-08-2026" || tree.ModifDate != "2026-08-19" {
		t.Fatalf("revision decoded wrong: id=%q modifDate=%q", tree.ID, tree.ModifDate)
	}

	// The walk must reach articles nested two levels deep AND articles hanging
	// directly off a top-level section.
	arts := tree.FlatArticles()
	if len(arts) != 3 {
		t.Fatalf("walked %d articles, want 3: %+v", len(arts), arts)
	}
	byNum := map[string]connectors.CodeArticle{}
	for _, a := range arts {
		byNum[a.Num] = a
	}
	for _, want := range []string{"L1232-1", "L1232-2", "R1234-1"} {
		if _, ok := byNum[want]; !ok {
			t.Fatalf("article %s missing from the walk", want)
		}
	}

	// HTML body → plain text, with the paragraph break preserved so chunking
	// still has something to split on.
	a := byNum["L1232-1"]
	// A paragraph break, not a line break: chunkText splits on "\n\n".
	wantText := "Tout licenciement pour motif personnel est motivé.\n\nIl est justifié par une cause réelle et sérieuse."
	if a.Text() != wantText {
		t.Fatalf("Text():\n got %q\nwant %q", a.Text(), wantText)
	}
	if a.NotaText() != "Une note." {
		t.Fatalf("NotaText() = %q", a.NotaText())
	}
	if got := a.SectionPath(); len(got) != 2 || got[0] != "Partie législative" {
		t.Fatalf("SectionPath() = %+v, want the padding trimmed and blanks dropped", got)
	}
	from, to := a.Validity()
	if from == nil || *from != 1214524800 {
		t.Fatalf("ValidFrom = %v, want 1214524800", from)
	}
	if to != nil {
		t.Fatalf("ValidTo = %v, want nil for LEGI's 2999 sentinel", *to)
	}

	// A table must not collapse into one run of words: the cells carry the
	// indemnity scale, and a pleading may quote it.
	scale := byNum["R1234-1"].Text()
	if !strings.Contains(scale, "1 an") || !strings.Contains(scale, "1/4 de mois") {
		t.Fatalf("table cells lost: %q", scale)
	}
	if strings.Contains(scale, "1 an1/4") {
		t.Fatalf("table cells were mashed together: %q", scale)
	}

	// The repealed article still parses; scoping is the loader's job, not the
	// connector's.
	if byNum["L1232-2"].Etat != "ABROGE" {
		t.Fatalf("etat lost: %+v", byNum["L1232-2"])
	}
}
