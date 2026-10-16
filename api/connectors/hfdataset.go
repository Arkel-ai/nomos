package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var hfLog = u.NewLogger("connectors.hfdataset")

// The AgentPublic/legi bootstrap shortcut (docs/PLAN.md §5.1).
//
// AgentPublic/legi is the LEGI consolidated-legislation corpus ALREADY CHUNKED
// AND EMBEDDED with BAAI/bge-m3 at 1024 dimensions, under Etalab Open Licence
// 2.0. That is the same model and the same dimension Nomos queries with, so
// the vectors load straight into the halfvec(1024) column with ZERO embedding
// cost — which is the entire reason this connector exists.
//
// IT IS BOOTSTRAP MATERIAL, NOT AN AUTHORITATIVE LEGAL SNAPSHOT. It is a
// third-party redistribution of LEGI frozen at a dataset revision; it mixes
// article statuses (VIGUEUR alongside ABROGE and MODIFIE); and it carries no
// guarantee of freshness. The opposable text is the authenticated Légifrance
// publication, so anything loaded from here is a cache that export-time
// revalidation must check (§5.1, §6.4). Which is why every row records BOTH
// the pinned dataset revision AND a content hash: without them a chunk cannot
// be traced back to what was actually ingested.
const (
	// HFDatasetID is the dataset repo.
	HFDatasetID = "AgentPublic/legi"

	// HFPinnedRevision is the dataset commit this build ingests. PINNED ON
	// PURPOSE: `main` is a moving target, and a corpus whose contents change
	// under a citation is not citable. Bumping it is a deliberate act that
	// re-runs the sync and writes new source_version rows.
	HFPinnedRevision = "67be48b3d4a8df343d7dc6597b88bb896d02236e"

	// HFConfig / HFSplit are the dataset's single configuration and split.
	HFConfig = "latest"
	HFSplit  = "train"

	// HFEmbeddingModel / HFEmbeddingDims describe the DATASET, not this app.
	//
	// They no longer match: the corpus embeds with openai/text-embedding-3-small
	// at 1536 dimensions, and these vectors are bge-m3 at 1024. Two different
	// models are two different vector spaces, so the dataset's pre-computed
	// vectors can no longer be loaded as-is — the "zero embedding cost"
	// bootstrap of §5.1 is void until either side moves. Vector() still
	// asserts the dataset's own width, so a loader that tries gets a clear
	// error instead of silently poisoning the corpus (§9.1(2)).
	HFEmbeddingModel = "baai/bge-m3"
	HFEmbeddingDims  = 1024

	// HFLicense is Etalab Open Licence 2.0 — the same reuse terms, and the
	// same attribution obligation, as the Légifrance original.
	HFLicense = "etalab-2.0"

	defaultHFAPIURL  = "https://huggingface.co/api"
	defaultHFCDNURL  = "https://huggingface.co"
	defaultHFRowsURL = "https://datasets-server.huggingface.co"
)

// ErrHFRowsUnavailable is returned when the public rows API declines to serve
// the dataset. It is not a bug in this connector: the free datasets viewer
// refuses datasets above its resource budget, and AgentPublic/legi (1M–10M
// rows) is one of them. Callers fall back to the committed fixture corpus.
var ErrHFRowsUnavailable = errors.New("hugging face rows API unavailable for this dataset")

// EmploymentPartitions is the §5.3 scope filter, applied at LOAD time.
//
// The dataset is physically partitioned one directory per code, which is what
// makes scoping cheap: the loader fetches only these partitions and never
// touches the other ~118. Ingesting all of LEGI would be the 80–150 GB the
// plan rules out.
var EmploymentPartitions = []string{
	"legi_code_du_travail",
	"legi_code_civil",
	"legi_code_de_procedure_civile",
	"legi_code_de_la_securite_sociale",
}

// employmentTitles is the same scope expressed as full_title values, for
// filtering rows that arrive from the row API (which serves the concatenated
// split, not one partition).
var employmentTitles = map[string]bool{
	"Code du travail":             true,
	"Code civil":                  true,
	"Code de procédure civile":    true,
	"Code de la sécurité sociale": true,
}

// HFOptions configures the connector. Everything is injectable so the package
// is testable against an httptest.Server with no network.
type HFOptions struct {
	Dataset    string
	Revision   string
	APIURL     string
	CDNURL     string
	RowsURL    string
	HTTPClient *http.Client
}

// HFDataset reads the public Hugging Face endpoints. NO AUTHENTICATION: the
// dataset is public, and this connector deliberately sends no token — a
// bootstrap corpus that needs a credential is not a bootstrap.
type HFDataset struct {
	dataset  string
	revision string
	apiURL   string
	cdnURL   string
	rowsURL  string
	http     *http.Client
}

// NewHFDataset builds the connector, defaulting to the pinned revision.
func NewHFDataset(o HFOptions) *HFDataset {
	h := &HFDataset{
		dataset:  or(o.Dataset, HFDatasetID),
		revision: or(o.Revision, HFPinnedRevision),
		apiURL:   strings.TrimRight(or(o.APIURL, defaultHFAPIURL), "/"),
		cdnURL:   strings.TrimRight(or(o.CDNURL, defaultHFCDNURL), "/"),
		rowsURL:  strings.TrimRight(or(o.RowsURL, defaultHFRowsURL), "/"),
		http:     o.HTTPClient,
	}
	if h.http == nil {
		h.http = &http.Client{Timeout: defaultTimeout}
	}
	return h
}

// Revision is the pinned dataset commit recorded on every loaded row.
func (h *HFDataset) Revision() string { return h.revision }

// HeadRevision resolves the dataset's CURRENT head commit. It exists to detect
// drift — "the pin is 12 commits behind" is an admin decision, never an
// automatic upgrade, because silently following head would change the contents
// of already-issued citations.
func (h *HFDataset) HeadRevision(ctx context.Context) (string, error) {
	var meta struct {
		SHA          string `json:"sha"`
		LastModified string `json:"lastModified"`
		Private      bool   `json:"private"`
		CardData     struct {
			License any `json:"license"`
		} `json:"cardData"`
	}
	if err := h.getJSON(ctx, h.apiURL+"/datasets/"+h.dataset, &meta); err != nil {
		return "", err
	}
	hfLog.Infof("Hugging Face dataset %s head=%s pinned=%s last_modified=%s",
		h.dataset, short(meta.SHA), short(h.revision), meta.LastModified)
	return meta.SHA, nil
}

// Partition is one scoped parquet file: its path, its size, and the LFS
// sha256 the hub publishes for it. That oid is the CONTENT HASH recorded
// against every row loaded from the file — the pin says which commit, the hash
// says which bytes.
type Partition struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Code   string `json:"code"`
}

// URL is the direct download URL of the partition at the pinned revision.
func (h *HFDataset) URL(p Partition) string {
	return fmt.Sprintf("%s/datasets/%s/resolve/%s/%s", h.cdnURL, h.dataset, h.revision, p.Path)
}

// Partitions lists the parquet files for the employment-law scope only. This
// is where §5.3's "employment law only" rule is enforced against the wire: the
// other ~118 code partitions are never listed, never fetched, never counted.
func (h *HFDataset) Partitions(ctx context.Context) ([]Partition, error) {
	out := make([]Partition, 0, len(EmploymentPartitions))
	for _, code := range EmploymentPartitions {
		dir := "data/legi-latest/" + code
		var entries []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			LFS  struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs"`
		}
		endpoint := fmt.Sprintf("%s/datasets/%s/tree/%s/%s", h.apiURL, h.dataset, h.revision, dir)
		if err := h.getJSON(ctx, endpoint, &entries); err != nil {
			return nil, fmt.Errorf("list %s: %w", code, err)
		}
		for _, e := range entries {
			if e.Type != "file" || !strings.HasSuffix(e.Path, ".parquet") {
				continue
			}
			size := e.Size
			if e.LFS.Size > 0 {
				size = e.LFS.Size
			}
			out = append(out, Partition{Path: e.Path, Size: size, SHA256: e.LFS.OID, Code: code})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no parquet partitions found for the employment scope at revision %s", short(h.revision))
	}
	hfLog.Infof("Hugging Face scope resolved: %d parquet partitions across %d codes (revision=%s)",
		len(out), len(EmploymentPartitions), short(h.revision))
	return out, nil
}

// ParquetProbe is the result of verifying a partition without downloading it.
type ParquetProbe struct {
	Partition   Partition `json:"partition"`
	Bytes       int64     `json:"bytes"`
	IsParquet   bool      `json:"is_parquet"`
	FooterBytes uint32    `json:"footer_bytes"`
}

// ProbeParquet verifies a partition by reading only its last 8 bytes with an
// HTTP range request: a parquet file ends with a 4-byte footer length followed
// by the "PAR1" magic. It costs 8 bytes instead of 250 MB, and it is what the
// corpus status check uses to assert "the pinned revision still resolves and
// still serves parquet" before an admin starts a long sync.
func (h *HFDataset) ProbeParquet(ctx context.Context, p Partition) (ParquetProbe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL(p), nil)
	if err != nil {
		return ParquetProbe{}, err
	}
	req.Header.Set("Range", "bytes=-8")

	resp, err := h.http.Do(req)
	if err != nil {
		return ParquetProbe{}, fmt.Errorf("probe %s: %w", p.Code, err)
	}
	defer resp.Body.Close()
	tail, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ParquetProbe{}, err
	}
	if resp.StatusCode >= 400 {
		return ParquetProbe{}, &APIError{Source: "huggingface", Status: resp.StatusCode,
			Message: "range request rejected", Retryable: retryableStatus(resp.StatusCode)}
	}
	probe := ParquetProbe{Partition: p, Bytes: p.Size}
	if len(tail) == 8 && string(tail[4:]) == "PAR1" {
		probe.IsParquet = true
		probe.FooterBytes = uint32(tail[0]) | uint32(tail[1])<<8 | uint32(tail[2])<<16 | uint32(tail[3])<<24
	}
	hfLog.Infof("Parquet probe %s: parquet=%t size=%dB footer=%dB", p.Code, probe.IsParquet, probe.Bytes, probe.FooterBytes)
	return probe, nil
}

// LegiRow is one pre-embedded LEGI chunk. Embeddings arrive as a JSON-encoded
// string, not an array, which is why they are decoded explicitly.
type LegiRow struct {
	ChunkID    string `json:"chunk_id"`
	DocID      string `json:"doc_id"`
	ChunkIndex int    `json:"chunk_index"`
	// ChunkXXH64 is the dataset's own per-chunk content hash. Combined with
	// the pinned revision it is the full provenance of a loaded vector.
	ChunkXXH64 string `json:"chunk_xxh64"`
	Nature     string `json:"nature"`
	Category   string `json:"category"`
	Status     string `json:"status"`
	Title      string `json:"title"`
	FullTitle  string `json:"full_title"`
	Number     string `json:"number"`
	StartDate  string `json:"start_date"`
	EndDate    string `json:"end_date"`
	Text       string `json:"text"`
	ChunkText  string `json:"chunk_text"`
	Embeddings string `json:"embeddings_bge-m3"`
}

// InEmploymentScope applies §5.3 at row level, for rows that came from the
// concatenated split rather than a scoped partition.
func (r LegiRow) InEmploymentScope() bool { return employmentTitles[strings.TrimSpace(r.FullTitle)] }

// Vector decodes the pre-computed bge-m3 embedding and asserts its width. A
// row whose vector is not exactly HFEmbeddingDims wide is REJECTED rather than
// loaded: the dimension is baked into the halfvec column, so a mismatch is a
// silently poisoned corpus, not a recoverable warning (§9.1(2)).
func (r LegiRow) Vector() ([]float32, error) {
	s := strings.TrimSpace(r.Embeddings)
	if s == "" {
		return nil, errors.New("row carries no embedding")
	}
	var v []float32
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	if len(v) != HFEmbeddingDims {
		return nil, fmt.Errorf("embedding has %d dimensions, the corpus column is halfvec(%d)", len(v), HFEmbeddingDims)
	}
	return v, nil
}

// Validity converts the dataset's ISO start/end dates into the half-open unix
// interval legal_versions stores, normalising LEGI's 2999-01-01 sentinel to a
// nil end (§6.4).
func (r LegiRow) Validity() (validFrom, validTo *int64) {
	validFrom = parseISODate(r.StartDate)
	validTo = parseISODate(r.EndDate)
	if validTo != nil && *validTo >= legiOpenEndedMillis/1000 {
		validTo = nil
	}
	return validFrom, validTo
}

// ContentHash is the provenance stamp written on every loaded row: the pinned
// revision plus the dataset's chunk hash, plus a sha256 of the text as a
// fallback when the dataset omits its own hash.
func (r LegiRow) ContentHash(revision string) string {
	if r.ChunkXXH64 != "" {
		return revision + ":" + r.ChunkXXH64
	}
	sum := sha256.Sum256([]byte(r.ChunkText))
	return revision + ":" + hex.EncodeToString(sum[:])
}

// Rows reads a page from the public rows API, which is served from the hub's
// auto-converted parquet branch — the same bytes as the parquet files, decoded
// upstream, which is what lets this connector stay on the standard library
// instead of carrying a parquet reader.
//
// It returns ErrHFRowsUnavailable when the free viewer declines the dataset
// (HTTP 501 with a "resource usage" message, which is what AgentPublic/legi
// currently returns because of its size). That is a supported outcome, not a
// failure: the committed fixture corpus covers the offline path.
func (h *HFDataset) Rows(ctx context.Context, offset, length int) ([]LegiRow, error) {
	if length <= 0 {
		length = 100
	}
	v := url.Values{
		"dataset": {h.dataset},
		"config":  {HFConfig},
		"split":   {HFSplit},
		"offset":  {strconv.Itoa(offset)},
		"length":  {strconv.Itoa(length)},
	}
	var body struct {
		Rows []struct {
			RowIdx int     `json:"row_idx"`
			Row    LegiRow `json:"row"`
		} `json:"rows"`
		Error string `json:"error"`
	}
	err := h.getJSON(ctx, h.rowsURL+"/rows?"+v.Encode(), &body)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotImplemented || ae.Status == http.StatusNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrHFRowsUnavailable, ae.Message)
		}
		return nil, err
	}
	if body.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrHFRowsUnavailable, body.Error)
	}

	out := make([]LegiRow, 0, len(body.Rows))
	skipped := 0
	for _, r := range body.Rows {
		if !r.Row.InEmploymentScope() {
			skipped++
			continue
		}
		out = append(out, r.Row)
	}
	hfLog.Infof("Hugging Face rows %d..%d: %d in employment scope, %d out of scope",
		offset, offset+length, len(out), skipped)
	return out, nil
}

func (h *HFDataset) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	var lastErr error
	for attempt := 1; attempt <= defaultMaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt - 1)):
			}
		}
		resp, err := h.http.Do(req.Clone(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = &APIError{Source: "huggingface", Message: "connection error", Retryable: true}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		resp.Body.Close()
		if readErr != nil {
			lastErr = &APIError{Source: "huggingface", Status: resp.StatusCode, Message: "truncated response body", Retryable: true}
			continue
		}
		if resp.StatusCode >= 400 {
			ae := &APIError{Source: "huggingface", Status: resp.StatusCode,
				Message: upstreamMessage(raw), Retryable: retryableStatus(resp.StatusCode)}
			if !ae.Retryable {
				return ae
			}
			lastErr = ae
			continue
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s: %w", endpoint, err)
		}
		return nil
	}
	return lastErr
}

func or(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
