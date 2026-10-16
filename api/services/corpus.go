package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/fixtures"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var corpusLog = u.NewLogger("services.corpus")

// The public legal corpus (docs/PLAN.md §5). Sources are open-licence only:
// Légifrance and Judilibre via PISTE, DILA bulk, JADE. Nomos ships NO
// connector for Dalloz, LexisNexis, Éditions Francis Lefebvre, Lamy or
// Doctrine.fr — those are paywalled, their ToS forbid scraping, and their
// commentary is copyrighted. Attribution, update dates and licence version
// must survive into the output (Licence Ouverte 2.0, §13).

// SourceFixtures is the committed offline corpus in api/fixtures. It is not in
// types.Source* because it is not a remote connector: it is the corpus that
// makes a credential-less checkout — and the relevance evaluation harness —
// work at all. Move it into types/nomos_const.go the next time that file is
// edited by whoever owns it.
const SourceFixtures = "fixtures"

// legalKindArticle / legalKindDecision are the legal_documents.kind values.
const (
	legalKindArticle  = "article"
	legalKindDecision = "decision"
)

const (
	// chunkTargetChars ≈ 800 tokens of French at ~4 characters per token, and
	// chunkOverlapChars ≈ 100 tokens (§7.1). Character budgets, not a
	// tokenizer: the corpus is chunked once and the retrieval unit only has
	// to be *about* the right size.
	chunkTargetChars  = 3200
	chunkOverlapChars = 400
	// charsPerToken is the estimate written to legal_chunks.token_count. It
	// is a shape for capacity planning, never a billing figure — the audited
	// token counts live in llm_calls (§4.6).
	charsPerToken = 4

	// articlesPerBatch bounds one transaction plus one embedding call. Small
	// enough that a pod death costs one batch, big enough that the cursor is
	// not rewritten per row.
	articlesPerBatch = 32

	// syncTimeout bounds a whole sync run. The corpus is one practice area
	// (§5.3), so a run that has not finished in an hour is stuck, not slow.
	syncTimeout = time.Hour

	// syncLease bounds how long a claim survives WITHOUT PROGRESS. Every batch
	// stamps last_run_at (persistProgress), so this is a no-progress bound
	// rather than a run bound, the same shape as the run-step lease and its
	// heartbeat (§4.2). Generous next to one batch — 32 articles and one
	// embedding call — and short enough that a pod restarted after an OOM
	// resumes the seed in minutes instead of never.
	syncLease = 10 * time.Minute
)

// SyncCorpus starts or resumes a corpus load, resumable from corpus_syncs.cursor.
//
// MVP corpus is ONE PRACTICE AREA — employment law (§5.3): the Code du
// travail, the Code civil and CPC provisions the workflow uses, and published
// Cass. soc. decisions from 2018. That is ~10^5 chunks and a few GB. "All of
// French law" is ~10^7 vectors and 80–150 GB, which is incompatible with
// AutoMigrate-at-boot index creation; everything outside the corpus is fetched
// live and cached on retrieval instead.
//
// Bootstrap material (the pinned AgentPublic/legi revision) must record its
// dataset revision and every source hash: it is a bootstrap snapshot, not a
// current legal state, and it mixes article statuses.
//
// Judilibre disclaims completeness, freshness and reliability, and DILA is
// explicit that the opposable text is the authenticated Légifrance
// publication. Anything indexed here is a CACHE; export-time revalidation is
// what makes a citation trustworthy (§5.1, §6.4).
//
// RESUMABLE AND IDEMPOTENT, by construction:
//
//   - resumable — corpus_syncs.cursor advances once per committed batch, and a
//     run that resumes reads it back and continues from there;
//   - idempotent — legal_documents.id and legal_versions.id are DERIVED from
//     (source, source_id) and (document, source_version), so a replay lands on
//     the same primary keys, and every write converges on
//     uniq(source, source_id), uniq(legal_document_id, source_version) and
//     uniq(legal_version_id, ordinal). Re-running produces identical row
//     counts, never duplicates. A stored version is never rewritten: a
//     changed official text becomes a separate revision (upsertVersions).
//
// The claim itself is a conditional UPDATE off the current state, so a second
// admin click — or a second replica — gets 409 instead of a second loader
// racing the first over the same rows.
func SyncCorpus(c *gin.Context, body types.CorpusSyncRequest) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}

	source := strings.TrimSpace(body.Source)
	loader, status, detail := loaderFor(source)
	if loader == nil {
		return status, gin.H{"detail": detail}
	}

	ctx := c.Request.Context()

	claimed, won, err := claimSync(ctx, source, body.Full)
	if err != nil {
		return http.StatusInternalServerError, gin.H{"detail": err.Error()}
	}
	if !won {
		return http.StatusConflict, gin.H{"detail": "A sync for this source is already running"}
	}

	// The load outlives the request. context.WithoutCancel is deliberate: the
	// claim is already committed, so a browser disconnect must not leave the
	// row stuck in `running` with nothing working on it.
	go func(cursor string) {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
		defer cancel()
		runSync(bg, source, cursor, loader)
	}(claimed.Cursor)

	corpusLog.Infof("Corpus sync started (source=%s full=%t cursor=%q)", source, body.Full, claimed.Cursor)
	return http.StatusAccepted, gin.H{
		"source": source,
		"state":  types.CorpusSyncRunning,
		"cursor": claimed.Cursor,
		"full":   body.Full,
	}
}

// CorpusStatus reports the sync cursors plus the corpus counters an admin
// needs to confirm §5.3's sizing still holds — chunk count is the tripwire for
// moving HNSW creation out of boot and into an explicit admin job.
func CorpusStatus(c *gin.Context) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	db := u.DB.WithContext(c.Request.Context())

	var resp types.CorpusStatusResponse
	if err := db.Order("source").Find(&resp.Syncs).Error; err != nil {
		corpusLog.Errorf("Corpus status: read syncs failed: %v", err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to read the corpus status"}
	}
	db.Model(&types.LegalDocument{}).Count(&resp.Documents)
	db.Model(&types.LegalVersion{}).Count(&resp.Versions)
	db.Model(&types.LegalChunk{}).Count(&resp.Chunks)
	db.Model(&types.LegalChunk{}).Where("embedding IS NOT NULL").Count(&resp.Embedded)

	// index_ready is the §9.1(10) tripwire: retrieval degrades to a sequential
	// scan without these two, and the admin is the one who has to know.
	var indexes int64
	db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'legal_chunks'
	          AND indexname IN ('idx_legal_chunks_embedding','idx_legal_chunks_fts')`).Scan(&indexes)
	resp.IndexReady = indexes == 2

	corpusLog.Infof("Corpus status: %d documents, %d versions, %d chunks (%d embedded), index_ready=%t",
		resp.Documents, resp.Versions, resp.Chunks, resp.Embedded, resp.IndexReady)
	return http.StatusOK, resp
}

// ---------------------------------------------------------------------------
// The loader
// ---------------------------------------------------------------------------

// claimSync takes exclusive ownership of one source's cursor row and returns
// it. won is false when another worker still holds a LIVE claim.
//
// The claim is a CONDITIONAL UPDATE off the current state, which is what makes
// it safe for more than one caller: a second admin click, a second replica, or
// the boot-time seed racing an operator all land on RowsAffected = 0 instead of
// a second loader walking the same rows.
//
// The claim also EXPIRES, which is the half that makes it a lease. A worker
// killed mid-run — OOM, eviction, node loss — never reaches finishSync, so a
// claim only its owner could release would pin the row at `running` forever
// and every later boot would skip the source with "another worker holds the
// sync". Liveness is last_run_at, refreshed by every batch, and syncLease is
// the no-progress bound. A stalled-but-alive worker can therefore be
// overtaken; that is the accepted at-least-once trade (§2.3), and the loader's
// writes are upserts on derived ids, so the overlap costs work, not rows.
//
// Two statements rather than one upsert-with-condition because the claim must
// be able to report "it was already running" distinctly from "it did not exist".
func claimSync(ctx context.Context, source string, full bool) (types.CorpusSync, bool, error) {
	db := u.DB.WithContext(ctx)

	row := types.CorpusSync{ID: source, Source: source, State: types.CorpusSyncIdle}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "source"}},
		DoNothing: true,
	}).Create(&row).Error; err != nil {
		corpusLog.Errorf("Corpus sync row upsert failed (source=%s): %v", source, err)
		return types.CorpusSync{}, false, errors.New("Failed to record the sync")
	}

	updates := map[string]any{
		"state":       types.CorpusSyncRunning,
		"last_run_at": time.Now().Unix(),
		"last_error":  "",
	}
	if full {
		updates["cursor"] = ""
	}
	stale := time.Now().Add(-syncLease).Unix()
	claim := db.Model(&types.CorpusSync{}).
		Where("source = ? AND (state <> ? OR COALESCE(last_run_at, 0) < ?)",
			source, types.CorpusSyncRunning, stale).
		Updates(updates)
	if claim.Error != nil {
		corpusLog.Errorf("Corpus sync claim failed (source=%s): %v", source, claim.Error)
		return types.CorpusSync{}, false, errors.New("Failed to claim the sync")
	}
	if claim.RowsAffected == 0 {
		return types.CorpusSync{}, false, nil
	}

	var claimed types.CorpusSync
	if err := db.Where("source = ?", source).First(&claimed).Error; err != nil {
		corpusLog.Errorf("Corpus sync reload failed (source=%s): %v", source, err)
		return types.CorpusSync{}, false, errors.New("Failed to read the sync cursor")
	}
	return claimed, true, nil
}

// loadStats is what a run reports and what corpus_syncs.stats records.
type loadStats struct {
	Articles int `json:"articles"`
	// Decisions is counted separately from Articles: a corpus report that
	// merged them could not answer "is there any case law in here", which is
	// exactly the question a note de synthèse depends on.
	Decisions int    `json:"decisions"`
	Documents int    `json:"documents"`
	Versions  int    `json:"versions"`
	Chunks    int    `json:"chunks"`
	Embedded  int    `json:"embedded"`
	License   string `json:"license"`
	// Attribution travels with the numbers on purpose: Licence Ouverte 2.0
	// permits reuse WITH ATTRIBUTION, so the admin screen that reads this row
	// is the last place it can still be surfaced (§13).
	Attribution string `json:"attribution"`
	Provenance  string `json:"provenance"`
}

// loader loads one source, resuming from cursor and returning the cursor to
// persist next. done reports whether the source is fully loaded.
type loader func(ctx context.Context, cursor string, stats *loadStats) (next string, done bool, err error)

// loaderFor resolves a source name, or the HTTP status and detail to return.
//
// The PISTE sources degrade CLEANLY here: with empty credentials they answer
// 503 with the exact detail string the connector's sentinel error carries,
// rather than starting a run that would fail on its first token exchange.
func loaderFor(source string) (loader, int, string) {
	cfg := config.Load()

	switch source {
	case SourceFixtures:
		return loadFixtures, 0, ""

	case types.SourceLegifrance:
		lf := connectors.NewLegifrance(pisteOptions(cfg, cfg.LegifranceBaseURL))
		if !lf.Configured() {
			return nil, http.StatusServiceUnavailable, connectors.ErrLegifranceNotConfigured.Error()
		}
		return legifranceLoader(lf), 0, ""

	case types.SourceJudilibre:
		jl := connectors.NewJudilibre(pisteOptions(cfg, cfg.JudilibreBaseURL))
		if !jl.Configured() {
			return nil, http.StatusServiceUnavailable, connectors.ErrJudilibreNotConfigured.Error()
		}
		return judilibreLoader(jl), 0, ""

	case types.SourceLegiHF:
		return nil, http.StatusNotImplemented, "AgentPublic/legi bootstrap sync is not implemented yet"

	default:
		return nil, http.StatusBadRequest, fmt.Sprintf("Unknown corpus source %q", source)
	}
}

// pisteOptions builds the connector options for one PISTE API. Légifrance and
// Judilibre share the credential pair and the token endpoint but sit behind
// different base paths, so the base URL is the per-API argument. Empty
// overrides leave the connector on its production default.
func pisteOptions(cfg config.Config, baseURL string) connectors.Options {
	return connectors.Options{
		ClientID:     cfg.PisteClientID,
		ClientSecret: cfg.PisteClientSecret,
		TokenURL:     cfg.PisteTokenURL,
		BaseURL:      baseURL,
	}
}

// runSync drives one source to completion, persisting the cursor after every
// committed batch so a pod death costs at most one batch of work.
func runSync(ctx context.Context, source, cursor string, load loader) {
	started := time.Now()
	stats := loadStats{}

	for {
		next, done, err := load(ctx, cursor, &stats)
		if err != nil {
			corpusLog.Errorf("Corpus sync failed (source=%s cursor=%q): %v", source, cursor, err)
			finishSync(ctx, source, cursor, stats, types.CorpusSyncFailed, err.Error())
			return
		}
		cursor = next
		persistProgress(ctx, source, cursor, stats)
		if done {
			break
		}
		if ctx.Err() != nil {
			corpusLog.Warnf("Corpus sync interrupted (source=%s cursor=%q): %v", source, cursor, ctx.Err())
			finishSync(ctx, source, cursor, stats, types.CorpusSyncFailed, ctx.Err().Error())
			return
		}
	}

	finishSync(ctx, source, cursor, stats, types.CorpusSyncIdle, "")
	corpusLog.Infof("Corpus sync completed (source=%s articles=%d decisions=%d documents=%d versions=%d chunks=%d embedded=%d in %s)",
		source, stats.Articles, stats.Decisions, stats.Documents, stats.Versions, stats.Chunks, stats.Embedded,
		time.Since(started).Round(time.Millisecond))
}

// persistProgress writes the resume point after every batch. last_run_at goes
// with it: progress IS the claim's heartbeat, and claimSync only overtakes a
// `running` row that has not moved for syncLease.
func persistProgress(ctx context.Context, source, cursor string, stats loadStats) {
	updates := map[string]any{
		"cursor":      cursor,
		"stats":       encodeStats(stats),
		"last_run_at": time.Now().Unix(),
	}
	if err := u.DB.WithContext(ctx).Model(&types.CorpusSync{}).
		Where("source = ?", source).Updates(updates).Error; err != nil {
		corpusLog.Warnf("Corpus cursor write failed (source=%s): %v", source, err)
	}
}

func finishSync(ctx context.Context, source, cursor string, stats loadStats, state, lastErr string) {
	updates := map[string]any{
		"cursor":      cursor,
		"state":       state,
		"stats":       encodeStats(stats),
		"last_error":  lastErr,
		"last_run_at": time.Now().Unix(),
	}
	if err := u.DB.WithContext(ctx).Model(&types.CorpusSync{}).
		Where("source = ?", source).Updates(updates).Error; err != nil {
		corpusLog.Errorf("Corpus sync finalisation failed (source=%s): %v", source, err)
	}
}

func encodeStats(s loadStats) types.JSONB {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return types.JSONB(raw)
}

// loadFixtures loads the committed offline corpus (api/fixtures).
//
// The cursor is the index of the next article to process. That is safe here
// precisely because the corpus is a COMMITTED file: its order cannot change
// under a running sync the way a remote API's pagination can. A remote source
// must use the API's own cursor instead — Judilibre's next_batch, Légifrance's
// page number.
func loadFixtures(ctx context.Context, cursor string, stats *loadStats) (string, bool, error) {
	corpus, err := fixtures.CodeDuTravail()
	if err != nil {
		return cursor, false, err
	}
	stats.License = corpus.License.Code
	stats.Attribution = corpus.License.Attribution
	stats.Provenance = corpus.Source.Snapshot + " sha256:" + corpus.Source.SnapshotSHA256

	start, _ := strconv.Atoi(cursor)
	if start < 0 || start > len(corpus.Articles) {
		start = 0
	}
	if start >= len(corpus.Articles) {
		return cursor, true, nil
	}
	end := min(start+articlesPerBatch, len(corpus.Articles))

	batch := corpus.Articles[start:end]
	docs := make([]types.LegalDocument, 0, len(batch))
	versions := make([]types.LegalVersion, 0, len(batch))
	var pending []pendingChunk

	for _, a := range batch {
		docID := derivedID(SourceFixtures, a.DocumentID)
		versionID := derivedID(docID, a.VersionID)

		docs = append(docs, types.LegalDocument{
			ID:            docID,
			Source:        SourceFixtures,
			SourceID:      a.DocumentID,
			Kind:          "article",
			Jurisdiction:  "fr",
			CanonicalURL:  a.CanonicalURL,
			CitationLabel: a.CitationLabel,
			LicenseCode:   corpus.License.Code,
		})
		versions = append(versions, types.LegalVersion{
			ID:              versionID,
			LegalDocumentID: docID,
			SourceVersion:   a.VersionID,
			Text:            a.Text,
			TextSHA256:      a.TextSHA256,
			ValidFrom:       a.ValidFrom,
			ValidTo:         a.ValidTo,
			State:           types.NormalizeLegalVersionState(a.State),
			SourceMeta:      articleMeta(corpus, a),
		})
		for i, text := range chunkText(retrievalText(a)) {
			pending = append(pending, pendingChunk{versionID: versionID, ordinal: i, text: text})
		}
	}

	// Embed the whole batch in one gateway call. Best effort on purpose: a
	// chunk with a NULL embedding is still findable by the french FTS arm of
	// the RRF fusion, so an LLM gateway outage degrades retrieval instead of
	// aborting the load. CorpusStatus.Embedded is what makes the gap visible.
	embedded := embedChunks(ctx, pending)

	err = u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return persistBatch(tx, docs, versions, pending)
	})
	if err != nil {
		return cursor, false, err
	}

	stats.Articles += len(batch)
	stats.Documents += len(docs)
	stats.Versions += len(versions)
	stats.Chunks += len(pending)
	stats.Embedded += embedded
	corpusLog.Debugf("Fixture batch loaded (articles=%d..%d chunks=%d embedded=%d)", start, end, len(pending), embedded)

	return strconv.Itoa(end), end >= len(corpus.Articles), nil
}

type pendingChunk struct {
	versionID string
	ordinal   int
	text      string
	embedding *pgvector.HalfVector
	model     string
}

func embedChunks(ctx context.Context, chunks []pendingChunk) int {
	if len(chunks) == 0 || !u.LLMConfigured() {
		return 0
	}
	inputs := make([]string, len(chunks))
	for i, ch := range chunks {
		inputs[i] = ch.text
	}
	cfg := config.Load()
	vectors, err := u.Embed(ctx, cfg.EmbeddingModel, inputs)
	if err != nil {
		corpusLog.Warnf("Corpus embedding failed for %d chunks, loading them without vectors: %v", len(chunks), err)
		return 0
	}
	n := 0
	for i := range chunks {
		if i >= len(vectors) || len(vectors[i]) == 0 {
			continue
		}
		v := pgvector.NewHalfVector(vectors[i])
		chunks[i].embedding = &v
		chunks[i].model = cfg.EmbeddingModel
		n++
	}
	return n
}

func upsertDocuments(tx *gorm.DB, docs []types.LegalDocument) error {
	if len(docs) == 0 {
		return nil
	}
	// first_seen_at is deliberately NOT updated: it is when Nomos first saw
	// the document, and a re-run must not rewrite history.
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source"}, {Name: "source_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"kind", "jurisdiction", "canonical_url", "citation_label", "license_code", "source_updated_at",
		}),
	}).Create(&docs).Error
}

// persistBatch writes one loader batch in one transaction: documents, the
// immutable versions, then the chunks of whichever row now holds each
// version's text.
func persistBatch(tx *gorm.DB, docs []types.LegalDocument, versions []types.LegalVersion, chunks []pendingChunk) error {
	if err := upsertDocuments(tx, docs); err != nil {
		return err
	}
	stored, err := upsertVersions(tx, versions)
	if err != nil {
		return err
	}
	for i := range chunks {
		chunks[i].versionID = stored[chunks[i].versionID]
	}
	return upsertChunks(tx, chunks)
}

// upsertVersions stores versions without ever rewriting one (§6.3, §6.4): a
// citation rests on the exact text of the row it names, so that text cannot
// move under it. For each version it returns the id of the row holding its
// text, keyed by the version's own id:
//
//   - new identity: inserted;
//   - same identity, same text hash: converges on the stored row, no write;
//   - same identity, different text: a SEPARATE revision row, whose source
//     version is "<official id>@<hash prefix>" and whose metadata keeps the
//     official identifier and the row it revises. The read_law/read_decision
//     path (storeLegalVersion) derives the same identity, so a sync and an
//     on-demand read of one changed text converge on one revision.
//
// Validity and state stay as first stored: the official revalidation at
// publication (citation_sources.go) is what checks a cited version against
// the official source.
func upsertVersions(tx *gorm.DB, versions []types.LegalVersion) (map[string]string, error) {
	stored := make(map[string]string, len(versions))
	if len(versions) == 0 {
		return stored, nil
	}
	held, err := insertVersionsOnce(tx, versions)
	if err != nil {
		return nil, err
	}
	var revisions []types.LegalVersion
	revisionOf := map[string]string{} // revision id → input version id
	for _, v := range versions {
		row := held[versionKey(v)]
		if row.TextSHA256 == v.TextSHA256 {
			stored[v.ID] = row.ID
			continue
		}
		rev := v
		rev.SourceVersion = v.SourceVersion + "@" + v.TextSHA256[:min(16, len(v.TextSHA256))]
		rev.ID = derivedID(v.LegalDocumentID, rev.SourceVersion)
		rev.SourceMeta = revisionMeta(v.SourceMeta, v.SourceVersion, row.ID)
		revisions = append(revisions, rev)
		revisionOf[rev.ID] = v.ID
	}
	if len(revisions) == 0 {
		return stored, nil
	}
	heldRevisions, err := insertVersionsOnce(tx, revisions)
	if err != nil {
		return nil, err
	}
	for _, rev := range revisions {
		row := heldRevisions[versionKey(rev)]
		if row.TextSHA256 != rev.TextSHA256 {
			return nil, fmt.Errorf("legal version %s: revision %s holds another text", revisionOf[rev.ID], row.ID)
		}
		stored[revisionOf[rev.ID]] = row.ID
	}
	corpusLog.Infof("Changed official text stored as separate revisions (revisions=%d batch=%d)", len(revisions), len(versions))
	return stored, nil
}

// insertVersionsOnce inserts the versions whose identity is new and returns
// the row now holding each identity — the existing one where there was one.
func insertVersionsOnce(tx *gorm.DB, versions []types.LegalVersion) (map[string]types.LegalVersion, error) {
	if err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "legal_document_id"}, {Name: "source_version"}},
		DoNothing: true,
	}).Create(&versions).Error; err != nil {
		return nil, err
	}
	keys := make([][]any, 0, len(versions))
	for _, v := range versions {
		keys = append(keys, []any{v.LegalDocumentID, v.SourceVersion})
	}
	var rows []types.LegalVersion
	if err := tx.Select("id", "legal_document_id", "source_version", "text_sha256").
		Where("(legal_document_id, source_version) IN ?", keys).Find(&rows).Error; err != nil {
		return nil, err
	}
	held := make(map[string]types.LegalVersion, len(rows))
	for _, r := range rows {
		held[versionKey(r)] = r
	}
	for _, v := range versions {
		if _, ok := held[versionKey(v)]; !ok {
			return nil, fmt.Errorf("legal version %s: not stored", v.ID)
		}
	}
	return held, nil
}

func versionKey(v types.LegalVersion) string { return v.LegalDocumentID + "\x00" + v.SourceVersion }

// revisionMeta is a revision's source metadata: the incoming one, plus the
// official identifier it was published under and the stored row it revises.
func revisionMeta(raw types.JSONB, officialVersion, revises string) types.JSONB {
	meta := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &meta)
	}
	meta["official_source_version"] = officialVersion
	meta["revision_of"] = revises
	out, err := json.Marshal(meta)
	if err != nil {
		return raw
	}
	return types.JSONB(out)
}

// upsertChunks writes legal_chunks with raw SQL for one reason: search_vector
// must be `to_tsvector('french', unaccent($text))`, and GORM would bind a Go
// string straight into the tsvector column, where Postgres parses it as a
// tsvector LITERAL rather than analysing it. §6.2 also rules out the two
// alternatives — a generated column and a trigger — because the no-DDL
// constraint can create neither.
//
// unaccent is applied on the WRITE side, so the query side must apply it too:
// `websearch_to_tsquery('french', unaccent($q))`.
//
// A stored chunk's text is never rewritten — it belongs to an immutable
// version. A replay only fills the vector of a chunk stored without one
// (a gateway outage during an earlier load), and only for identical text.
func upsertChunks(tx *gorm.DB, chunks []pendingChunk) error {
	now := time.Now().Unix()
	for _, ch := range chunks {
		sum := sha256.Sum256([]byte(ch.text))
		err := tx.Exec(`
			INSERT INTO legal_chunks
			  (legal_version_id, ordinal, text, text_sha256, search_vector,
			   embedding, embedding_model, token_count, created_at)
			VALUES (?, ?, ?, ?, to_tsvector('french', unaccent(?)), ?, ?, ?, ?)
			ON CONFLICT (legal_version_id, ordinal) DO UPDATE SET
			  embedding = EXCLUDED.embedding,
			  embedding_model = EXCLUDED.embedding_model
			WHERE legal_chunks.embedding IS NULL
			  AND EXCLUDED.embedding IS NOT NULL
			  AND legal_chunks.text_sha256 = EXCLUDED.text_sha256`,
			ch.versionID, ch.ordinal, ch.text, hex.EncodeToString(sum[:]), ch.text,
			ch.embedding, ch.model, len([]rune(ch.text))/charsPerToken, now).Error
		if err != nil {
			return fmt.Errorf("upsert chunk %s#%d: %w", ch.versionID, ch.ordinal, err)
		}
	}
	return nil
}

// derivedID makes the primary key a pure function of the natural key, which is
// what makes a replay converge instead of duplicating. Truncated to 32 bytes
// of hex — the column is size:64 and 128 bits of a sha256 is far past any
// collision risk for a corpus of ~10^5 rows.
func derivedID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// retrievalText is what gets embedded and indexed. It leads with the citation
// label and the section path because a lawyer searches for "L1232-1" and for
// "licenciement pour motif personnel", and the bare article body contains
// neither. legal_versions.text keeps the untouched official text.
func retrievalText(a fixtures.Article) string {
	return retrievalTextFor(a.CitationLabel, a.SectionPath, a.Text)
}

func retrievalTextFor(citationLabel string, sectionPath []string, text string) string {
	var b strings.Builder
	b.WriteString(citationLabel)
	if len(sectionPath) > 0 {
		b.WriteString(" — ")
		b.WriteString(strings.Join(sectionPath, " › "))
	}
	b.WriteString("\n\n")
	b.WriteString(text)
	return b.String()
}

// chunkText splits on paragraph boundaries into ~chunkTargetChars pieces with
// chunkOverlapChars of overlap (§7.1). Most Code du travail articles are one
// chunk; the long procedural ones are not.
func chunkText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= chunkTargetChars {
		return []string{text}
	}

	var out []string
	for start := 0; start < len(runes); {
		end := min(start+chunkTargetChars, len(runes))
		if end < len(runes) {
			// Prefer a paragraph break, then a sentence end, inside the last
			// quarter of the window — a chunk cut mid-sentence retrieves badly.
			if cut := lastBreak(runes[start:end]); cut > chunkTargetChars*3/4 {
				end = start + cut
			}
		}
		out = append(out, strings.TrimSpace(string(runes[start:end])))
		if end >= len(runes) {
			break
		}
		start = end - chunkOverlapChars
	}
	return out
}

func lastBreak(window []rune) int {
	s := string(window)
	if i := strings.LastIndex(s, "\n\n"); i > 0 {
		return len([]rune(s[:i])) + 2
	}
	if i := strings.LastIndex(s, ". "); i > 0 {
		return len([]rune(s[:i])) + 2
	}
	return 0
}

// articleMeta records the provenance §5.1 requires on bootstrap material: the
// snapshot it came from, its content hash, and the reuse terms. Without them a
// chunk cannot be traced back to what was actually ingested.
func articleMeta(corpus fixtures.Corpus, a fixtures.Article) types.JSONB {
	meta := map[string]any{
		"corpus_id":       corpus.CorpusID,
		"article_number":  a.Number,
		"section_path":    a.SectionPath,
		"snapshot":        corpus.Source.Snapshot,
		"snapshot_sha256": corpus.Source.SnapshotSHA256,
		"license":         corpus.License.Code,
		"attribution":     corpus.License.Attribution,
		"source_url":      corpus.License.SourceURL,
		"authority":       "cache — the opposable text is the authenticated Légifrance publication (§6.4)",
	}
	if a.Nota != nil {
		meta["nota"] = *a.Nota
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return types.JSONB(raw)
}

// ---------------------------------------------------------------------------
// Légifrance (§5.1, §5.3)
// ---------------------------------------------------------------------------

// CodeDuTravailTextID is the Code du travail in LEGI.
const CodeDuTravailTextID = "LEGITEXT000006072050"

// employmentFamilies is the corpus scope: the article families of the
// individual employment dispute (§5.3). It is the SAME scope the offline
// fixture generator uses (api/fixtures/generate.py), so the live corpus and
// the fixture corpus describe the same practice area and the relevance
// evaluations stay comparable.
//
// Widening this is a deliberate decision, not a convenience. The whole Code du
// travail is 11 833 articles in force; these 33 families are ~705 of them, and
// "all of LEGI" is ~10^7 vectors and 80–150 GB against a 4 GiB default volume.
var employmentFamilies = []string{
	"L1132", "L1152", "L1153", "L1221", "L1222", "L1225", "L1226", "L1231", "L1232", "L1233",
	"L1234", "L1235", "L1237", "L1242", "L1243", "L1245", "L1332", "L1411", "L1451", "L1453",
	"L1454", "L1471", "L3121", "L3123", "L3141", "L3221", "L3245", "L3251",
	"R1234", "R1452", "R1454", "R1455", "R1461",
}

// legifranceAttribution is what Licence Ouverte 2.0 requires to travel with the
// data (§13). It mirrors the fixture corpus' licence header.
const legifranceAttribution = "Code du travail — Direction de l'information légale et administrative (DILA) / " +
	"Légifrance, Licence Ouverte 2.0. Réutilisation autorisée, y compris commercialement, " +
	"sous réserve de mentionner la source et sa date de dernière mise à jour."

// legifranceLoader loads the employment-law scope of the Code du travail.
//
// The whole code arrives in ONE /consult/legiPart call, so the tree is fetched
// on the first batch and held for the rest of the run: re-fetching 32 MB for
// each batch of 32 articles would move 700 MB to load 705 articles. A resumed
// run in a fresh process re-fetches and seeks — which is why the cursor is an
// ARTICLE NUMBER and not an index. An index into a remote list is only safe
// when the list cannot change under the run (loadFixtures can rely on that
// because its corpus is a committed file; this one cannot).
func legifranceLoader(lf *connectors.Legifrance) loader {
	var articles []connectors.CodeArticle
	var revision, modified string

	return func(ctx context.Context, cursor string, stats *loadStats) (string, bool, error) {
		if articles == nil {
			tree, err := lf.LegiPart(ctx, CodeDuTravailTextID, time.Time{})
			if err != nil {
				return cursor, false, err
			}
			revision, modified = tree.ID, tree.ModifDate
			articles = scopeToEmployment(tree.FlatArticles())
			if len(articles) == 0 {
				return cursor, false, fmt.Errorf("legifrance: no in-force article matched the %d employment families", len(employmentFamilies))
			}
			corpusLog.Infof("Légifrance scope resolved (revision=%s modified=%s articles=%d families=%d)",
				revision, modified, len(articles), len(employmentFamilies))
		}

		stats.License = connectors.LicenceOuverte2
		stats.Attribution = legifranceAttribution
		stats.Provenance = revision + " (modifié le " + modified + ")"

		// Resume past the last article committed, by identity rather than by
		// position, so an article added or repealed upstream between two runs
		// shifts nothing.
		start := 0
		if cursor != "" {
			start = sort.Search(len(articles), func(i int) bool { return articleCursor(articles[i]) > cursor })
		}
		if start >= len(articles) {
			return cursor, true, nil
		}
		end := min(start+articlesPerBatch, len(articles))
		batch := articles[start:end]

		docs := make([]types.LegalDocument, 0, len(batch))
		versions := make([]types.LegalVersion, 0, len(batch))
		var pending []pendingChunk

		for _, a := range batch {
			text := a.Text()
			if text == "" {
				// An article with no body is a structural placeholder; it has
				// nothing to retrieve and no text to cite.
				corpusLog.Debugf("Légifrance article skipped, empty body (num=%s id=%s)", a.Num, a.ID)
				continue
			}
			docID := derivedID(types.SourceLegifrance, a.CID)
			versionID := derivedID(docID, a.ID)
			sum := sha256.Sum256([]byte(text))
			validFrom, validTo := a.Validity()
			sectionPath := a.SectionPath()

			docs = append(docs, types.LegalDocument{
				ID:            docID,
				Source:        types.SourceLegifrance,
				SourceID:      a.CID,
				Kind:          "article",
				Jurisdiction:  "fr",
				CanonicalURL:  connectors.CanonicalURL(a.ID),
				CitationLabel: "C. trav., art. " + a.Num,
				LicenseCode:   connectors.LicenceOuverte2,
			})
			versions = append(versions, types.LegalVersion{
				ID:              versionID,
				LegalDocumentID: docID,
				SourceVersion:   a.ID,
				Text:            text,
				TextSHA256:      hex.EncodeToString(sum[:]),
				ValidFrom:       validFrom,
				ValidTo:         validTo,
				State:           types.NormalizeLegalVersionState(a.Etat),
				SourceMeta:      legifranceMeta(a, sectionPath, revision, modified),
			})
			for i, chunk := range chunkText(retrievalTextFor("C. trav., art. "+a.Num, sectionPath, text)) {
				pending = append(pending, pendingChunk{versionID: versionID, ordinal: i, text: chunk})
			}
		}

		embedded := embedChunks(ctx, pending)

		err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return persistBatch(tx, docs, versions, pending)
		})
		if err != nil {
			return cursor, false, err
		}

		stats.Articles += len(batch)
		stats.Documents += len(docs)
		stats.Versions += len(versions)
		stats.Chunks += len(pending)
		stats.Embedded += embedded
		corpusLog.Debugf("Légifrance batch loaded (articles=%s..%s count=%d chunks=%d embedded=%d)",
			batch[0].Num, batch[len(batch)-1].Num, len(batch), len(pending), embedded)

		// The cursor is only advanced after the batch has committed, so a pod
		// death costs one batch and never skips one.
		return articleCursor(batch[len(batch)-1]), end >= len(articles), nil
	}
}

// articleCursor is the resume key. Appending the LEGIARTI id to the number
// makes it total by construction rather than by luck: article numbers are
// unique in the current corpus only after deduplication, and the code's
// tripartite structure (ordre public / négociation collective / dispositions
// supplétives) is exactly the kind of thing that puts two provisions at
// neighbouring numbers.
//
// "#" is below "-" and below every digit in ASCII, so ordering the joined
// strings is the same as ordering the (Num, ID) pairs even when one number is a
// prefix of another ("L3121-2" before "L3121-24").
func articleCursor(a connectors.CodeArticle) string { return a.Num + "#" + a.ID }

// scopeToEmployment keeps the in-force articles of the employment families,
// deduplicates them, and puts them in a stable total order.
//
// Deduplication is not defensive tidying — the tree really does hand back the
// same article twice. Ten provisions of the working-time chapter (L3121-24,
// L3121-25 …) hang under both their pre-2016 heading and the "Dispositions
// supplétives" heading the 2016 restructuring introduced, with identical id and
// cid. Left in, each batch containing one becomes an INSERT with two rows
// sharing a conflict key, and Postgres rejects the whole statement with
// "ON CONFLICT DO UPDATE command cannot affect row a second time" — so the
// duplicate does not corrupt the corpus, it stops the load dead.
//
// The first occurrence wins, which is the code's own reading order.
//
// The order is lexical on articleCursor rather than the code's reading order:
// the cursor is a position in this sequence, so it needs an order the API
// cannot reshuffle between two runs. It is not the order a lawyer would read
// the code in, and it does not need to be.
func scopeToEmployment(all []connectors.CodeArticle) []connectors.CodeArticle {
	out := make([]connectors.CodeArticle, 0, 1024)
	seen := make(map[string]bool, len(all))
	for _, a := range all {
		if types.NormalizeLegalVersionState(a.Etat) != types.LegalVersionInForce {
			continue
		}
		if a.ID == "" || seen[a.ID] {
			continue
		}
		num := strings.ReplaceAll(a.Num, " ", "")
		for _, f := range employmentFamilies {
			if strings.HasPrefix(num, f) {
				seen[a.ID] = true
				a.Num = num
				out = append(out, a)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return articleCursor(out[i]) < articleCursor(out[j]) })
	return out
}

// legifranceMeta records what §5.1 requires to trace a chunk back to what was
// actually ingested: the code revision it was read at, the reuse terms, and
// the reminder that this row is a cache.
func legifranceMeta(a connectors.CodeArticle, sectionPath []string, revision, modified string) types.JSONB {
	meta := map[string]any{
		"article_number": a.Num,
		"section_path":   sectionPath,
		"text_id":        CodeDuTravailTextID,
		"code_revision":  revision,
		"code_modified":  modified,
		"license":        connectors.LicenceOuverte2,
		"attribution":    legifranceAttribution,
		"source_url":     "https://www.legifrance.gouv.fr/codes/texte_lc/" + CodeDuTravailTextID,
		"authority":      "cache — le texte opposable est la publication authentifiée de Légifrance (§6.4)",
	}
	if nota := a.NotaText(); nota != "" {
		meta["nota"] = nota
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return types.JSONB(raw)
}

// ---------------------------------------------------------------------------
// Boot-time seed (§5.3, §9.1(10))
// ---------------------------------------------------------------------------

// SeedCorpus loads the sources named by CORPUS_AUTOSEED_SOURCES if they are not
// already loaded, so a fresh cluster comes up with a usable corpus instead of an
// empty one waiting for someone to press a button.
//
// This is a DELIBERATE, BOUNDED exception to "loading the corpus is an admin
// action, never deployment behaviour". That rule exists because building an
// HNSW index at boot over millions of rows turns a rollout into a timeout — and
// it still holds at that size. What makes it safe here is the §5.3 scope: one
// practice area is ~700 chunks and ~45 seconds, and EnsureSearchIndexes already
// runs at boot for exactly the same reason. THE TRIPWIRE IS THE CHUNK COUNT: if
// the corpus ever approaches the full 10^7-chunk estimate, this must go back to
// being an explicit admin job, along with index creation.
//
// Four properties keep it out of the rollout's way:
//
//   - it never blocks readiness — main runs it in a goroutine, and the HTTP
//     server is listening long before the load finishes;
//   - it never runs twice — a source that has already completed is skipped, and
//     the conditional claim means only ONE replica of a scaled Deployment can
//     hold a source at a time;
//   - it resumes rather than restarts — a pod killed mid-load leaves a cursor,
//     and the next boot continues from it;
//   - it degrades quietly — no database, no credentials, or a source that is
//     not implemented yet all log one line and return.
func SeedCorpus(ctx context.Context, cfg config.Config) {
	if len(cfg.CorpusAutoseedSources) == 0 {
		return
	}
	if u.DB == nil {
		corpusLog.Infof("Corpus autoseed skipped: database not configured")
		return
	}

	for _, source := range cfg.CorpusAutoseedSources {
		if ctx.Err() != nil {
			return
		}
		seedOne(ctx, source)
	}
}

func seedOne(ctx context.Context, source string) {
	load, _, detail := loaderFor(source)
	if load == nil {
		// Not configured (503) or not implemented (501) is a normal state for
		// a source, not a boot failure. Say which, once, and move on.
		corpusLog.Infof("Corpus autoseed skipped (source=%s): %s", source, detail)
		return
	}

	// "If not exists" is read off the state machine rather than off a row
	// count: a run that finished leaves state=idle with the cursor it ended
	// on, so that pair — and only that pair — means "already loaded". A fresh
	// row (idle, empty cursor), a failed run and an interrupted one all want a
	// run, and the loader resumes from whatever cursor is there.
	var existing types.CorpusSync
	err := u.DB.WithContext(ctx).Where("source = ?", source).First(&existing).Error
	if err == nil && existing.State == types.CorpusSyncIdle && existing.Cursor != "" {
		corpusLog.Infof("Corpus autoseed skipped (source=%s): already loaded (cursor=%q)", source, existing.Cursor)
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		corpusLog.Warnf("Corpus autoseed could not read the cursor (source=%s): %v", source, err)
		return
	}

	claimed, won, err := claimSync(ctx, source, false)
	if err != nil {
		corpusLog.Warnf("Corpus autoseed claim failed (source=%s): %v", source, err)
		return
	}
	if !won {
		// Another replica booted first. That is the mechanism working.
		corpusLog.Infof("Corpus autoseed skipped (source=%s): another worker holds the sync", source)
		return
	}

	corpusLog.Infof("Corpus autoseed started (source=%s cursor=%q)", source, claimed.Cursor)
	bg, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	runSync(bg, source, claimed.Cursor, load)
}

// ---------------------------------------------------------------------------
// Judilibre — jurisprudence de la Cour de cassation (§5.1, §5.3)
// ---------------------------------------------------------------------------

// decisionsPerBatch is the export page size, and it is part of the CURSOR's
// meaning: Judilibre's `batch` is an index into pages of this size, so
// changing it mid-corpus would make a stored cursor point somewhere else.
// Change it only together with a full reload.
const decisionsPerBatch = 50

// judilibreAttribution is what Licence Ouverte 2.0 requires to travel with the
// decisions all the way to the rendered note.
const judilibreAttribution = "Jurisprudence judiciaire — Cour de cassation, base Judilibre, " +
	"Licence Ouverte 2.0. Réutilisation autorisée, y compris commercialement, sous réserve " +
	"de mentionner la source et sa date de dernière mise à jour. La Cour de cassation ne " +
	"garantit ni l'exhaustivité ni l'actualité du fonds."

// judilibreLoader loads the employment-law case-law scope: published Cour de
// cassation, chambre sociale decisions from 2018 (connectors.EmploymentScope).
//
// It walks /export rather than paginating /search, because /search is capped
// and was never meant to enumerate a corpus. The resume key is the API's OWN
// `next_batch` — unlike the Légifrance loader, which resumes by article
// identity because its list can shift underneath a run, an export batch is a
// server-side position the API undertakes to honour, and it is the only cursor
// Judilibre offers.
//
// A decision has NO validity interval. Its authority is contextual — court,
// formation, publication level, and whether a later decision overruled it —
// so valid_from/valid_to stay null and the metadata carries the weight. §6.3
// says this plainly: nothing here establishes that a decision is still good
// law, and neither retrieval nor a reranker can.
func judilibreLoader(j *connectors.Judilibre) loader {
	return func(ctx context.Context, cursor string, stats *loadStats) (string, bool, error) {
		batch := 0
		if cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil {
				return cursor, false, fmt.Errorf("judilibre: unreadable cursor %q", cursor)
			}
			batch = n
		}

		scope := connectors.EmploymentScope()
		scope.PageSize = decisionsPerBatch
		page, err := j.Export(ctx, scope, batch)
		if err != nil {
			return cursor, false, err
		}

		stats.License = connectors.LicenceOuverte2
		stats.Attribution = judilibreAttribution
		stats.Provenance = fmt.Sprintf("Judilibre — Cour de cassation, chambre sociale, publiées, depuis 2018 (%d décisions)", page.Total)

		docs := make([]types.LegalDocument, 0, len(page.Results))
		versions := make([]types.LegalVersion, 0, len(page.Results))
		var pending []pendingChunk

		for _, d := range page.Results {
			text := strings.TrimSpace(d.Text)
			if text == "" {
				// A decision whose text the API withholds cannot be quoted, and
				// a source that cannot be quoted cannot be cited (§6.4).
				corpusLog.Debugf("Judilibre decision skipped, no text (id=%s ecli=%s)", d.ID, d.ECLI)
				continue
			}
			docID := derivedID(types.SourceJudilibre, d.ID)
			versionID := derivedID(docID, d.SourceRevision())
			sum := sha256.Sum256([]byte(text))
			label := decisionCitationLabel(d)

			docs = append(docs, types.LegalDocument{
				ID:            docID,
				Source:        types.SourceJudilibre,
				SourceID:      d.ID,
				Kind:          legalKindDecision,
				Jurisdiction:  decisionJurisdiction(d),
				CanonicalURL:  connectors.DecisionURL(d.ID),
				CitationLabel: label,
				LicenseCode:   connectors.LicenceOuverte2,
			})
			versions = append(versions, types.LegalVersion{
				ID:              versionID,
				LegalDocumentID: docID,
				SourceVersion:   d.SourceRevision(),
				Text:            text,
				TextSHA256:      hex.EncodeToString(sum[:]),
				// No interval, on purpose — see the function comment.
				DecisionDate:     d.DecidedAt(),
				PublicationDate:  d.PublishedAt(),
				Formation:        d.Formation,
				PublicationLevel: d.PublicationLevel(),
				Solution:         firstNonEmpty(d.Solution, d.SolutionAlt),
				ECLI:             d.ECLI,
				State:            types.LegalVersionInForce,
				SourceMeta:       judilibreMeta(d),
			})
			// The retrieval header carries the themes: a lawyer searches
			// "clause de non-concurrence", and the sommaire's vocabulary is
			// what makes a decision findable by notion rather than by wording.
			for i, chunk := range chunkText(retrievalTextFor(label, decisionRetrievalPath(d), text)) {
				pending = append(pending, pendingChunk{versionID: versionID, ordinal: i, text: chunk})
			}
		}

		embedded := embedChunks(ctx, pending)

		err = u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return persistBatch(tx, docs, versions, pending)
		})
		if err != nil {
			return cursor, false, err
		}

		stats.Decisions += len(page.Results)
		stats.Documents += len(docs)
		stats.Versions += len(versions)
		stats.Chunks += len(pending)
		stats.Embedded += embedded
		corpusLog.Infof("Judilibre batch loaded (batch=%d decisions=%d kept=%d chunks=%d embedded=%d total=%d)",
			page.Batch, len(page.Results), len(docs), len(pending), embedded, page.Total)

		// The cursor advances only after the batch has committed, so a pod
		// death costs one batch and never skips one. A nil next_batch is the
		// API saying the scope is exhausted.
		next, more := page.NextBatchNumber()
		if !more || len(page.Results) == 0 {
			return cursor, true, nil
		}
		return strconv.Itoa(next), false, nil
	}
}

// decisionCitationLabel is how a lawyer writes the decision:
// "Cass. soc., 12 janvier 2022, n° 21-11.111".
func decisionCitationLabel(d connectors.Decision) string {
	parts := []string{decisionCourtAbbrev(d)}
	if at := d.DecidedAt(); at != nil {
		parts = append(parts, frenchLongDate(*at))
	}
	if p := d.Pourvoi(); p != "" {
		parts = append(parts, "n° "+p)
	} else if d.ECLI != "" {
		parts = append(parts, d.ECLI)
	}
	return strings.Join(parts, ", ")
}

// decisionCourtAbbrev renders the court and chamber the way they are cited.
// An unknown code falls back to the raw value rather than being dropped: a
// citation naming a chamber we do not recognise is still a correct citation.
func decisionCourtAbbrev(d connectors.Decision) string {
	// The export speaks French labels ("Chambre sociale") while the search
	// filters speak codes ("soc"), so both are mapped — a citation reading
	// "Cass. Chambre sociale" is not how anyone writes it.
	chambers := map[string]string{
		"soc": "soc.", "chambre sociale": "soc.",
		"civ1": "1re civ.", "première chambre civile": "1re civ.", "premiere chambre civile": "1re civ.",
		"civ2": "2e civ.", "deuxième chambre civile": "2e civ.", "deuxieme chambre civile": "2e civ.",
		"civ3": "3e civ.", "troisième chambre civile": "3e civ.", "troisieme chambre civile": "3e civ.",
		"comm": "com.", "chambre commerciale financière et économique": "com.",
		"chambre commerciale": "com.",
		"crim":                "crim.", "chambre criminelle": "crim.",
		"mi": "ch. mixte", "chambre mixte": "ch. mixte",
		"ap": "ass. plén.", "assemblée plénière": "ass. plén.", "assemblee pleniere": "ass. plén.",
	}
	court := "Cass."
	if j := strings.ToLower(strings.TrimSpace(d.Jurisdiction)); j == "ca" || strings.HasPrefix(j, "cour d'appel") {
		court = "CA"
	}
	chamber := strings.ToLower(strings.TrimSpace(d.Chamber))
	if abbrev, ok := chambers[chamber]; ok {
		return court + " " + abbrev
	}
	if chamber == "" {
		return court
	}
	return court + " " + chamber
}

// decisionJurisdiction is the readable court name stored on the document, and
// what the drafting locator shows next to a quote.
func decisionJurisdiction(d connectors.Decision) string {
	switch j := strings.ToLower(strings.TrimSpace(d.Jurisdiction)); j {
	case "cc":
		return "Cour de cassation"
	case "ca":
		return "Cour d'appel"
	case "tj":
		return "Tribunal judiciaire"
	case "":
		return "fr"
	default:
		// Already a label ("Cour de cassation") — keep it as the source wrote it.
		return d.Jurisdiction
	}
}

// decisionRetrievalPath is the header path of a decision's retrieval text: its
// formation and its themes, which is the vocabulary a question is asked in.
func decisionRetrievalPath(d connectors.Decision) []string {
	path := make([]string, 0, len(d.Themes)+1)
	if d.Formation != "" {
		path = append(path, d.Formation)
	}
	path = append(path, d.Themes...)
	return path
}

// judilibreMeta records what §5.1 requires to trace a chunk back to what was
// published, plus the analysis metadata the source provides — visa and
// rapprochements are how a lawyer checks whether a decision was later
// distinguished, and they are not derivable from the text.
func judilibreMeta(d connectors.Decision) types.JSONB {
	// The source wraps article references in <a href> anchors. They are
	// stripped here rather than at read time: this metadata is shown to a
	// lawyer and quoted into a note, and markup that reached either would be
	// noise at best.
	visa := make([]string, 0, len(d.Visa))
	for _, v := range d.Visa {
		if title := stripHTML(v.Title); title != "" {
			visa = append(visa, title)
		}
	}
	rapprochements := make([]map[string]string, 0, len(d.Rapprochements))
	for _, r := range d.Rapprochements {
		rapprochements = append(rapprochements, map[string]string{"id": r.ID, "titre": r.Title})
	}
	meta := map[string]any{
		"jurisdiction":      d.Jurisdiction,
		"chamber":           d.Chamber,
		"formation":         d.Formation,
		"publication":       d.Publication,
		"publication_level": d.PublicationLevel(),
		"solution":          firstNonEmpty(d.Solution, d.SolutionAlt),
		"type":              d.Type,
		"numbers":           d.Numbers,
		"themes":            d.Themes,
		"visa":              visa,
		"rapprochements":    rapprochements,
		"sommaire":          d.Summary,
		"update_date":       d.UpdateDate,
		"license":           connectors.LicenceOuverte2,
		"attribution":       judilibreAttribution,
		"source_url":        connectors.DecisionURL(d.ID),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return types.JSONB(raw)
}

// stripHTML removes tags and collapses whitespace. It is not a sanitiser and
// does not need to be: the input is the source's own visa markup, and the
// output only ever reaches a jsonb column and a prompt.
func stripHTML(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// frenchLongDate renders a unix date the way a citation carries it.
func frenchLongDate(sec int64) string {
	months := []string{"janvier", "février", "mars", "avril", "mai", "juin",
		"juillet", "août", "septembre", "octobre", "novembre", "décembre"}
	t := time.Unix(sec, 0).UTC()
	day := strconv.Itoa(t.Day())
	if t.Day() == 1 {
		day = "1er"
	}
	return fmt.Sprintf("%s %s %d", day, months[int(t.Month())-1], t.Year())
}
