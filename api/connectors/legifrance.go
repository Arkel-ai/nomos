// Package connectors holds the outbound clients for the public legal corpus:
// Légifrance and Judilibre on the PISTE platform, and the Hugging Face
// bootstrap dataset (docs/PLAN.md §5.1).
//
// Every source here is OPEN LICENCE (Licence Ouverte 2.0 / Etalab 2.0), and
// the licence code travels with the data into legal_documents.license_code —
// commercial reuse is permitted WITH ATTRIBUTION, and the licence does not
// lift personal-data or third-party-IP obligations (§13).
//
// Nomos ships NO connector for Dalloz, LexisNexis, Éditions Francis Lefebvre,
// Lamy or Doctrine.fr. They are paywalled, their terms forbid scraping and
// re-publication, and their commentary is copyrighted (§5.2). Do not add one.
//
// SCOPE. These connectors exist to load ONE PRACTICE AREA — employment law
// (§5.3). "All of LEGI" or "all of Judilibre" is ~10^7 vectors and 80–150 GB
// of working database against a 4 GiB default volume: it does not fit, and
// nothing here should be pointed at an unscoped fetch.
//
// CONFIGURATION CONTRACT. Every connector follows the template's
// optional-integration rule: with no credentials the constructor still returns
// a usable value, and each call returns a sentinel error whose message is
// exactly the `detail` the service turns into a 503. Never panic on a missing
// credential — PISTE credentials are empty in development by default.
package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var legifranceLog = u.NewLogger("connectors.legifrance")

// Default PISTE endpoints. Overridable through Options so the whole package is
// unit-testable against recorded JSON served by an httptest.Server, with no
// network and no credentials.
const (
	// DefaultPisteTokenURL is the OAuth2 client-credentials token endpoint.
	DefaultPisteTokenURL = "https://oauth.piste.gouv.fr/api/oauth/token"
	// DefaultLegifranceBaseURL is the DILA Légifrance engine on PISTE.
	DefaultLegifranceBaseURL = "https://api.piste.gouv.fr/dila/legifrance/lf-engine-app"
	// DefaultJudilibreBaseURL is the Cour de cassation's Judilibre on PISTE.
	DefaultJudilibreBaseURL = "https://api.piste.gouv.fr/cassation/judilibre/v1.0"

	// LicenceOuverte2 is the licence code stamped on every row loaded from
	// Légifrance, Judilibre or DILA. It must survive into the output.
	LicenceOuverte2 = "etalab-2.0"
)

// ErrLegifranceNotConfigured is returned by every Légifrance call when
// PISTE_CLIENT_ID / PISTE_CLIENT_SECRET are empty. Its message IS the 503
// detail string the service returns, so the two can never drift apart.
var ErrLegifranceNotConfigured = errors.New("Légifrance (PISTE) not configured")

// ErrJudilibreNotConfigured is the Judilibre equivalent.
var ErrJudilibreNotConfigured = errors.New("Judilibre (PISTE) not configured")

// APIError is a non-2xx answer from a connector's upstream. Retryable marks
// the transient classes — 408, 429 and 5xx — matching the retry policy of
// §4.3: a 400 is schema-invalid input and a 401/403 is configuration, and
// neither changes with an identical second attempt.
type APIError struct {
	Source     string
	Status     int
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Source, e.Status, e.Message)
}

// Options configures a connector. Zero values fall back to the public PISTE
// endpoints and a client with a bounded timeout — never http.DefaultClient,
// which has none.
type Options struct {
	ClientID     string
	ClientSecret string
	// TokenURL and BaseURL are injection points for tests.
	TokenURL string
	BaseURL  string
	// HTTPClient is shared with the token exchange.
	HTTPClient *http.Client
	// MaxAttempts caps the retry loop. 0 → defaultMaxAttempts.
	MaxAttempts int
}

const (
	defaultTimeout     = 60 * time.Second
	defaultMaxAttempts = 4
	backoffBase        = 500 * time.Millisecond
	backoffCap         = 20 * time.Second
	tokenExpirySlack   = 30 * time.Second
	// maxResponseBodySize bounds one response. It is sized for /consult/legiPart,
	// which returns an ENTIRE code in one document — the Code du travail is
	// ~32 MB today and grows with every amending decree. A Judilibre export
	// batch is large; neither is unbounded.
	maxResponseBodySize = 128 << 20
)

// piste is the shared PISTE plumbing: one OAuth2 client-credentials token
// cached until just before it expires, plus a retrying JSON caller. Légifrance
// and Judilibre are two APIs on ONE platform with ONE credential pair, so they
// share it rather than each running its own token dance.
type piste struct {
	clientID     string
	clientSecret string
	tokenURL     string
	scope        string
	http         *http.Client
	maxAttempts  int

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newPiste(o Options, scope string) *piste {
	p := &piste{
		clientID:     strings.TrimSpace(o.ClientID),
		clientSecret: strings.TrimSpace(o.ClientSecret),
		tokenURL:     o.TokenURL,
		scope:        scope,
		http:         o.HTTPClient,
		maxAttempts:  o.MaxAttempts,
	}
	if p.tokenURL == "" {
		p.tokenURL = DefaultPisteTokenURL
	}
	if p.http == nil {
		p.http = &http.Client{Timeout: defaultTimeout}
	}
	if p.maxAttempts <= 0 {
		p.maxAttempts = defaultMaxAttempts
	}
	return p
}

func (p *piste) configured() bool { return p.clientID != "" && p.clientSecret != "" }

// accessToken returns a cached token, refreshing it when it is within
// tokenExpirySlack of expiry. The mutex is held across the refresh on purpose:
// a corpus sync is a single background loop, so the contention is nil and a
// thundering herd of token requests would be worse than serialising.
func (p *piste) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" && time.Now().Add(tokenExpirySlack).Before(p.expiresAt) {
		return p.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"scope":         {p.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("piste token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("piste token: %w", err)
	}
	if resp.StatusCode >= 400 {
		// Never echo the credentials or the body: a token endpoint's error
		// payload can contain the client id (§7.4).
		return "", &APIError{Source: "piste-token", Status: resp.StatusCode,
			Message: "token request rejected", Retryable: retryableStatus(resp.StatusCode)}
	}

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("piste token: decode: %w", err)
	}
	if body.AccessToken == "" {
		return "", errors.New("piste token: empty access_token")
	}
	if body.ExpiresIn <= 0 {
		body.ExpiresIn = 3600
	}
	p.token = body.AccessToken
	p.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	legifranceLog.Debugf("PISTE token refreshed (expires_in=%ds scope=%s)", body.ExpiresIn, p.scope)
	return p.token, nil
}

// call performs one authenticated JSON request with capped-backoff retry.
//
// The retry policy is the project's (§4.3): 408, 429 and 5xx and transport
// errors are retried with exponential backoff plus jitter, capped at
// backoffCap, honouring a `Retry-After` header when the upstream sends one —
// PISTE rate-limits per application, so a synchronised retry storm across
// replicas is exactly what the jitter is for. Everything else fails at once.
func (p *piste) call(ctx context.Context, method, endpoint string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		if attempt > 1 {
			wait := backoff(attempt - 1)
			var ae *APIError
			if errors.As(lastErr, &ae) && ae.RetryAfter > 0 {
				wait = min(ae.RetryAfter, backoffCap)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		raw, err := p.attempt(ctx, method, endpoint, payload)
		if err == nil {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return nil
		}
		lastErr = err
		var ae *APIError
		if ctx.Err() != nil || !errors.As(err, &ae) || !ae.Retryable {
			return err
		}
		legifranceLog.Warnf("PISTE attempt %d/%d failed (status=%d): %s", attempt, p.maxAttempts, ae.Status, ae.Message)
	}
	return lastErr
}

func (p *piste) attempt(ctx context.Context, method, endpoint string, payload []byte) ([]byte, error) {
	token, err := p.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &APIError{Source: "piste", Status: 0, Message: "connection error", Retryable: true}
	}
	defer resp.Body.Close()

	// Read one byte past the cap: io.LimitReader TRUNCATES silently, so
	// without this an oversized body would surface as a baffling JSON syntax
	// error instead of the size problem it actually is.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize+1))
	if err != nil {
		return nil, &APIError{Source: "piste", Status: resp.StatusCode, Message: "truncated response body", Retryable: true}
	}
	if len(raw) > maxResponseBodySize {
		return nil, &APIError{Source: "piste", Status: resp.StatusCode,
			Message: fmt.Sprintf("response exceeds the %d MiB cap", maxResponseBodySize>>20)}
	}
	if resp.StatusCode >= 400 {
		// A 401 after a successful token exchange usually means the cached
		// token was revoked; drop it so the next attempt re-authenticates.
		if resp.StatusCode == http.StatusUnauthorized {
			p.mu.Lock()
			p.token, p.expiresAt = "", time.Time{}
			p.mu.Unlock()
		}
		return nil, &APIError{
			Source:     "piste",
			Status:     resp.StatusCode,
			Message:    upstreamMessage(raw),
			Retryable:  retryableStatus(resp.StatusCode),
			RetryAfter: retryAfter(resp.Header),
		}
	}
	return raw, nil
}

// retryableStatus implements §4.3: only the transient classes come back.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoff is capped exponential with ±25 % jitter, so replicas recovering from
// a PISTE rate limit do not resynchronise into a herd.
func backoff(n int) time.Duration {
	d := float64(backoffBase) * math.Pow(2, float64(n))
	if d > float64(backoffCap) {
		d = float64(backoffCap)
	}
	return time.Duration(d * (0.75 + 0.5*rand.Float64()))
}

// upstreamMessage extracts an error string without echoing the request back
// into a log line (§7.4).
func upstreamMessage(raw []byte) string {
	var body struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		for _, s := range []string{body.ErrorDescription, body.Message, body.Error} {
			if s != "" {
				return s
			}
		}
	}
	return "upstream error"
}

// ---------------------------------------------------------------------------
// Légifrance
// ---------------------------------------------------------------------------

// Legifrance is the DILA Légifrance client on PISTE (§5.1). Licence Ouverte
// 2.0: reuse including commercial reuse is permitted with attribution.
//
// It is the AUTHORITY OF RECORD for legislation, which is why §6.4 requires
// export-time revalidation against it: everything Nomos indexes — the
// AgentPublic/legi bootstrap, a Judilibre lead, a fixture — is a cache, and
// the opposable text is the authenticated Légifrance publication.
type Legifrance struct {
	piste   *piste
	baseURL string
}

// NewLegifrance builds the client. With empty credentials it still returns a
// usable value whose calls report ErrLegifranceNotConfigured — the service
// turns that into 503 {"detail": "Légifrance (PISTE) not configured"}.
func NewLegifrance(o Options) *Legifrance {
	base := o.BaseURL
	if base == "" {
		base = DefaultLegifranceBaseURL
	}
	return &Legifrance{piste: newPiste(o, "openid"), baseURL: strings.TrimRight(base, "/")}
}

// Configured reports whether PISTE credentials are present.
func (l *Legifrance) Configured() bool { return l.piste.configured() }

// SearchQuery is a POST /search body, narrowed to what the employment-law
// loader needs. Fond is the corpus ("CODE_DATE" for a consolidated code at a
// date, "LODA_DATE", "JURI"); the caller sets it explicitly rather than
// letting a default silently widen the scope beyond §5.3.
type SearchQuery struct {
	Terms    string
	Fond     string
	CodeName string
	PageSize int
	Page     int
	// AtDate is the consolidation date. Zero means "today", which is only
	// correct for a corpus refresh, never for a dispute about an earlier
	// period (§6.4).
	AtDate time.Time
}

// SearchResult is one ARTICLE hit: the identity needed to fetch the article,
// not the article itself.
//
// Légifrance does NOT return a flat list of articles. It paginates by TEXT —
// one result per code version — and hangs the matching articles off
// results[].sections[].extracts[]. The LEGIARTI id GetArticle needs only
// exists down there, which is why Search flattens rather than decoding the
// envelope straight into this struct.
type SearchResult struct {
	// ID is the LEGIARTI version id — the value GetArticle takes.
	ID string `json:"id"`
	// Num is the article number as lawyers cite it ("L1232-1").
	Num string `json:"num"`
	// Etat is the Légifrance status vocabulary ("VIGUEUR", "ABROGE"), the
	// same vocabulary types.LegalVersion* uses.
	Etat string `json:"etat"`
	// TextCID is the containing text's chronicle id (LEGITEXT000006072050).
	// A search hit carries no per-article cid — only the text's.
	TextCID string `json:"text_cid"`
	// TextTitle is the containing text ("Code du travail").
	TextTitle string `json:"text_title"`
}

// SearchResponse is the trimmed POST /search answer.
type SearchResponse struct {
	// Total is the API's totalResultNumber. It counts TEXT VERSIONS, not
	// articles: a "licenciement" search in CODE_DATE reports ~1942 because
	// that is how many dated versions of the Code du travail matched, while
	// the page itself yields a few dozen articles. A loader that treats it
	// as an article count will size its work wrong.
	Total   int            `json:"totalResultNumber"`
	Results []SearchResult `json:"results"`
}

// searchEnvelope is the wire shape of POST /search. It exists because the
// public SearchResult is a flattened view of it.
type searchEnvelope struct {
	Total   int `json:"totalResultNumber"`
	Results []struct {
		Titles []struct {
			CID   string `json:"cid"`
			Title string `json:"title"`
		} `json:"titles"`
		Sections []struct {
			Extracts []struct {
				ID          string `json:"id"`
				Num         string `json:"num"`
				LegalStatus string `json:"legalStatus"`
			} `json:"extracts"`
		} `json:"sections"`
	} `json:"results"`
}

// flatten pulls the article hits out of the text-shaped envelope.
//
// It deduplicates on the LEGIARTI id: consecutive dated versions of the same
// code match the same articles, so one page routinely repeats a hit. Emitting
// the repeat would inflate every counter downstream of the loader.
func (e searchEnvelope) flatten() []SearchResult {
	out := make([]SearchResult, 0, len(e.Results))
	seen := make(map[string]bool)
	for _, r := range e.Results {
		var cid, title string
		if len(r.Titles) > 0 {
			cid, title = r.Titles[0].CID, r.Titles[0].Title
		}
		for _, s := range r.Sections {
			for _, x := range s.Extracts {
				if x.ID == "" || seen[x.ID] {
					continue
				}
				seen[x.ID] = true
				out = append(out, SearchResult{
					ID:        x.ID,
					Num:       x.Num,
					Etat:      x.LegalStatus,
					TextCID:   cid,
					TextTitle: title,
				})
			}
		}
	}
	return out
}

// Article is a consolidated article version. The date fields are LEGAL
// VALIDITY — a half-open [ValidFrom, ValidTo) interval (§6.4) — and are kept
// strictly apart from any ingestion timestamp. LEGI encodes them as
// milliseconds, with 2999-01-01 as the open-ended sentinel; normalise reduces
// both to unix seconds and a nil ValidTo.
type Article struct {
	ID        string `json:"id"`
	CID       string `json:"cid"`
	Num       string `json:"num"`
	Text      string `json:"texte"`
	Etat      string `json:"etat"`
	Nota      string `json:"nota"`
	DateDebut int64  `json:"dateDebut"`
	DateFin   int64  `json:"dateFin"`
	Context   struct {
		TitreTxt []struct {
			Titre string `json:"titre"`
		} `json:"titreTxt"`
	} `json:"context"`
	// TextHTML is the same body as HTML — what the code tree (LegiPart)
	// serves and the corpus loader stores through htmlText. The plain Text
	// is NOT the same rendering: it pads a cross-reference link with a space
	// ("article L. 1243-8 ." where the HTML gives "L. 1243-8</a>.").
	TextHTML string `json:"texteHtml"`
}

// HTMLText renders TextHTML as plain text, exactly as the corpus loader
// renders a code tree article (CodeArticle.Text).
func (a Article) HTMLText() string { return htmlText(a.TextHTML) }

// legiOpenEndedMillis is LEGI's 2999-01-01 "no end date" sentinel.
const legiOpenEndedMillis int64 = 32472144000000

// Validity converts the LEGI millisecond dates into the unix-second half-open
// interval legal_versions stores. A nil ValidTo means "still in force".
func (a Article) Validity() (validFrom *int64, validTo *int64) {
	if a.DateDebut > 0 {
		s := a.DateDebut / 1000
		validFrom = &s
	}
	if a.DateFin > 0 && a.DateFin < legiOpenEndedMillis {
		s := a.DateFin / 1000
		validTo = &s
	}
	return validFrom, validTo
}

// Search runs POST /search.
func (l *Legifrance) Search(ctx context.Context, q SearchQuery) (SearchResponse, error) {
	if !l.Configured() {
		return SearchResponse{}, ErrLegifranceNotConfigured
	}
	at := q.AtDate
	if at.IsZero() {
		at = time.Now()
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}

	champ := map[string]any{
		"typeChamp": "ALL",
		"criteres": []map[string]any{{
			"typeRecherche": "UN_DES_MOTS",
			"valeur":        q.Terms,
			"operateur":     "ET",
		}},
		"operateur": "ET",
	}
	recherche := map[string]any{
		"champs":                []any{champ},
		"pageNumber":            page,
		"pageSize":              pageSize,
		"operateur":             "ET",
		"typePagination":        "ARTICLE",
		"sort":                  "PERTINENCE",
		"secondSort":            "ID",
		"fromAdvancedRecherche": false,
	}
	if q.CodeName != "" {
		recherche["filtres"] = []map[string]any{{
			"facette": "NOM_CODE",
			"valeurs": []string{q.CodeName},
		}}
	}
	payload := map[string]any{
		"recherche": recherche,
		"fond":      q.Fond,
		"date":      at.Format("2006-01-02"),
	}

	var env searchEnvelope
	if err := l.piste.call(ctx, http.MethodPost, l.baseURL+"/search", payload, &env); err != nil {
		return SearchResponse{}, err
	}
	resp := SearchResponse{Total: env.Total, Results: env.flatten()}
	legifranceLog.Infof("Légifrance search returned %d articles across %d texts (fond=%s page=%d total_texts=%d)",
		len(resp.Results), len(env.Results), q.Fond, page, env.Total)
	return resp, nil
}

// GetArticle runs POST /consult/getArticle for one LEGIARTI id.
func (l *Legifrance) GetArticle(ctx context.Context, id string) (Article, error) {
	if !l.Configured() {
		return Article{}, ErrLegifranceNotConfigured
	}
	if strings.TrimSpace(id) == "" {
		return Article{}, errors.New("legifrance: empty article id")
	}
	var resp struct {
		Article Article `json:"article"`
	}
	if err := l.piste.call(ctx, http.MethodPost, l.baseURL+"/consult/getArticle",
		map[string]any{"id": id}, &resp); err != nil {
		return Article{}, err
	}
	if resp.Article.ID == "" {
		return Article{}, fmt.Errorf("legifrance: article %s not found", id)
	}
	legifranceLog.Debugf("Légifrance article fetched (id=%s num=%s etat=%s length=%d)",
		resp.Article.ID, resp.Article.Num, resp.Article.Etat, len(resp.Article.Text))
	return resp.Article, nil
}

// CanonicalURL is the citable public URL of an article version.
func CanonicalURL(articleID string) string {
	return "https://www.legifrance.gouv.fr/codes/article_lc/" + articleID
}

// ---------------------------------------------------------------------------
// Code consultation (POST /consult/legiPart)
// ---------------------------------------------------------------------------

// CodeTree is one consolidated code as /consult/legiPart returns it: the whole
// text at one date, sections nested arbitrarily deep, with every article's body
// inline.
//
// One call, one code. That is the point — enumerating a code through /search is
// not possible (a NUM_ARTICLE search matches one number at a time, and prefix
// matching is unsupported), and paginating CODE_DATE returns the same articles
// once per dated version of the code rather than once each.
type CodeTree struct {
	// ID is the dated text version ("LEGITEXT000006072050_19-08-2026"). It is
	// the revision marker recorded as provenance, the live-source equivalent
	// of the pinned dataset revision §5.1 requires on bootstrap material.
	ID string `json:"id"`
	// CID is the stable text identity ("LEGITEXT000006072050").
	CID   string `json:"cid"`
	Title string `json:"title"`
	// ModifDate is the code's last modification date, as "2026-08-19".
	ModifDate string        `json:"modifDate"`
	Sections  []CodeSection `json:"sections"`
	Articles  []CodeArticle `json:"articles"`
}

// CodeSection is one level of a code's structure. It recurses.
type CodeSection struct {
	Title    string        `json:"title"`
	Sections []CodeSection `json:"sections"`
	Articles []CodeArticle `json:"articles"`
}

// CodeArticle is one article as it appears inside a CodeTree.
//
// Content is the article body as Légifrance stores it: HTML, not plain text —
// 702 of the 705 employment-law articles carry markup, including the tables
// that hold indemnity scales. Text() is the plain-text rendering; Content is
// left untouched so nothing is lost.
type CodeArticle struct {
	// ID is the LEGIARTI version id; CID is the stable article identity that
	// survives across versions. They map onto legal_versions.source_version
	// and legal_documents.source_id respectively.
	ID  string `json:"id"`
	CID string `json:"cid"`
	// Num is the article number as lawyers cite it ("L1232-1").
	Num  string `json:"num"`
	Etat string `json:"etat"`
	// Content and Nota are HTML.
	Content string `json:"content"`
	Nota    string `json:"nota"`
	// PathTitle is the section path from the code root down to the article,
	// carried by the API on the article itself. Its entries arrive padded
	// with trailing spaces; SectionPath trims them.
	PathTitle []string `json:"pathTitle"`
	// DateDebut and DateFin are LEGI millisecond dates. Validity converts them.
	DateDebut int64 `json:"dateDebut"`
	DateFin   int64 `json:"dateFin"`
}

// Validity converts the LEGI millisecond dates into the unix-second half-open
// interval legal_versions stores. A nil ValidTo means "still in force".
func (a CodeArticle) Validity() (validFrom *int64, validTo *int64) {
	return Article{DateDebut: a.DateDebut, DateFin: a.DateFin}.Validity()
}

// SectionPath is PathTitle with the API's padding removed and empty levels
// dropped.
func (a CodeArticle) SectionPath() []string {
	out := make([]string, 0, len(a.PathTitle))
	for _, p := range a.PathTitle {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Text renders Content as plain text. See htmlText.
func (a CodeArticle) Text() string { return htmlText(a.Content) }

// NotaText renders Nota as plain text.
func (a CodeArticle) NotaText() string { return htmlText(a.Nota) }

// FlatArticles walks the tree depth-first and returns every article in it.
// Order is the code's own reading order; callers that need a stable cursor
// sort it themselves.
func (t CodeTree) FlatArticles() []CodeArticle {
	var out []CodeArticle
	var walk func(sections []CodeSection, articles []CodeArticle)
	walk = func(sections []CodeSection, articles []CodeArticle) {
		out = append(out, articles...)
		for _, s := range sections {
			walk(s.Sections, s.Articles)
		}
	}
	walk(t.Sections, t.Articles)
	return out
}

// LegiPart fetches one consolidated code at a date. A zero date means today,
// which is correct for a corpus refresh and wrong for a dispute about an
// earlier period (§6.4) — the caller decides.
func (l *Legifrance) LegiPart(ctx context.Context, textID string, at time.Time) (CodeTree, error) {
	if !l.Configured() {
		return CodeTree{}, ErrLegifranceNotConfigured
	}
	if strings.TrimSpace(textID) == "" {
		return CodeTree{}, errors.New("legifrance: empty text id")
	}
	if at.IsZero() {
		at = time.Now()
	}

	var tree CodeTree
	if err := l.piste.call(ctx, http.MethodPost, l.baseURL+"/consult/legiPart",
		map[string]any{"textId": textID, "date": at.Format("2006-01-02")}, &tree); err != nil {
		return CodeTree{}, err
	}
	if tree.CID == "" {
		return CodeTree{}, fmt.Errorf("legifrance: code %s not found", textID)
	}
	legifranceLog.Infof("Légifrance code fetched (text=%s revision=%s modified=%s articles=%d)",
		textID, tree.ID, tree.ModifDate, len(tree.FlatArticles()))
	return tree, nil
}

// htmlText renders a Légifrance HTML body as plain text.
//
// A tokenizer rather than a tag-stripping regexp because the bodies contain
// tables — indemnity scales, notice-period grids — and mashing their cells
// into one run of words would corrupt text that ends up quoted in a pleading.
// Block boundaries become newlines, cells are separated, entities are decoded.
func htmlText(fragment string) string {
	if strings.TrimSpace(fragment) == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(fragment))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapseBlankLines(b.String())
		case html.TextToken:
			b.Write(z.Text())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "br", "p", "div", "tr", "table", "center":
				b.WriteString("\n")
			case "td", "th":
				b.WriteString("\t")
			}
		}
	}
}

// collapseBlankLines trims each line and squeezes runs of blank lines to one,
// so the paragraph breaks chunkText splits on stay meaningful.
func collapseBlankLines(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if blank || len(out) == 0 {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
