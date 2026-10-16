package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Rendering and publication of verified agent documents: the DOCX is a valid
// package that carries the revision word for word and every « À vérifier »
// warning; publication is keyed by the revision hash, so a retry reuses the
// rows; and an agent artifact downloads only while a final, hash-matching
// verification backs it.

// publishedChronology builds a run with one chronology carrying one pièce
// mismatch, verified through a review cycle and the final gate, plus the
// answer that accompanies it.
func publishedChronology(t *testing.T) (*agentFixture, VerifiedRevision, VerifiedRevision) {
	t.Helper()
	f := newAgentFixture(t, "publish")
	contrat := f.addPiece(1, "Contrat de travail")
	lettre := f.addPiece(8, "Lettre de licenciement")
	sc := f.pieceSpan(contrat, 2, "Le présent contrat prend effet le 2 septembre 2019 ; le salaire mensuel brut est fixé à 3 100,00 €.")
	sl := f.pieceSpan(lettre, 1, "Par lettre recommandée du 5 mars 2025, nous vous notifions votre licenciement.")

	embauche := "Embauche en CDI (salaire de 3 100,00 € brut)"
	envoi := "Envoi de la lettre de licenciement"
	chrono := f.addRevision(types.AgentOutputChronology, "Chronologie — dossier de test",
		types.DocumentBlock{ID: "h1", Role: types.BlockHeading, Level: 1, Text: "Relation de travail"},
		row("r1", embauche, day(2019, 9, 2), types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: 0, End: len([]rune(embauche)),
			EvidenceIDs: []string{sc.ID}, Dates: []types.ClaimDate{{PartialDate: day(2019, 9, 2), Role: types.DateRoleEvent}},
			Amounts: []types.Money{{MinorUnits: 310000, Currency: "EUR", Qualifier: types.MoneyGross}}}),
		row("r2", envoi, day(2025, 3, 3), types.DocumentClaim{ID: "c2", Kind: types.ClaimFactual, Start: 0, End: len([]rune(envoi)),
			EvidenceIDs: []string{sl.ID}, Dates: []types.ClaimDate{{PartialDate: day(2025, 3, 3), Role: types.DateRoleEvent}}}),
		para("p1", "Les dates sont reprises des pièces citées ; l'apostrophe, les « guillemets » et le tiret — restent intacts."),
	)
	f.reviewed(chrono.ID)
	chronoReport := f.verify(chrono.ID, true)
	if chronoReport.Result != types.VerificationWarnings || chronoReport.Warnings != 1 {
		t.Fatalf("chronology final report: %+v", chronoReport.Issues)
	}
	chronoVer := f.store(chronoReport)

	reply := "La chronologie est prête ; une date reste à vérifier."
	answer := f.addRevision(types.AgentOutputAnswer, "", para("a1", reply,
		types.DocumentClaim{ID: "c9", Kind: types.ClaimFactual, Start: 0, End: 15, EvidenceIDs: []string{sl.ID},
			Dates: []types.ClaimDate{{PartialDate: day(2025, 3, 3), Role: types.DateRoleEvent}}}))
	answerVer := f.store(f.verify(answer.ID, true))
	return f, VerifiedRevision{Revision: chrono, Verification: chronoVer}, VerifiedRevision{Revision: answer, Verification: answerVer}
}

func TestRenderVerifiedDocumentCarriesWarnings(t *testing.T) {
	requirePandoc(t)
	requireLocalBlob(t)
	f, chrono, _ := publishedChronology(t)

	staged, err := AgentArtifacts.PrepareArtifact(context.Background(), chrono, ArtifactFormatDOCX)
	if err != nil {
		t.Fatalf("PrepareArtifact: %v", err)
	}
	if staged.PieceWarnings != 1 || staged.Partial || staged.DocumentRevisionID == nil || *staged.DocumentRevisionID != chrono.Revision.ID ||
		len(staged.PublicationKey) != 64 || !strings.HasPrefix(staged.StagingKey, tenantKeyPrefix(f.firm)) {
		t.Fatalf("staged artifact: %+v", staged)
	}
	if n := testCount(t, "artifacts", "tenant_id = ?", f.firm); n != 0 {
		t.Fatalf("staging must not publish: %d artifact rows", n)
	}

	rc, err := u.Blob.Get(context.Background(), staged.StagingKey)
	if err != nil {
		t.Fatalf("staged object: %v", err)
	}
	pkg, _ := io.ReadAll(rc)
	rc.Close()
	if err := ValidateOOXML(pkg); err != nil {
		t.Fatalf("invalid package: %v", err)
	}
	text, err := DocxText(pkg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		DraftNotice,
		"Chronologie — dossier de test",
		"Embauche en CDI (salaire de 3 100,00 € brut)",
		"2 septembre 2019",
		"Pièce n° 8 — Lettre de licenciement, p. 1",
		"Envoi de la lettre de licenciement — À vérifier — la date du 3 mars 2025 attribuée à la pièce n° 8 n'a pas été retrouvée dans le passage cité.",
		"l'apostrophe, les « guillemets » et le tiret — restent intacts.",
		"Sources citées",
		"[1] Pièce n° 1 — Contrat de travail, p. 2",
		"Annexe — contrôle automatique",
		"1 point(s) à vérifier",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("DOCX text lacks %q:\n%s", want, text)
		}
	}

	// A revision without a matching verification never renders.
	forged := chrono
	forged.Verification.ContentSHA256 = strings.Repeat("f", 64)
	if _, err := RenderVerifiedDocument(context.Background(), forged, nil); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("forged verification rendered: %v", err)
	}
	// The fidelity check refuses a package missing a warning.
	if err := CheckDocxFidelity(pkg, []string{"À vérifier — une phrase qui n'est pas dans le document"}); !errors.Is(err, ErrArtifactFidelity) {
		t.Fatalf("fidelity: %v", err)
	}
}

func TestPublishResultIsIdempotentAndDownloadNeedsVerification(t *testing.T) {
	requirePandoc(t)
	requireLocalBlob(t)
	f, chrono, answer := publishedChronology(t)
	ctx := context.Background()

	// render_document stages under the verify_output report, before the
	// final gate; publication binds the final report of the same content.
	var early types.AgentVerification
	f.must(f.repo.AgentVerifications().Where("document_revision_id = ? AND verifier_version = ?",
		chrono.Revision.ID, VerifierVersion).Take(&early).Error)
	staging, err := LoadVerifiedRevision(ctx, f.firm, f.run.ID, early.ID)
	if err != nil {
		t.Fatalf("LoadVerifiedRevision: %v", err)
	}
	staged, err := AgentArtifacts.PrepareArtifact(ctx, staging, ArtifactFormatDOCX)
	if err != nil {
		t.Fatalf("PrepareArtifact: %v", err)
	}
	if *staged.VerificationID != early.ID {
		t.Fatalf("staged under %s, want the verify_output report", *staged.VerificationID)
	}
	in := PublicationInput{
		TenantID: f.firm, RunID: f.run.ID, ActorID: f.user, AssistantMessageID: f.assist.ID,
		Answer: answer, Artifacts: []StagedArtifact{staged}, Outcome: types.AgentOutcomeCompleted, Fence: testFence(f),
	}
	first, err := AgentPublisher.PublishResult(ctx, in)
	if err != nil {
		t.Fatalf("PublishResult: %v", err)
	}
	if first.Reused || len(first.ArtifactIDs) != 1 || len(first.CitationIDs) != 3 {
		t.Fatalf("first publication: %+v", first)
	}

	// A retry — same revision, same render — converges on the same rows.
	again, err := AgentPublisher.PublishResult(ctx, in)
	if err != nil || !again.Reused || again.ArtifactIDs[0] != first.ArtifactIDs[0] {
		t.Fatalf("retry: %v %+v", err, again)
	}
	restaged, err := AgentArtifacts.PrepareArtifact(ctx, chrono, ArtifactFormatDOCX)
	if err != nil || restaged.PublicationKey != staged.PublicationKey || restaged.StagingKey != staged.StagingKey {
		t.Fatalf("re-render changed the publication identity: %v", err)
	}
	if n := testCount(t, "artifacts", "tenant_id = ?", f.firm); n != 1 {
		t.Fatalf("artifacts after retry: %d", n)
	}
	if n := testCount(t, "run_events", "tenant_id = ? AND type = ?", f.firm, types.EventArtifactReady); n != 1 {
		t.Fatalf("artifact.ready events: %d", n)
	}
	if n := testCount(t, "citations", "tenant_id = ?", f.firm); n != 3 {
		t.Fatalf("citations after retry: %d", n)
	}

	// The legacy export revalidation leaves agent citations to the agent gate:
	// it must not flag them "sans source identifiable" and downgrade an export.
	if rep, err := RevalidateCitations(ctx, f.repo, []string{f.run.ID}); err != nil || rep.Checked != 0 || len(rep.Drifted) != 0 {
		t.Fatalf("legacy revalidation touched agent citations: %v %+v", err, rep)
	}

	var art types.Artifact
	f.must(f.repo.Artifacts().Where("id = ?", first.ArtifactIDs[0]).Take(&art).Error)
	if art.PieceWarnings != 1 || art.Partial || art.VerificationID == nil || *art.VerificationID != chrono.Verification.ID ||
		art.DocumentRevisionID == nil || *art.DocumentRevisionID != chrono.Revision.ID {
		t.Fatalf("artifact row: %+v", art)
	}
	var msg types.Message
	f.must(f.repo.Messages().Where("id = ?", f.assist.ID).Take(&msg).Error)
	if msg.AnswerRevisionID == nil || *msg.AnswerRevisionID != answer.Revision.ID || msg.AgentOutcome != types.AgentOutcomeCompleted {
		t.Fatalf("answer not frozen: %+v", msg)
	}

	// The frozen answer carries its citation marker and its warning.
	text, err := PublishedAnswerText(ctx, f.repo, f.assist.ID)
	if err != nil {
		t.Fatalf("PublishedAnswerText: %v", err)
	}
	if !strings.Contains(text, "La chronologie est prête ; une date reste à vérifier. [1]") ||
		!strings.Contains(text, "> À vérifier — la date du 3 mars 2025 attribuée à la pièce n° 8") {
		t.Fatalf("answer text:\n%s", text)
	}

	// Pièce warnings allow download.
	status, body := DownloadArtifact(ctxFor(f.firm, f.user), art.ID)
	if status != http.StatusOK {
		t.Fatalf("download with pièce warnings: %d %+v", status, body)
	}
	// A verification that no longer matches the revision refuses it.
	f.must(f.repo.Artifacts().Where("id = ?", art.ID).Update("verification_id", answer.Verification.ID).Error)
	if status, _ := DownloadArtifact(ctxFor(f.firm, f.user), art.ID); status != http.StatusConflict {
		t.Fatalf("mismatched verification must refuse download, got %d", status)
	}
	f.must(f.repo.Artifacts().Where("id = ?", art.ID).Update("verification_id", nil).Error)
	if status, _ := DownloadArtifact(ctxFor(f.firm, f.user), art.ID); status != http.StatusConflict {
		t.Fatalf("missing verification must refuse download, got %d", status)
	}
	// Another firm cannot learn the artifact exists.
	stranger := newAgentFixture(t, "stranger")
	if status, _ := DownloadArtifact(ctxFor(stranger.firm, stranger.user), art.ID); status != http.StatusNotFound {
		t.Fatalf("cross-tenant download: %d", status)
	}
}

func TestPublishResultRefusesUnverifiedOrStaleWork(t *testing.T) {
	requirePandoc(t)
	requireLocalBlob(t)
	f, chrono, answer := publishedChronology(t)
	ctx := context.Background()
	staged, err := AgentArtifacts.PrepareArtifact(ctx, chrono, ArtifactFormatDOCX)
	if err != nil {
		t.Fatalf("PrepareArtifact: %v", err)
	}
	base := PublicationInput{
		TenantID: f.firm, RunID: f.run.ID, ActorID: f.user, AssistantMessageID: f.assist.ID,
		Answer: answer, Artifacts: []StagedArtifact{staged}, Outcome: types.AgentOutcomeCompleted, Fence: testFence(f),
	}
	refused := func(name string, in PublicationInput) {
		t.Helper()
		if _, err := AgentPublisher.PublishResult(ctx, in); !errors.Is(err, ErrPublicationRefused) {
			t.Fatalf("%s: expected a refusal, got %v", name, err)
		}
		if n := testCount(t, "artifacts", "tenant_id = ?", f.firm); n != 0 {
			t.Fatalf("%s: %d artifact rows published", name, n)
		}
	}

	wrongActor := base
	wrongActor.ActorID = "someone-else"
	refused("actor", wrongActor)

	tampered := base
	st := staged
	st.SHA256 = strings.Repeat("0", 64)
	tampered.Artifacts = []StagedArtifact{st}
	refused("staged bytes", tampered)

	// A blocked answer has no final, download-allowing verification.
	blocked := f.addRevision(types.AgentOutputAnswer, "", para("a1", "Selon l'article L. 1237-19, la rupture est nulle."))
	blockedVer := f.store(f.verify(blocked.ID, true))
	if blockedVer.Result != types.VerificationBlocked {
		t.Fatalf("expected a blocked answer: %+v", blockedVer)
	}
	unverified := base
	unverified.Answer = VerifiedRevision{Revision: blocked, Verification: blockedVer}
	refused("blocked answer", unverified)
	if _, err := AgentArtifacts.PrepareArtifact(ctx, VerifiedRevision{Revision: blocked, Verification: blockedVer}, ArtifactFormatDOCX); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("a blocked revision must not render: %v", err)
	}

	// A piece added after verification changes the scope: re-verify first.
	f.addPiece(9, "Attestation tardive")
	refused("stale scope", base)
}

func testCount(t *testing.T, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := u.DB.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
