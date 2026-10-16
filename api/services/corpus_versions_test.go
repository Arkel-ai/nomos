package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Legal-version immutability, on both writers of
// legal_versions: the corpus synchronisation (persistBatch) and the
// on-demand read_law / read_decision path (storeLegalVersion). An identical
// identity and hash converges without a write; a changed text becomes a
// separate revision that keeps the official identifier; the text — and the
// chunks — behind an existing citation never move.

func newVersionsFixture(t *testing.T) {
	t.Helper()
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres not reachable — legal version suite skipped")
	}
	// Any embedding request goes to a fake that refuses it: chunks are
	// stored without vectors, as during a gateway outage, and nothing is paid.
	newScriptedGateway(t, func(int, map[string]any) (int, string) { return 500, `{}` })
}

// cleanupLegalDocs removes the documents, versions and chunks a test wrote.
func cleanupLegalDocs(t *testing.T, docIDs ...string) {
	t.Cleanup(func() {
		u.DB.Exec(`DELETE FROM legal_chunks WHERE legal_version_id IN (SELECT id FROM legal_versions WHERE legal_document_id IN ?)`, docIDs)
		u.DB.Exec(`DELETE FROM legal_versions WHERE legal_document_id IN ?`, docIDs)
		u.DB.Exec(`DELETE FROM legal_documents WHERE id IN ?`, docIDs)
	})
}

// syncArticle is one article as the corpus loader batches it.
func syncArticle(cid, legiID, text string) ([]types.LegalDocument, []types.LegalVersion, []pendingChunk) {
	docID := derivedID(types.SourceLegifrance, cid)
	versionID := derivedID(docID, legiID)
	from := time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC).Unix()
	doc := types.LegalDocument{ID: docID, Source: types.SourceLegifrance, SourceID: cid, Kind: legalKindArticle,
		Jurisdiction: "fr", CitationLabel: "C. trav., art. L1232-1"}
	ver := types.LegalVersion{ID: versionID, LegalDocumentID: docID, SourceVersion: legiID, Text: text,
		TextSHA256: sha256Hex(text), ValidFrom: &from, State: types.LegalVersionInForce,
		SourceMeta: types.JSONB(`{"article_number":"L1232-1"}`)}
	var chunks []pendingChunk
	for i, c := range chunkText(retrievalTextFor(doc.CitationLabel, nil, text)) {
		chunks = append(chunks, pendingChunk{versionID: versionID, ordinal: i, text: c})
	}
	return []types.LegalDocument{doc}, []types.LegalVersion{ver}, chunks
}

func syncBatch(t *testing.T, docs []types.LegalDocument, versions []types.LegalVersion, chunks []pendingChunk) {
	t.Helper()
	if err := u.DB.Transaction(func(tx *gorm.DB) error { return persistBatch(tx, docs, versions, chunks) }); err != nil {
		t.Fatalf("persistBatch: %v", err)
	}
}

// rowVersion is Postgres' xmin: it changes when, and only when, the row is
// written.
func rowVersion(t *testing.T, table, id string) string {
	t.Helper()
	var xmin string
	if err := u.DB.Raw("SELECT xmin::text FROM "+table+" WHERE id = ?", id).Scan(&xmin).Error; err != nil || xmin == "" {
		t.Fatalf("xmin of %s %s: %v", table, id, err)
	}
	return xmin
}

func versionsOf(t *testing.T, docID string) []types.LegalVersion {
	t.Helper()
	var vs []types.LegalVersion
	if err := u.DB.Where("legal_document_id = ?", docID).Order("ingested_at, source_version").Find(&vs).Error; err != nil {
		t.Fatal(err)
	}
	return vs
}

func chunkTexts(t *testing.T, versionID string) []string {
	t.Helper()
	var texts []string
	if err := u.DB.Model(&types.LegalChunk{}).Where("legal_version_id = ?", versionID).Order("ordinal").
		Pluck("text", &texts).Error; err != nil {
		t.Fatal(err)
	}
	return texts
}

func TestCorpusSyncNeverRewritesAStoredVersion(t *testing.T) {
	newVersionsFixture(t)
	cid, legiID := testLegiID(), testLegiID()
	const original = "Tout licenciement pour motif personnel est justifié par une cause réelle et sérieuse."
	docs, versions, chunks := syncArticle(cid, legiID, original)
	cleanupLegalDocs(t, docs[0].ID)
	syncBatch(t, docs, versions, chunks)
	stored := versionsOf(t, docs[0].ID)
	if len(stored) != 1 || stored[0].ID != versions[0].ID || stored[0].Text != original {
		t.Fatalf("first sync: %+v", stored)
	}
	// A citation rests on that exact text.
	citedID, citedSHA := stored[0].ID, stored[0].TextSHA256
	citedChunks := chunkTexts(t, citedID)
	before := rowVersion(t, "legal_versions", citedID)

	// The same identity and hash again: converges, no write.
	docs, versions, chunks = syncArticle(cid, legiID, original)
	syncBatch(t, docs, versions, chunks)
	if after := rowVersion(t, "legal_versions", citedID); after != before {
		t.Fatal("an identical replay wrote the stored version")
	}
	if n := len(versionsOf(t, docs[0].ID)); n != 1 {
		t.Fatalf("an identical replay left %d versions", n)
	}

	// Légifrance now publishes another text under the same identifier.
	const changed = "Tout licenciement pour motif personnel est motivé et justifié par une cause réelle et sérieuse."
	docs, versions, chunks = syncArticle(cid, legiID, changed)
	syncBatch(t, docs, versions, chunks)
	all := versionsOf(t, docs[0].ID)
	if len(all) != 2 {
		t.Fatalf("a changed text must be a separate revision, got %d versions", len(all))
	}
	var cited, revision types.LegalVersion
	for _, v := range all {
		if v.ID == citedID {
			cited = v
		} else {
			revision = v
		}
	}
	if cited.Text != original || cited.TextSHA256 != citedSHA || rowVersion(t, "legal_versions", citedID) != before {
		t.Fatal("the text behind an existing citation was rewritten")
	}
	if got := chunkTexts(t, citedID); strings.Join(got, "|") != strings.Join(citedChunks, "|") {
		t.Fatal("the chunks behind an existing citation were rewritten")
	}
	if revision.Text != changed || !strings.HasPrefix(revision.SourceVersion, legiID+"@") ||
		revision.ID != derivedID(docs[0].ID, legiID+"@"+sha256Hex(changed)[:16]) {
		t.Fatalf("revision identity: %+v", revision)
	}
	var meta map[string]any
	if err := json.Unmarshal(revision.SourceMeta, &meta); err != nil ||
		meta["official_source_version"] != legiID || meta["revision_of"] != citedID || meta["article_number"] != "L1232-1" {
		t.Fatalf("the revision must keep the official identifier and what it revises: %s", revision.SourceMeta)
	}
	if officialArticleID(revision) != legiID {
		t.Fatalf("the revision's official identity is lost: %q", officialArticleID(revision))
	}
	if got := chunkTexts(t, revision.ID); len(got) == 0 || !strings.Contains(got[0], "motivé et justifié") {
		t.Fatalf("the revision is not indexed under its own id: %v", got)
	}

	// Replaying the changed text converges on that revision.
	docs, versions, chunks = syncArticle(cid, legiID, changed)
	syncBatch(t, docs, versions, chunks)
	if n := len(versionsOf(t, docs[0].ID)); n != 2 {
		t.Fatalf("replaying a changed text left %d versions, want 2", n)
	}
}

// read_law and read_decision store what they fetch through
// storeLegalVersion: the same rule, and the same identities as the sync, so
// the two paths converge on one row per text.
func TestOnDemandRetrievalKeepsVersionsImmutable(t *testing.T) {
	newVersionsFixture(t)
	ctx := context.Background()
	cid, legiID := testLegiID(), testLegiID()
	const original = "L'employeur qui envisage de licencier un salarié le convoque, avant toute décision, à un entretien préalable."
	article := connectors.Article{ID: legiID, CID: cid, Num: "L1232-2", Text: original, Etat: "VIGUEUR",
		DateDebut: time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), DateFin: 32472144000000}

	first, doc, err := storeLegifranceArticle(ctx, article)
	if err != nil {
		t.Fatalf("read_law store: %v", err)
	}
	cleanupLegalDocs(t, doc.ID)
	before := rowVersion(t, "legal_versions", first.ID)
	again, _, err := storeLegifranceArticle(ctx, article)
	if err != nil || again.ID != first.ID || rowVersion(t, "legal_versions", first.ID) != before {
		t.Fatalf("an identical read must converge without a write: %v %s", err, again.ID)
	}

	changed := article
	changed.Text = original + " La convocation précise l'objet de l'entretien."
	revision, _, err := storeLegifranceArticle(ctx, changed)
	if err != nil {
		t.Fatalf("read_law store of a changed text: %v", err)
	}
	if revision.ID == first.ID || revision.Text != changed.Text || officialArticleID(revision) != legiID {
		t.Fatalf("a changed text must be a separate revision keeping its official id: %+v", revision)
	}
	if v := storedVersion(t, first.ID); v.Text != original || rowVersion(t, "legal_versions", first.ID) != before {
		t.Fatal("an on-demand read rewrote the stored version")
	}

	// A later sync of the same changed text converges on that revision.
	docs, versions, chunks := syncArticle(cid, legiID, changed.Text)
	if docs[0].ID != doc.ID || versions[0].ID != first.ID {
		t.Fatal("the sync and read_law derive different identities")
	}
	syncBatch(t, docs, versions, chunks)
	if n := len(versionsOf(t, doc.ID)); n != 2 {
		t.Fatalf("sync after read_law left %d versions, want 2", n)
	}

	// read_decision: the same rule for a decision.
	decisionID := "test" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	decision := connectors.Decision{ID: decisionID, Jurisdiction: "cc", Chamber: "soc", DecisionDate: "2022-01-12",
		UpdateDate: "2022-02-01", Text: "Attendu que le licenciement doit reposer sur une cause réelle et sérieuse."}
	dv, ddoc, err := storeJudilibreDecision(ctx, decision)
	if err != nil {
		t.Fatalf("read_decision store: %v", err)
	}
	cleanupLegalDocs(t, ddoc.ID)
	dBefore := rowVersion(t, "legal_versions", dv.ID)
	if same, _, err := storeJudilibreDecision(ctx, decision); err != nil || same.ID != dv.ID || rowVersion(t, "legal_versions", dv.ID) != dBefore {
		t.Fatalf("an identical decision read must converge without a write: %v", err)
	}
	edited := decision
	edited.Text = decision.Text + " Rejette le pourvoi."
	if rev, _, err := storeJudilibreDecision(ctx, edited); err != nil || rev.ID == dv.ID || rev.Text != edited.Text {
		t.Fatalf("a changed decision text must be a separate revision: %v %+v", err, rev)
	}
	if v := storedVersion(t, dv.ID); v.Text != decision.Text {
		t.Fatal("an on-demand read rewrote the stored decision")
	}
}
