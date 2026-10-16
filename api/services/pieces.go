package services

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var piecesLog = u.NewLogger("services.pieces")

// downloadURLTTL is how long a presigned download link stays valid. The
// bucket is private, so downloads always go through a short-lived signed URL
// generated on demand — never a public object ACL.
const downloadURLTTL = 15 * time.Minute

// Pieces — ingestion and the editable pieces table (docs/PLAN.md §7.1).
//
// The rule that outranks everything else here: ORIGINALS ARE NEVER MODIFIED OR
// RENAMED. Renaming is metadata, applied at export only. "Zero original bytes
// modified" is a measurable slice-1 acceptance criterion (§14).

// ListPieces returns a dossier's pieces in ordinal order, paginated with a
// Count total, tenant-scoped.
func ListPieces(c *gin.Context, dossierID string, q types.ListQuery) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	// Resolving the dossier through the tenant predicate first is what makes a
	// cross-tenant dossier id return 404 instead of an empty (or worse,
	// populated) piece list.
	d, status, errBody := loadDossier(repo, dossierID)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionRead) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}
	limit, offset := q.Normalized()

	var total int64
	if err := repo.Pieces().Where("dossier_id = ?", d.ID).Count(&total).Error; err != nil {
		piecesLog.Errorf("Piece count failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	rows := []types.Piece{}
	if err := repo.Pieces().Where("dossier_id = ?", d.ID).
		Order("ordinal ASC, created_at ASC").Limit(limit).Offset(offset).
		Find(&rows).Error; err != nil {
		piecesLog.Errorf("Piece select failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	piecesLog.Debugf("Listed pieces (dossier=%s rows=%d total=%d)", d.ID, len(rows), total)
	return http.StatusOK, types.ListResponse{Items: rows, Total: total, Limit: limit, Offset: offset}
}

// UpdatePiece applies a lawyer correction. Every proposed field must be
// editable before export — that is the acceptance criterion, and low
// confidence is surfaced, never silently accepted. Renumbering (ordinal) and
// final filenames commit as ONE transaction so the export can never see a
// half-applied reordering (§4.3).
func UpdatePiece(c *gin.Context, pieceID string, body types.PieceUpdateRequest) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	p, status, errBody := loadPiece(repo, pieceID)
	if p == nil {
		return status, errBody
	}
	d, status, errBody := loadDossier(repo, p.DossierID)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionWrite) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}

	updates := map[string]any{}
	if body.ProposedName != nil {
		updates["proposed_name"] = strings.TrimSpace(*body.ProposedName)
	}
	if body.FinalName != nil {
		updates["final_name"] = strings.TrimSpace(*body.FinalName)
	}
	if body.Ordinal != nil {
		if *body.Ordinal < 0 {
			return http.StatusBadRequest, gin.H{"detail": "Ordinal must be zero or positive"}
		}
		updates["ordinal"] = *body.Ordinal
	}
	if body.DocumentDate != nil {
		updates["document_date"] = *body.DocumentDate
	}
	if body.DatePrecision != nil {
		if !isDatePrecision(*body.DatePrecision) {
			return http.StatusBadRequest, gin.H{"detail": "Unknown date precision"}
		}
		updates["date_precision"] = *body.DatePrecision
	}
	if body.PieceKind != nil {
		// The same CLOSED vocabulary the model picks from. A correction UI
		// offering a different list is a second, silent schema: a kind the
		// rubrique catalogue does not know would land the piece under "Autres
		// pièces" without anyone being told (§7.1).
		kind := strings.TrimSpace(*body.PieceKind)
		if !IsPieceKind(kind) {
			return http.StatusBadRequest, gin.H{"detail": "Unknown piece kind"}
		}
		updates["piece_kind"] = kind
	}
	if body.Title != nil {
		updates["title"] = strings.TrimSpace(*body.Title)
	}
	if body.ClassificationState != nil {
		if !types.IsPieceState(*body.ClassificationState) {
			return http.StatusBadRequest, gin.H{"detail": "Unknown classification state"}
		}
		updates["classification_state"] = *body.ClassificationState
	}
	if len(updates) == 0 {
		return http.StatusBadRequest, gin.H{"detail": "No correctable field supplied"}
	}

	var updated types.Piece
	err := repo.Tx(func(tx *u.TenantRepo) error {
		if err := tx.Pieces().Where("id = ?", p.ID).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Pieces().Where("id = ?", p.ID).First(&updated).Error
	})
	if err != nil {
		piecesLog.Errorf("Piece update failed (piece=%s): %v", p.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database update failed"}
	}

	user := middlewares.CurrentUser(c)
	audit(repo, user.ID, "piece.correct", "piece", p.ID, jsonbOf(map[string]any{
		"fields": sortedKeys(updates), "dossier_id": p.DossierID,
	}))
	// Field NAMES are a shape; their values are document content (§7.4).
	piecesLog.Infof("Piece corrected (piece=%s dossier=%s fields=%d)", p.ID, p.DossierID, len(updates))
	return http.StatusOK, updated
}

// DownloadPiece returns a short-lived presigned URL for the ORIGINAL bytes.
// The bucket is private, access is owner-or-Casbin on `dossier:<id>`, and the
// URL itself is never logged (§7.4).
func DownloadPiece(c *gin.Context, pieceID string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	// Resolution and authorization come BEFORE the storage check, so another
	// tenant's piece id is "not found" whether or not S3 happens to be
	// configured — a 503 where a stranger expects a 404 is an existence oracle.
	p, status, errBody := loadPiece(repo, pieceID)
	if p == nil {
		return status, errBody
	}
	d, status, errBody := loadDossier(repo, p.DossierID)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionRead) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}
	if u.S3 == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Object storage not configured"}
	}

	var asset types.Asset
	if err := repo.Assets().Where("id = ?", p.SourceAssetID).First(&asset).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return http.StatusNotFound, gin.H{"detail": "Piece not found"}
		}
		piecesLog.Errorf("Asset select failed (piece=%s): %v", p.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	if !strings.HasPrefix(asset.S3Key, u.S3Prefix) {
		// Never sign a key outside this app's prefix, whatever the row says.
		piecesLog.Errorf("Refusing to sign asset %s: key outside the app prefix", asset.ID)
		return http.StatusInternalServerError, gin.H{"detail": "Could not generate download link"}
	}

	// The download name is metadata; the stored object is the untouched
	// original and is never renamed (§7.1).
	name := firstNonEmpty(p.FinalName, p.ProposedName, p.OriginalName)
	reqParams := url.Values{}
	reqParams.Set("response-content-disposition", fmt.Sprintf("attachment; filename=%q", name))

	presigned, err := u.S3.PresignedGetObject(c.Request.Context(), u.S3Bucket, asset.S3Key, downloadURLTTL, reqParams)
	if err != nil {
		piecesLog.Errorf("Presign failed (piece=%s): %v", p.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Could not generate download link"}
	}
	// The URL is a bearer credential: log that one was issued, never the URL.
	piecesLog.Infof("Presigned a piece download (piece=%s dossier=%s ttl=%s)", p.ID, p.DossierID, downloadURLTTL)
	return http.StatusOK, types.DownloadResponse{
		URL:       presigned.String(),
		Name:      name,
		ExpiresIn: int(downloadURLTTL.Seconds()),
	}
}

// --- helpers ----------------------------------------------------------------

// loadPiece reads one piece through the tenant predicate. Another firm's piece
// id is "not found", never "forbidden".
func loadPiece(repo *u.TenantRepo, id string) (*types.Piece, int, any) {
	if id == "" {
		return nil, http.StatusBadRequest, gin.H{"detail": "Invalid piece id"}
	}
	var p types.Piece
	if err := repo.Pieces().Where("id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, http.StatusNotFound, gin.H{"detail": "Piece not found"}
		}
		piecesLog.Errorf("Piece select failed (id=%s tenant=%s): %v", id, repo.TenantID(), err)
		return nil, http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	return &p, 0, nil
}

func isDatePrecision(s string) bool {
	switch s {
	case types.DatePrecisionDay, types.DatePrecisionMonth,
		types.DatePrecisionYear, types.DatePrecisionUnknown:
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "piece"
}

// sortedKeys makes the audit metadata deterministic.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
