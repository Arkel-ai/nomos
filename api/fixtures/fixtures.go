// Package fixtures is the committed offline legal corpus.
//
// It exists so that Nomos — and, more importantly, the relevance evaluation
// harness — runs with NO network access and NO PISTE credentials. The PISTE
// connectors degrade to 503 when unconfigured (docs/PLAN.md §5.1), which would
// otherwise leave a fresh checkout with an empty `legal_chunks` table and no
// way to score retrieval at all.
//
// WHAT THIS IS: a curated subset of the French Code du travail, in force,
// covering the individual employment dispute — the scope §5.3 fixes for the
// MVP. Employment law ONLY: ingesting all of LEGI or all of Judilibre is
// ~10^7 vectors and 80–150 GB, which does not fit and is not attempted here.
//
// WHAT THIS IS NOT: an authoritative legal snapshot. Consolidated LEGI text is
// a cache; DILA is explicit that the opposable text is the authenticated
// Légifrance publication (§5.1). Every citation drawn from it must still be
// revalidated against the official source before export (§6.4).
//
// LICENCE: Licence Ouverte / Open Licence 2.0 (Etalab). Commercial reuse is
// permitted WITH ATTRIBUTION, and the attribution travels in the corpus file's
// own header — see Corpus.License, which the loader copies onto every
// legal_documents row as license_code. Do not strip it.
package fixtures

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// codeDuTravailJSON is the corpus itself. It is committed, not fetched: an
// offline fixture that needs the network is not a fixture.
//
//go:embed code_du_travail.json
var codeDuTravailJSON []byte

// Corpus is one fixture file: a licence header, a provenance header, and the
// articles. The two headers are mandatory — a legal corpus whose origin and
// reuse terms are not recorded cannot be cited (§13).
type Corpus struct {
	SchemaVersion int       `json:"schema_version"`
	CorpusID      string    `json:"corpus_id"`
	Description   string    `json:"description"`
	License       License   `json:"license"`
	Source        Source    `json:"source"`
	Articles      []Article `json:"articles"`
}

// License is the reuse terms, carried into legal_documents.license_code and
// into any output that quotes the corpus.
type License struct {
	Code            string `json:"code"`
	Name            string `json:"name"`
	URL             string `json:"url"`
	Attribution     string `json:"attribution"`
	SourceURL       string `json:"source_url"`
	ReuseConditions string `json:"reuse_conditions"`
}

// Source is the provenance of the snapshot: which official corpus, which
// redistribution, which content hash, and how the subset was selected. This is
// the fixture's equivalent of the pinned dataset revision the Hugging Face
// bootstrap records on every row (§5.1).
type Source struct {
	Corpus         string   `json:"corpus"`
	TextID         string   `json:"text_id"`
	TextTitle      string   `json:"text_title"`
	Publisher      string   `json:"publisher"`
	Snapshot       string   `json:"snapshot"`
	SnapshotSHA256 string   `json:"snapshot_sha256"`
	ExtractedAt    string   `json:"extracted_at"`
	Selection      string   `json:"selection"`
	Families       []string `json:"families"`
}

// Article is one article in force. Field names mirror the legal_documents /
// legal_versions split: DocumentID is the stable identity (LEGI `cid`),
// VersionID the immutable version (LEGI `id`), and ValidFrom/ValidTo the
// half-open legal validity interval of §6.4 — never an ingestion timestamp.
// ValidTo is nil when the article is open-ended (LEGI's 2999-01-01 sentinel is
// normalised away).
type Article struct {
	DocumentID    string   `json:"document_id"`
	VersionID     string   `json:"version_id"`
	Number        string   `json:"number"`
	CitationLabel string   `json:"citation_label"`
	SectionPath   []string `json:"section_path"`
	State         string   `json:"state"`
	ValidFrom     *int64   `json:"valid_from"`
	ValidTo       *int64   `json:"valid_to"`
	CanonicalURL  string   `json:"canonical_url"`
	Nota          *string  `json:"nota"`
	Text          string   `json:"text"`
	TextSHA256    string   `json:"text_sha256"`
}

// CodeDuTravail decodes the embedded corpus. It parses on every call rather
// than caching: it is called once per sync run, and a shared mutable corpus
// would be a footgun for no measurable gain.
func CodeDuTravail() (Corpus, error) {
	var c Corpus
	if err := json.Unmarshal(codeDuTravailJSON, &c); err != nil {
		return Corpus{}, fmt.Errorf("decode code_du_travail fixture: %w", err)
	}
	if len(c.Articles) == 0 {
		return Corpus{}, fmt.Errorf("code_du_travail fixture is empty")
	}
	return c, nil
}
