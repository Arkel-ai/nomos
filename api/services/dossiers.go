package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var dossiersLog = u.NewLogger("services.dossiers")

// Dossiers — the document collections a conversation binds to (docs/PLAN.md
// §10). Every read and write here goes through a tenant-scoped repository:
// `WHERE tenant_id = ?` is not RLS, and one missed clause is a cross-tenant
// breach (§6.1). No ad-hoc u.DB query against a private table, anywhere.

// CreateDossier creates a dossier owned by the caller's tenant, seeds the
// Casbin grant for `dossier:<id>` in the tenant domain, and writes an
// audit_events row.
func CreateDossier(c *gin.Context, body types.DossierCreateRequest) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c) // never nil: RequireAuth ran first

	title := strings.TrimSpace(body.Title)
	if title == "" {
		return http.StatusBadRequest, gin.H{"detail": "Title is required"}
	}

	d := types.Dossier{
		ID:              uuid.NewString(),
		Title:           title,
		ClientReference: strings.TrimSpace(body.ClientReference),
		MatterType:      strings.TrimSpace(body.MatterType),
		State:           types.DossierStateActive,
		TitlePending:    body.AutoTitle,
		CreatedBy:       user.ID,
	}
	if err := repo.Create(&d); err != nil {
		dossiersLog.Errorf("Dossier insert failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database insert failed"}
	}

	// Per-dossier sharing, in the FIRM's Casbin domain — never "*" (§6.1).
	// Best-effort: the row exists and the creator can already reach it, so a
	// policy write failure must not strand the dossier.
	for _, act := range []string{types.ActionRead, types.ActionWrite} {
		if err := u.GrantPermission(user.ID, repo.TenantID(), dossierObject(d.ID), act); err != nil &&
			!errors.Is(err, u.ErrAuthNotConfigured) {
			dossiersLog.Warnf("Dossier grant failed (dossier=%s act=%s): %v", d.ID, act, err)
		}
	}

	audit(repo, user.ID, "dossier.create", "dossier", d.ID, nil)
	dossiersLog.Infof("Dossier created (id=%s tenant=%s)", d.ID, repo.TenantID())
	return http.StatusCreated, d
}

// ListDossiers returns the caller's tenant's dossiers, newest-updated first,
// paginated with a Count total. Never unbounded.
func ListDossiers(c *gin.Context, q types.ListQuery) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	limit, offset := q.Normalized()

	var total int64
	if err := repo.Dossiers().Count(&total).Error; err != nil {
		dossiersLog.Errorf("Dossier count failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	rows := []types.Dossier{}
	if err := repo.Dossiers().
		Order("updated_at DESC").Limit(limit).Offset(offset).
		Find(&rows).Error; err != nil {
		dossiersLog.Errorf("Dossier select failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	dossiersLog.Debugf("Listed dossiers (tenant=%s rows=%d total=%d)", repo.TenantID(), len(rows), total)
	return http.StatusOK, types.ListResponse{Items: rows, Total: total, Limit: limit, Offset: offset}
}

// GetDossier returns one dossier plus its piece/run counters, 404 when it does
// not exist *or* belongs to another tenant — the two must be indistinguishable
// to the caller.
func GetDossier(c *gin.Context, id string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	d, status, errBody := loadDossier(repo, id)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionRead) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}

	detail := types.DossierDetail{Dossier: *d}
	if err := repo.Pieces().Where("dossier_id = ?", d.ID).Count(&detail.PieceCount).Error; err != nil {
		dossiersLog.Errorf("Piece count failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	if err := repo.Pieces().
		Where("dossier_id = ? AND classification_state = ?", d.ID, types.PieceStateApproved).
		Count(&detail.ApprovedCount).Error; err != nil {
		dossiersLog.Errorf("Approved piece count failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	if err := repo.Runs().Where("dossier_id = ?", d.ID).Count(&detail.RunCount).Error; err != nil {
		dossiersLog.Errorf("Run count failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	return http.StatusOK, detail
}

// DeleteDossier purges a dossier for real: the S3 originals and artifacts, the
// chunks, the pieces, the runs and the rows — then the dossier itself.
//
// It is NOT a GORM soft delete. A soft delete purges nothing in S3, vectors or
// backups (§9.1(8)) and would leave a GDPR erasure request unsatisfied while
// looking green in the UI.
//
// What this does NOT yet cover, stated rather than hidden (§13): backup
// expiry, cost payloads already delivered to the tracker, and any page image
// or export written outside the assets table. Those need the retention
// workflow, not this endpoint.
//
// The purge is deliberately explicit about partial failure. Objects go first;
// if any object cannot be removed, its asset row is KEPT and the dossier stays
// in `purge_pending`, so the remaining work is still discoverable and a retry
// is a re-DELETE. Only a clean sweep removes the dossier row.
func DeleteDossier(c *gin.Context, id string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	d, status, errBody := loadDossier(repo, id)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionWrite) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}
	user := middlewares.CurrentUser(c)

	// The purge reconciles side effects that are already committed (objects
	// deleted from S3), so a client disconnect must not abandon it halfway.
	ctx := context.WithoutCancel(c.Request.Context())
	purge := u.ForTenantID(ctx, repo.TenantID())

	if err := purge.Dossiers().Where("id = ?", d.ID).
		Update("state", types.DossierStatePurgePending).Error; err != nil {
		dossiersLog.Errorf("Marking dossier purge_pending failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database update failed"}
	}

	var pieceIDs, assetIDs, runIDs []string
	if err := purge.Pieces().Where("dossier_id = ?", d.ID).Pluck("id", &pieceIDs).Error; err == nil {
		err = purge.Pieces().Where("dossier_id = ?", d.ID).Pluck("source_asset_id", &assetIDs).Error
		if err != nil {
			dossiersLog.Errorf("Collecting piece assets failed (dossier=%s): %v", d.ID, err)
			return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
		}
	} else {
		dossiersLog.Errorf("Collecting pieces failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	if err := purge.Runs().Where("dossier_id = ?", d.ID).Pluck("id", &runIDs).Error; err != nil {
		dossiersLog.Errorf("Collecting runs failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	var artifactAssetIDs []string
	if err := purge.Artifacts().Where("dossier_id = ?", d.ID).
		Pluck("asset_id", &artifactAssetIDs).Error; err != nil {
		dossiersLog.Errorf("Collecting artifacts failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	assetIDs = append(assetIDs, artifactAssetIDs...)

	removed, kept := purgeObjects(ctx, purge, assetIDs)

	// Rows go in one transaction: a dossier that is half-deleted is worse
	// than one that is not deleted at all.
	err := purge.Tx(func(tx *u.TenantRepo) error {
		if err := tx.PieceChunks().Where("dossier_id = ?", d.ID).Delete(&types.PieceChunk{}).Error; err != nil {
			return err
		}
		if len(pieceIDs) > 0 {
			if err := tx.PieceParties().Where("piece_id IN ?", pieceIDs).Delete(&types.PiecePartie{}).Error; err != nil {
				return err
			}
			if err := tx.Citations().Where("piece_id IN ?", pieceIDs).Delete(&types.Citation{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Pieces().Where("dossier_id = ?", d.ID).Delete(&types.Piece{}).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := tx.RunStepDeps(runIDs).Delete(&types.RunStepDep{}).Error; err != nil {
				return err
			}
			for _, del := range []struct {
				q     *gorm.DB
				model any
			}{
				{tx.StepAttempts().Where("run_step_id IN (?)", tx.RunSteps().Select("id").Where("run_id IN ?", runIDs)), &types.StepAttempt{}},
				{tx.RunSteps().Where("run_id IN ?", runIDs), &types.RunStep{}},
				{tx.RunQuestions().Where("run_id IN ?", runIDs), &types.RunQuestion{}},
				{tx.RunEvents().Where("run_id IN ?", runIDs), &types.RunEvent{}},
				{tx.Citations().Where("run_id IN ?", runIDs), &types.Citation{}},
			} {
				if err := del.q.Delete(del.model).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Artifacts().Where("dossier_id = ?", d.ID).Delete(&types.Artifact{}).Error; err != nil {
			return err
		}
		if err := tx.Runs().Where("dossier_id = ?", d.ID).Delete(&types.Run{}).Error; err != nil {
			return err
		}
		// A thread outlives the collection it was bound to; unbind it rather
		// than deleting the lawyer's conversation.
		if err := tx.Conversations().Where("dossier_id = ?", d.ID).
			Update("dossier_id", nil).Error; err != nil {
			return err
		}
		if len(removed) > 0 {
			if err := tx.Assets().Where("id IN ?", removed).Delete(&types.Asset{}).Error; err != nil {
				return err
			}
		}
		if len(kept) > 0 {
			// Objects still in S3: keep their rows so the retry can find them.
			return nil
		}
		return tx.Dossiers().Where("id = ?", d.ID).Delete(&types.Dossier{}).Error
	})
	if err != nil {
		dossiersLog.Errorf("Dossier purge failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Purge failed"}
	}

	complete := len(kept) == 0
	audit(purge, user.ID, "dossier.purge", "dossier", d.ID, jsonbOf(map[string]any{
		"pieces": len(pieceIDs), "runs": len(runIDs),
		"objects_removed": len(removed), "objects_kept": len(kept),
		"complete": complete,
	}))
	if complete {
		dossiersLog.Infof("Dossier purged (id=%s tenant=%s pieces=%d runs=%d objects=%d)",
			d.ID, repo.TenantID(), len(pieceIDs), len(runIDs), len(removed))
	} else {
		dossiersLog.Errorf("Dossier purge INCOMPLETE (id=%s tenant=%s objects_removed=%d objects_kept=%d) — the dossier stays purge_pending; retry the DELETE",
			d.ID, repo.TenantID(), len(removed), len(kept))
	}
	return http.StatusOK, gin.H{
		"status":          "purged",
		"id":              d.ID,
		"complete":        complete,
		"pieces":          len(pieceIDs),
		"runs":            len(runIDs),
		"objects_removed": len(removed),
		"objects_kept":    len(kept),
	}
}

// --- helpers ----------------------------------------------------------------

// loadDossier reads one dossier through the tenant predicate. A row belonging
// to another firm is reported as "not found", never as "forbidden": the two
// must be indistinguishable, or the 403 itself confirms the id exists.
func loadDossier(repo *u.TenantRepo, id string) (*types.Dossier, int, any) {
	if id == "" {
		return nil, http.StatusBadRequest, gin.H{"detail": "Invalid dossier id"}
	}
	var d types.Dossier
	if err := repo.Dossiers().Where("id = ?", id).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, http.StatusNotFound, gin.H{"detail": "Dossier not found"}
		}
		dossiersLog.Errorf("Dossier select failed (id=%s tenant=%s): %v", id, repo.TenantID(), err)
		return nil, http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	return &d, 0, nil
}

// purgeObjects removes the stored objects behind a set of asset ids and reports
// which asset rows may now be deleted and which must be kept for a retry.
// Keys are re-read from the tenant-scoped assets table and checked against the
// app prefix, so a purge can never delete outside <app>/.
func purgeObjects(ctx context.Context, repo *u.TenantRepo, assetIDs []string) (removed, kept []string) {
	if len(assetIDs) == 0 {
		return nil, nil
	}
	var assets []types.Asset
	if err := repo.Assets().Where("id IN ?", assetIDs).Find(&assets).Error; err != nil {
		dossiersLog.Errorf("Asset select failed during purge: %v", err)
		return nil, assetIDs
	}
	// Through the same blob store the uploads went to: S3 in the cluster,
	// the local directory in development. Both refuse a key outside this
	// app's prefix, whatever the row says.
	for _, a := range assets {
		if err := u.Blob.Delete(ctx, a.S3Key); err != nil {
			dossiersLog.Errorf("Object delete failed during purge (asset=%s backend=%s): %v",
				a.ID, u.Blob.Backend(), err)
			kept = append(kept, a.ID)
			continue
		}
		removed = append(removed, a.ID)
	}
	return removed, kept
}

// audit writes one compliance row. Best-effort by design: failing the caller's
// request because the audit insert failed would trade a working purge for a
// missing log line, so the failure is logged loudly instead. Never put
// document content in here (§7.4).
func audit(repo *u.TenantRepo, actorID, action, objectType, objectID string, meta types.JSONB) {
	ev := types.AuditEvent{
		ActorID:    actorID,
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		Metadata:   meta,
	}
	if err := repo.Create(&ev); err != nil {
		dossiersLog.Errorf("Audit insert failed (action=%s object=%s): %v", action, objectID, err)
	}
}

// jsonbOf marshals audit metadata. Shapes and counts only — no document
// content, no party names, no presigned URL (§7.4).
func jsonbOf(v map[string]any) types.JSONB {
	b, err := json.Marshal(v)
	if err != nil {
		dossiersLog.Warnf("Audit metadata marshal failed: %v", err)
		return nil
	}
	return types.JSONB(b)
}
