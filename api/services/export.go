package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var exportLog = u.NewLogger("services.export")

// Export — the ZIP of renamed originals plus the deterministic bordereau.docx
// (docs/PLAN.md §7.3). One renderer: pandoc. Do NOT add a pure-Go OOXML
// fallback in parallel — two renderers double the test matrix and will
// diverge.
//
// The invariants, in the order they bite:
//
//   - RENAMING HAPPENS HERE. Stored originals keep their bytes and their keys
//     forever (§7.1, and "zero original bytes modified" is a measurable
//     acceptance criterion). The export writes renamed COPIES into the zip,
//     derived from the piece metadata the lawyer approved.
//   - Every public citation is rechecked against the source immediately before
//     publication, and drift is FLAGGED — never silently corrected. The
//     dispute may concern an earlier period, so a changed article is evidence
//     to show the lawyer, not a wording to swap in (§6.4).
//   - Publication is idempotent and two-phase: write to a temp key, VALIDATE
//     the bytes that actually landed, publish at the final key, and only then
//     insert the artifact row. Killing the pod mid-export must never leave a
//     half-published artifact visible (§4.3) — the artifact row IS the
//     visibility gate.
//   - An export the lawyer has not approved is stamped
//     "projet — vérification de l'avocat requise" (§13), and who approved it
//     and which source hashes they saw is recorded immutably.

const (
	// maxExportBytes caps one produced archive.
	maxExportBytes = 512 << 20 // 512 MiB
	// bordereauEntry / acteDir are the fixed layout of the archive.
	bordereauEntry = "bordereau.docx"
	acteDir        = "actes/"
)

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// ExportDossier renders the bordereau and bundles the renamed originals.
func ExportDossier(c *gin.Context, dossierID string, body types.ExportRequest) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c)
	if user == nil {
		return http.StatusUnauthorized, gin.H{"detail": "Not authenticated"}
	}
	d, status, errBody := loadDossier(repo, dossierID)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionWrite) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}

	res, err := BuildExport(c.Request.Context(), repo, *d, ExportOptions{
		Approve:    body.Approve,
		ApprovedBy: user.ID,
		RunID:      latestRunID(repo, d.ID),
	})
	if err != nil {
		return exportStatus(err)
	}

	audit(repo, user.ID, "dossier.export", "dossier", d.ID, jsonbOf(map[string]any{
		"pieces":              len(res.Entries),
		"approved":            res.Approved,
		"citations_checked":   res.Revalidation.Checked,
		"citations_drifted":   len(res.Revalidation.Drifted),
		"revalidated_against": res.Revalidation.Against,
		"template_version":    res.TemplateVersion,
		"renderer_version":    res.RendererVersion,
		"reference_sha256":    res.ReferenceSHA256,
		"export_sha256":       res.ExportSHA256,
	}))

	return http.StatusOK, res
}

// DownloadArtifact returns a short-lived presigned URL for a published
// artifact. Access is tenant + owner-or-Casbin on `dossier:<id>`; the URL is
// never logged.
func DownloadArtifact(c *gin.Context, artifactID string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	if artifactID == "" {
		return http.StatusBadRequest, gin.H{"detail": "Invalid artifact id"}
	}

	var art types.Artifact
	if err := repo.Artifacts().Where("id = ?", artifactID).Take(&art).Error; err != nil {
		return http.StatusNotFound, gin.H{"detail": "Artifact not found"}
	}
	// Authorization happens through the dossier, before storage is consulted:
	// a 503 where a stranger expects a 404 is an existence oracle.
	if art.DossierID != "" {
		d, status, errBody := loadDossier(repo, art.DossierID)
		if d == nil {
			return status, errBody
		}
		if !canAccessDossier(c, repo, *d, types.ActionRead) {
			return http.StatusForbidden, gin.H{"detail": "Forbidden"}
		}
	}
	// An agent artifact is served only under a published, hash-matching
	// final verification. Legacy artifacts keep
	// their legacy policy and never acquire a fabricated verification.
	if art.DocumentRevisionID != nil || art.PublicationKey != nil {
		if status, body := agentArtifactDownloadPolicy(c, repo, art); status != 0 {
			return status, body
		}
	}

	var asset types.Asset
	if err := repo.Assets().Where("id = ?", art.AssetID).Take(&asset).Error; err != nil {
		exportLog.Errorf("Artifact asset missing (artifact=%s): %v", art.ID, err)
		return http.StatusNotFound, gin.H{"detail": "Artifact not found"}
	}

	link, err := signedDownload(c.Request.Context(), asset.S3Key, art.Name)
	if err != nil {
		exportLog.Errorf("Presign failed (artifact=%s): %v", art.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Could not generate download link"}
	}
	// The URL is a bearer credential: log that one was issued, never the URL.
	exportLog.Infof("Presigned an artifact download (artifact=%s kind=%s dossier=%s ttl=%s)",
		art.ID, art.Kind, art.DossierID, downloadURLTTL)
	return http.StatusOK, types.DownloadResponse{
		URL:       link,
		Name:      art.Name,
		ExpiresIn: int(downloadURLTTL.Seconds()),
	}
}

// signedDownload prefers a real S3 presign (it can carry the download
// filename) and falls back to the blob abstraction, which is what makes the
// whole export path exercisable on a laptop with no S3 credentials.
func signedDownload(ctx context.Context, key, name string) (string, error) {
	if !strings.HasPrefix(key, u.BlobPrefix()) {
		// Never sign a key outside this app's prefix, whatever the row says.
		return "", fmt.Errorf("refusing to sign a key outside the app prefix")
	}
	if u.S3 != nil {
		params := url.Values{}
		params.Set("response-content-disposition", fmt.Sprintf("attachment; filename=%q", name))
		signed, err := u.S3.PresignedGetObject(ctx, u.S3Bucket, key, downloadURLTTL, params)
		if err != nil {
			return "", err
		}
		return signed.String(), nil
	}
	return u.Blob.SignedURL(ctx, key, downloadURLTTL)
}

// ---------------------------------------------------------------------------
// The runner step
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Building the export
// ---------------------------------------------------------------------------

// ErrExportEmpty means there is nothing to communicate.
var ErrExportEmpty = errors.New("le dossier ne contient aucune pièce à communiquer")

// ErrExportNotApproved means the caller asked to approve an export whose
// pieces the lawyer has not approved. Approval is a statement about specific
// fields, so it cannot be given over rows that are still proposals.
var ErrExportNotApproved = errors.New("toutes les pièces doivent être approuvées avant de valider l'export")

// ExportOptions parameterises one export.
type ExportOptions struct {
	Approve    bool
	ApprovedBy string
	RunID      string
	// Now is injectable so a golden render is reproducible in a test.
	Now time.Time
}

// ExportEntry is one renamed original inside the archive.
type ExportEntry struct {
	PieceID  string `json:"piece_id"`
	Ordinal  int    `json:"ordinal"`
	Name     string `json:"name"`
	Original string `json:"original_name"`
}

// ExportResult is what the endpoint and the step both return.
type ExportResult struct {
	ArtifactID   string             `json:"artifact_id"`
	BordereauID  string             `json:"bordereau_artifact_id"`
	Name         string             `json:"name"`
	Approved     bool               `json:"approved"`
	DraftNotice  string             `json:"draft_notice,omitempty"`
	Entries      []ExportEntry      `json:"entries"`
	Revalidation RevalidationReport `json:"revalidation"`

	// Artifacts are the two rows this export just published — the bordereau
	// and the archive. They travel WHOLE because POST /dossiers/:id/export is
	// SYNCHRONOUS: there is no run to follow and no artifact.ready event to
	// wait for, so a caller given only two ids has no name, no kind and no
	// approval flag to render, and cannot offer the download it just paid
	// for. Same reasoning as the plan and question payloads.
	Artifacts []types.Artifact `json:"artifacts"`

	// Provenance — what it takes to reproduce this document later.
	TemplateVersion string `json:"template_version"`
	RendererVersion string `json:"renderer_version"`
	ReferenceSHA256 string `json:"reference_sha256"`
	ExportSHA256    string `json:"export_sha256"`
	BordereauSHA256 string `json:"bordereau_sha256"`
}

// BuildExport produces, validates and publishes the archive.
func BuildExport(ctx context.Context, repo *u.TenantRepo, d types.Dossier, opts ExportOptions) (ExportResult, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	var pieces []types.Piece
	if err := repo.Pieces().Where("dossier_id = ?", d.ID).
		Where("classification_state <> ?", types.PieceStateRejected).
		Order("ordinal ASC, created_at ASC").Find(&pieces).Error; err != nil {
		return ExportResult{}, err
	}
	if len(pieces) == 0 {
		return ExportResult{}, ErrExportEmpty
	}

	unapproved := 0
	for _, p := range pieces {
		if p.ClassificationState != types.PieceStateApproved {
			unapproved++
		}
	}
	if opts.Approve && unapproved > 0 {
		return ExportResult{}, fmt.Errorf("%w (%d pièce(s) non approuvée(s))", ErrExportNotApproved, unapproved)
	}

	// Revalidate BEFORE rendering: drift changes what the document says about
	// itself, and it can withdraw an approval.
	runIDs := dossierRunIDs(repo, d.ID)
	report, err := RevalidateCitations(ctx, repo, runIDs)
	if err != nil {
		exportLog.Errorf("Citation revalidation failed (dossier=%s): %v", d.ID, err)
		return ExportResult{}, err
	}

	approved := opts.Approve && unapproved == 0
	if approved && len(report.Drifted) > 0 {
		// The lawyer approved a set of source hashes. Some of them moved, so
		// the approval no longer covers what the document says — the export
		// is downgraded to a projet rather than published as validated.
		exportLog.Warnf("Export downgraded to projet: %d citation(s) drifted since approval (dossier=%s)",
			len(report.Drifted), d.ID)
		approved = false
	}

	notice := ""
	if !approved {
		notice = DraftNotice
	}

	entries := make([]ExportEntry, 0, len(pieces))
	rows := make([]BordereauPiece, 0, len(pieces))
	used := map[string]int{}
	for i, p := range pieces {
		ordinal := p.Ordinal
		if ordinal <= 0 {
			ordinal = i + 1
		}
		name := uniqueName(used, ExportFileName(p, ordinal))
		entries = append(entries, ExportEntry{
			PieceID: p.ID, Ordinal: ordinal, Name: name, Original: p.OriginalName,
		})
		rows = append(rows, BordereauPiece{
			Ordinal:  ordinal,
			Title:    firstNonEmpty(p.Title, p.ProposedName, p.OriginalName),
			Date:     FrenchPieceDate(p.DocumentDate, p.DatePrecision),
			Kind:     firstNonEmpty(p.PieceKind, "—"),
			FileName: name,
		})
	}

	bordereau, err := RenderBordereau(ctx, BordereauDoc{
		DossierTitle:    d.Title,
		ClientReference: d.ClientReference,
		MatterType:      d.MatterType,
		GeneratedOn:     FrenchDate(opts.Now),
		Pieces:          rows,
		Footnotes:       exportFootnotes(unapproved, report),
		DraftNotice:     notice,
	})
	if err != nil {
		return ExportResult{}, err
	}

	actes, err := loadActes(ctx, repo, d.ID)
	if err != nil {
		return ExportResult{}, err
	}

	archive, err := buildExportZip(ctx, repo, entries, bordereau.Bytes, actes)
	if err != nil {
		return ExportResult{}, err
	}

	// --- two-phase publication ---------------------------------------------
	//
	// A disconnect must not be able to leave the halves inconsistent, so the
	// object writes and the row writes that RECONCILE them run on a context
	// that survives the request.
	//
	// The publication identity derives from the tenant, dossier, run and the
	// exact bytes (§4.3): a replayed export — an attempt that published and
	// then lost its lease — overwrites the same objects and finds the rows it
	// already committed, instead of publishing a second archive.
	pubCtx := context.WithoutCancel(ctx)
	zipSum := sha256.Sum256(archive)
	zipSHA := hex.EncodeToString(zipSum[:])
	exportID := exportIdentity(repo.TenantID(), d.ID, opts.RunID, zipSHA, bordereau.SHA256, approved, opts.ApprovedBy)
	// The temporary object is per attempt, so two overlapping attempts never
	// delete each other's staging copy; only what is published is shared.
	tmpKey := u.DossierKey(repo.TenantID(), d.ID, "tmp-export-"+uuid.NewString()+".zip")
	finalKey := u.DossierKey(repo.TenantID(), d.ID, "export-"+exportID+".zip")
	docxKey := u.DossierKey(repo.TenantID(), d.ID, "bordereau-"+exportID+".docx")

	if err := u.Blob.Put(pubCtx, tmpKey, bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		return ExportResult{}, err
	}
	if err := validateStoredZip(pubCtx, tmpKey, expectedEntryNames(entries, actes)); err != nil {
		_ = u.Blob.Delete(pubCtx, tmpKey)
		return ExportResult{}, err
	}
	if err := u.Blob.Put(pubCtx, finalKey, bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		_ = u.Blob.Delete(pubCtx, tmpKey)
		return ExportResult{}, err
	}
	if err := u.Blob.Put(pubCtx, docxKey, bytes.NewReader(bordereau.Bytes), int64(len(bordereau.Bytes)),
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document"); err != nil {
		_ = u.Blob.Delete(pubCtx, tmpKey)
		return ExportResult{}, err
	}
	_ = u.Blob.Delete(pubCtx, tmpKey)

	zipName := ExportArchiveName(d, opts.Now)

	var zipArtifact, docxArtifact types.Artifact
	err = repo.Tx(func(tx *u.TenantRepo) error {
		zipAsset := types.Asset{
			ID: exportRowID(exportID, "zip-asset"), S3Key: finalKey, Kind: types.AssetKindExport,
			Mime: "application/zip", Size: int64(len(archive)), SHA256: zipSHA,
			CreatedBy: opts.ApprovedBy,
		}
		docxAsset := types.Asset{
			ID: exportRowID(exportID, "bordereau-asset"), S3Key: docxKey, Kind: types.AssetKindArtifact,
			Mime:   "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			Size:   int64(len(bordereau.Bytes)),
			SHA256: bordereau.SHA256, CreatedBy: opts.ApprovedBy,
		}
		var approvedAt *int64
		if approved {
			at := opts.Now.Unix()
			approvedAt = &at
		}
		docxArtifact = types.Artifact{
			ID: exportRowID(exportID, "bordereau"), RunID: opts.RunID, DossierID: d.ID,
			Kind: types.ArtifactKindBordereauDocx, AssetID: docxAsset.ID, Name: bordereauEntry,
			CharCount: bordereau.CharCount,
			// Provenance travels with the row so the document can be
			// reproduced: template revision, pandoc version, charte hash.
			TemplateVersion: bordereau.TemplateVersion,
			RendererVersion: bordereau.RendererVersion + " ref:" + bordereau.ReferenceSHA256[:12],
			Approved:        approved, ApprovedBy: approvedBy(approved, opts.ApprovedBy), ApprovedAt: approvedAt,
		}
		zipArtifact = types.Artifact{
			ID: exportRowID(exportID, "zip"), RunID: opts.RunID, DossierID: d.ID,
			Kind: types.ArtifactKindExportZip, AssetID: zipAsset.ID, Name: zipName,
			CharCount:       len(archive),
			TemplateVersion: bordereau.TemplateVersion,
			RendererVersion: bordereau.RendererVersion + " ref:" + bordereau.ReferenceSHA256[:12],
			Approved:        approved, ApprovedBy: approvedBy(approved, opts.ApprovedBy), ApprovedAt: approvedAt,
		}
		// The ids derive from the publication identity, so the primary keys
		// make a replay converge: nothing is inserted twice, and the rows
		// returned are the ones first published.
		once := clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}
		assets := []types.Asset{zipAsset, docxAsset}
		arts := []types.Artifact{docxArtifact, zipArtifact}
		for i := range assets {
			assets[i].TenantID = tx.TenantID()
		}
		for i := range arts {
			arts[i].TenantID = tx.TenantID()
		}
		raw := tx.Assets().Session(&gorm.Session{NewDB: true})
		if err := raw.Clauses(once).Create(&assets).Error; err != nil {
			return err
		}
		if err := raw.Clauses(once).Create(&arts).Error; err != nil {
			return err
		}
		if err := tx.Artifacts().Where("id = ?", docxArtifact.ID).Take(&docxArtifact).Error; err != nil {
			return err
		}
		return tx.Artifacts().Where("id = ?", zipArtifact.ID).Take(&zipArtifact).Error
	})
	if err != nil {
		exportLog.Errorf("Artifact publication failed after the objects were written (dossier=%s): %v", d.ID, err)
		return ExportResult{}, err
	}

	exportLog.Infof("Export published (dossier=%s pieces=%d actes=%d bytes=%d approved=%t drifted=%d)",
		d.ID, len(entries), len(actes), len(archive), approved, len(report.Drifted))

	return ExportResult{
		ArtifactID:      zipArtifact.ID,
		BordereauID:     docxArtifact.ID,
		Artifacts:       []types.Artifact{docxArtifact, zipArtifact},
		Name:            zipName,
		Approved:        approved,
		DraftNotice:     notice,
		Entries:         entries,
		Revalidation:    report,
		TemplateVersion: bordereau.TemplateVersion,
		RendererVersion: bordereau.RendererVersion,
		ReferenceSHA256: bordereau.ReferenceSHA256,
		ExportSHA256:    zipSHA,
		BordereauSHA256: bordereau.SHA256,
	}, nil
}

// exportIdentity is the stable identity of one export publication: the
// tenant, dossier and run, the exact archive and bordereau bytes, and the
// approval they carry. The object keys and the row ids derive from it.
func exportIdentity(tenantID, dossierID, runID, zipSHA, bordereauSHA string, approved bool, approvedBy string) string {
	h := sha256.New()
	for _, part := range []string{tenantID, dossierID, runID, zipSHA, bordereauSHA, fmt.Sprint(approved), approvedBy} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// exportRowID is the id of one row of an export publication.
func exportRowID(exportID, role string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(exportID+"/"+role)).String()
}

// ---------------------------------------------------------------------------
// The archive
// ---------------------------------------------------------------------------

// acteFile is one drafted act already rendered to DOCX.
type acteFile struct {
	Name  string
	Bytes []byte
}

// buildExportZip assembles the archive: the renamed originals in the approved
// order, then the bordereau, then the drafted acts.
//
// The originals are streamed from storage and copied verbatim. Nothing here
// touches the stored object: the rename is an entry name in a zip.
func buildExportZip(ctx context.Context, repo *u.TenantRepo, entries []ExportEntry, bordereau []byte, actes []acteFile) ([]byte, error) {
	keys, err := pieceObjectKeys(repo, entries)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		key, ok := keys[e.PieceID]
		if !ok {
			return nil, fmt.Errorf("pièce %s : original introuvable dans le stockage", e.PieceID)
		}
		src, err := u.Blob.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("pièce %s : lecture de l'original impossible: %w", e.PieceID, err)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: time.Time{}})
		if err != nil {
			src.Close()
			return nil, err
		}
		n, err := io.Copy(w, io.LimitReader(src, maxExportBytes))
		src.Close()
		if err != nil {
			return nil, err
		}
		if buf.Len() > maxExportBytes {
			return nil, fmt.Errorf("archive trop volumineuse (>%d octets)", maxExportBytes)
		}
		exportLog.Debugf("Export entry written (piece=%s bytes=%d)", e.PieceID, n)
	}

	if err := writeZipEntry(zw, bordereauEntry, bordereau); err != nil {
		return nil, err
	}
	for _, a := range actes {
		if err := writeZipEntry(zw, acteDir+a.Name, a.Bytes); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeZipEntry(zw *zip.Writer, name string, body []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Time{}})
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// expectedEntryNames is the manifest the stored object is validated against.
func expectedEntryNames(entries []ExportEntry, actes []acteFile) []string {
	names := make([]string, 0, len(entries)+len(actes)+1)
	for _, e := range entries {
		names = append(names, e.Name)
	}
	names = append(names, bordereauEntry)
	for _, a := range actes {
		names = append(names, acteDir+a.Name)
	}
	return names
}

// validateStoredZip re-reads what actually landed in storage and checks it is
// a readable archive carrying exactly the expected entries, in order.
//
// Validating the in-memory buffer would prove nothing about the object: this
// is the step that stops a truncated upload from being published.
func validateStoredZip(ctx context.Context, key string, expected []string) error {
	rc, err := u.Blob.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxExportBytes))
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("archive illisible après écriture: %w", err)
	}
	if len(zr.File) != len(expected) {
		return fmt.Errorf("archive incomplète: %d entrées écrites, %d attendues", len(zr.File), len(expected))
	}
	for i, f := range zr.File {
		if f.Name != expected[i] {
			return fmt.Errorf("archive désordonnée à l'entrée %d", i)
		}
	}
	// The bordereau inside the published archive must still be a valid Word
	// package: it is the document that gets filed.
	for _, f := range zr.File {
		if f.Name != bordereauEntry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		docx, err := io.ReadAll(io.LimitReader(rc, maxDocxBytes))
		rc.Close()
		if err != nil {
			return err
		}
		return ValidateOOXML(docx)
	}
	return fmt.Errorf("archive publiée sans bordereau")
}

// ---------------------------------------------------------------------------
// Naming — the French label format, applied HERE and nowhere else
// ---------------------------------------------------------------------------

var frenchMonths = [...]string{
	"janvier", "février", "mars", "avril", "mai", "juin",
	"juillet", "août", "septembre", "octobre", "novembre", "décembre",
}

// ExportFileName is the renamed copy's name inside the archive:
//
//	Pièce n°01 - 2024-09-03 - Lettre de licenciement.pdf
//
// The date segment follows the piece's DatePrecision — a month-precision date
// never renders a fabricated day, and an illegible date drops the segment
// entirely rather than inventing one (§7.1).
func ExportFileName(p types.Piece, ordinal int) string {
	title := firstNonEmpty(p.Title, p.FinalName, p.ProposedName, stripExtension(p.OriginalName), "pièce")
	parts := []string{fmt.Sprintf("Pièce n°%02d", ordinal)}
	if date := isoPieceDate(p.DocumentDate, p.DatePrecision); date != "" {
		parts = append(parts, date)
	}
	parts = append(parts, sanitizeFileSegment(title))
	return strings.Join(parts, " - ") + extensionOf(p.OriginalName)
}

// ExportArchiveName names the archive itself.
func ExportArchiveName(d types.Dossier, now time.Time) string {
	base := sanitizeFileSegment(firstNonEmpty(d.ClientReference, d.Title, "dossier"))
	return fmt.Sprintf("Bordereau - %s - %s.zip", base, now.Format("2006-01-02"))
}

// FrenchPieceDate renders a piece date for the bordereau column, honouring its
// precision.
func FrenchPieceDate(ts *int64, precision string) string {
	if ts == nil {
		return "sans date"
	}
	t := time.Unix(*ts, 0).UTC()
	switch precision {
	case types.DatePrecisionYear:
		return t.Format("2006")
	case types.DatePrecisionMonth:
		return fmt.Sprintf("%s %d", frenchMonths[int(t.Month())-1], t.Year())
	case types.DatePrecisionUnknown, "":
		return "sans date"
	default:
		return FrenchDate(t)
	}
}

// FrenchDate is "3 septembre 2024" — the form a bordereau uses. The first of
// the month is "1er".
func FrenchDate(t time.Time) string {
	day := fmt.Sprintf("%d", t.Day())
	if t.Day() == 1 {
		day = "1er"
	}
	return fmt.Sprintf("%s %s %d", day, frenchMonths[int(t.Month())-1], t.Year())
}

func isoPieceDate(ts *int64, precision string) string {
	if ts == nil {
		return ""
	}
	t := time.Unix(*ts, 0).UTC()
	switch precision {
	case types.DatePrecisionYear:
		return t.Format("2006")
	case types.DatePrecisionMonth:
		return t.Format("2006-01")
	case types.DatePrecisionUnknown, "":
		return ""
	default:
		return t.Format("2006-01-02")
	}
}

// sanitizeFileSegment keeps accents (this is a French bordereau) but removes
// everything a filesystem, a zip reader or a path parser could misread.
func sanitizeFileSegment(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " .-")
	if s == "" {
		s = "pièce"
	}
	// Bounded so the archive stays portable: 120 runes leaves room for the
	// ordinal, the date and the extension inside a 255-byte name.
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimSpace(string(r[:120]))
	}
	return s
}

func extensionOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	ext := name[i:]
	if len(ext) > 12 || strings.ContainsAny(ext, "/\\ ") {
		return ""
	}
	return strings.ToLower(ext)
}

func stripExtension(name string) string {
	if ext := extensionOf(name); ext != "" {
		return name[:len(name)-len(ext)]
	}
	return name
}

// uniqueName keeps two identically-titled pieces from colliding in the zip.
func uniqueName(used map[string]int, name string) string {
	key := strings.ToLower(name)
	used[key]++
	if used[key] == 1 {
		return name
	}
	ext := extensionOf(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), used[key], ext)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func exportFootnotes(unapproved int, report RevalidationReport) []string {
	var out []string
	if unapproved > 0 {
		out = append(out, fmt.Sprintf(
			"%d pièce(s) n'ont pas encore été approuvées par l'avocat et figurent au bordereau à titre de projet.",
			unapproved))
	}
	if len(report.Drifted) > 0 {
		out = append(out, fmt.Sprintf(
			"%d citation(s) ne correspondent plus à la source enregistrée. Le texte cité n'a PAS été remplacé "+
				"— la période concernée par le litige peut porter sur une rédaction antérieure. Vérification requise.",
			len(report.Drifted)))
	}
	if report.Checked > 0 {
		out = append(out, fmt.Sprintf(
			"%d citation(s) vérifiées automatiquement contre %s au moment de l'export.",
			report.Checked, revalidationLabel(report.Against)))
	}
	return out
}

func revalidationLabel(against string) string {
	if against == "official_source" {
		return "la source officielle"
	}
	return "la version enregistrée (aucune source officielle n'était joignable)"
}

// pieceObjectKeys maps piece ids onto their stored original's key, checked
// against the app prefix. Keys are re-read from the tenant-scoped assets
// table, so an export can never reach outside <app>/.
func pieceObjectKeys(repo *u.TenantRepo, entries []ExportEntry) (map[string]string, error) {
	if len(entries) == 0 {
		return map[string]string{}, nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.PieceID)
	}
	var pieces []types.Piece
	if err := repo.Pieces().Where("id IN ?", ids).Find(&pieces).Error; err != nil {
		return nil, err
	}
	assetToPiece := map[string]string{}
	assetIDs := make([]string, 0, len(pieces))
	for _, p := range pieces {
		assetToPiece[p.SourceAssetID] = p.ID
		assetIDs = append(assetIDs, p.SourceAssetID)
	}
	var assets []types.Asset
	if err := repo.Assets().Where("id IN ?", assetIDs).Find(&assets).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(assets))
	for _, a := range assets {
		if !strings.HasPrefix(a.S3Key, u.BlobPrefix()) {
			exportLog.Errorf("Refusing to read asset %s: key outside the app prefix", a.ID)
			continue
		}
		out[assetToPiece[a.ID]] = a.S3Key
	}
	return out, nil
}

// loadActes renders the drafted acts this dossier's runs produced, so they
// travel in the same archive as the bordereau.
func loadActes(ctx context.Context, repo *u.TenantRepo, dossierID string) ([]acteFile, error) {
	var arts []types.Artifact
	if err := repo.Artifacts().Where("dossier_id = ?", dossierID).
		Where("kind = ?", types.ArtifactKindNoteDocx).
		Order("created_at ASC").Find(&arts).Error; err != nil {
		return nil, err
	}
	if len(arts) == 0 {
		return nil, nil
	}
	assetIDs := make([]string, 0, len(arts))
	for _, a := range arts {
		assetIDs = append(assetIDs, a.AssetID)
	}
	var assets []types.Asset
	if err := repo.Assets().Where("id IN ?", assetIDs).Find(&assets).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]types.Asset, len(assets))
	for _, a := range assets {
		byID[a.ID] = a
	}

	used := map[string]int{}
	out := make([]acteFile, 0, len(arts))
	for _, art := range arts {
		asset, ok := byID[art.AssetID]
		if !ok || !strings.HasPrefix(asset.S3Key, u.BlobPrefix()) {
			exportLog.Warnf("Skipping acte %s: object missing or outside the app prefix", art.ID)
			continue
		}
		rc, err := u.Blob.Get(ctx, asset.S3Key)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, maxDocxBytes))
		rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, acteFile{Name: uniqueName(used, sanitizeFileSegment(art.Name)+".docx"), Bytes: body})
	}
	return out, nil
}

// dossierRunIDs lists the runs whose citations this export must revalidate.
func dossierRunIDs(repo *u.TenantRepo, dossierID string) []string {
	var ids []string
	if err := repo.Runs().Where("dossier_id = ?", dossierID).
		Pluck("id", &ids).Error; err != nil {
		exportLog.Warnf("Run lookup failed during export (dossier=%s): %v", dossierID, err)
		return nil
	}
	sort.Strings(ids)
	return ids
}

// latestRunID attaches an HTTP-triggered export to the dossier's most recent
// run, so GET /api/runs/:id/artifacts finds it. An export on a dossier that
// has never been run carries no run id.
func latestRunID(repo *u.TenantRepo, dossierID string) string {
	var run types.Run
	if err := repo.Runs().Where("dossier_id = ?", dossierID).
		Order("created_at DESC").Limit(1).Take(&run).Error; err != nil {
		return ""
	}
	return run.ID
}

func approvedBy(approved bool, userID string) string {
	if !approved {
		return ""
	}
	return userID
}

// exportStatus maps an export failure onto the HTTP status the controller
// returns verbatim.
func exportStatus(err error) (int, any) {
	switch {
	case errors.Is(err, ErrExportEmpty):
		return http.StatusConflict, gin.H{"detail": ErrExportEmpty.Error()}
	case errors.Is(err, ErrExportNotApproved):
		return http.StatusConflict, gin.H{"detail": err.Error()}
	case errors.Is(err, ErrRendererUnavailable):
		return http.StatusServiceUnavailable, gin.H{"detail": "Renderer (pandoc) not available"}
	case errors.Is(err, ErrInvalidPackage):
		exportLog.Errorf("Export refused: %v", err)
		return http.StatusBadGateway, gin.H{"detail": "Le document produit n'a pas passé la validation OOXML"}
	case errors.Is(err, u.ErrBlobNotFound):
		return http.StatusConflict, gin.H{"detail": "Un original est introuvable dans le stockage"}
	default:
		exportLog.Errorf("Export failed: %v", err)
		return http.StatusBadGateway, gin.H{"detail": "L'export a échoué"}
	}
}
