package services

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var memoryLog = u.NewLogger("services.memory")

// Persistent style memory (docs/PLAN.md §8) — explicit, inspectable, editable,
// deletable, and always distinguishing user-approved from inferred. Nomos does
// NOT silently turn conversation text into permanent firm memory: that is a
// consent, leakage and surprising-behaviour problem before it is a feature.
// Scoped per tenant and per user; one firm's memory never reaches another.
//
// Two invariants hold everywhere below:
//
//   - An entry the model INFERRED lands in `proposed` and stays there. There
//     is no code path that promotes it; only a human POST carrying
//     provenance=user_approved does, and that POST is the act of consent.
//   - Every read and write goes through the tenant repository. A preference
//     leaking across firms is a secret-professionnel breach, not a bug (§6.1).

// memoryMaxValueBytes bounds one stored preference. Memory is a handful of
// style rules, never a place to park a document.
const memoryMaxValueBytes = 8 * 1024

// memoryMaxKeyRunes keeps the key inside its indexed column.
const memoryMaxKeyRunes = 128

// ListMemory returns the caller's memory entries, both approved and merely
// proposed, so the user can see exactly what the system believes about them.
//
// The window is the caller's own entries plus the firm-scoped ones of the same
// tenant — a cabinet-wide rule is meant to be visible to the cabinet. It never
// widens past the tenant.
func ListMemory(c *gin.Context, q types.ListQuery) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c)
	limit, offset := q.Normalized()

	visible := func() *gorm.DB {
		return repo.MemoryPreferences().
			Where("user_id = ? OR scope = ?", user.ID, types.MemoryScopeFirm)
	}

	var total int64
	if err := visible().Count(&total).Error; err != nil {
		memoryLog.Errorf("Memory count failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}
	rows := []types.MemoryPreference{}
	if err := visible().
		Order("scope ASC, key ASC").Limit(limit).Offset(offset).
		Find(&rows).Error; err != nil {
		memoryLog.Errorf("Memory select failed (tenant=%s): %v", repo.TenantID(), err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	memoryLog.Debugf("Listed memory (tenant=%s rows=%d total=%d)", repo.TenantID(), len(rows), total)
	return http.StatusOK, types.ListResponse{Items: rows, Total: total, Limit: limit, Offset: offset}
}

// UpsertMemory creates or replaces one entry, keyed by
// (tenant, user, scope, key). An entry the user asked for is `user_approved`
// and active immediately; an entry the model inferred is created `proposed`
// and is NOT used until a human approves it.
func UpsertMemory(c *gin.Context, body types.MemoryUpsertRequest) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c)

	scope := strings.TrimSpace(body.Scope)
	if scope != types.MemoryScopeUser && scope != types.MemoryScopeFirm {
		return http.StatusBadRequest, gin.H{"detail": "Scope must be user or firm"}
	}
	key := strings.TrimSpace(body.Key)
	if key == "" || len([]rune(key)) > memoryMaxKeyRunes {
		return http.StatusBadRequest, gin.H{"detail": "Key is required and must be at most 128 characters"}
	}
	if len(body.Value) > memoryMaxValueBytes {
		return http.StatusBadRequest, gin.H{"detail": "Value is too large for a memory preference"}
	}
	value := types.JSONB(nil)
	if len(body.Value) > 0 && strings.TrimSpace(string(body.Value)) != "null" {
		if !json.Valid(body.Value) {
			return http.StatusBadRequest, gin.H{"detail": "Value is not valid JSON"}
		}
		value = body.Value
	}

	// The default is user_approved because this endpoint is a human act. An
	// inferred entry must SAY it is inferred, and it lands proposed.
	provenance := strings.TrimSpace(body.Provenance)
	if provenance == "" {
		provenance = types.MemoryProvenanceApproved
	}
	var state string
	var approvedAt *int64
	switch provenance {
	case types.MemoryProvenanceApproved:
		state = types.MemoryStateApproved
		now := time.Now().Unix()
		approvedAt = &now
	case types.MemoryProvenanceInferred:
		state = types.MemoryStateProposed
	default:
		return http.StatusBadRequest, gin.H{"detail": "Provenance must be user_approved or inferred"}
	}

	entry := types.MemoryPreference{
		ID:         uuid.NewString(),
		UserID:     user.ID,
		Scope:      scope,
		Key:        key,
		Value:      value,
		Provenance: provenance,
		State:      state,
		ApprovedAt: approvedAt,
		ExpiresAt:  body.ExpiresAt,
	}

	// Replace by natural key. The unique index is (tenant, user, scope, key),
	// so a re-post of the same preference updates one row rather than growing
	// a pile of near-duplicates the user then has to audit.
	err := repo.Tx(func(tx *u.TenantRepo) error {
		var existing types.MemoryPreference
		err := tx.MemoryPreferences().
			Where("user_id = ? AND scope = ? AND key = ?", user.ID, scope, key).
			Take(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&entry)
		}
		if err != nil {
			return err
		}
		entry.ID = existing.ID
		entry.CreatedAt = existing.CreatedAt
		return tx.MemoryPreferences().Where("id = ?", existing.ID).Updates(map[string]any{
			"value":       value,
			"provenance":  provenance,
			"state":       state,
			"approved_at": approvedAt,
			"expires_at":  body.ExpiresAt,
		}).Error
	})
	if err != nil {
		memoryLog.Errorf("Memory upsert failed (tenant=%s scope=%s): %v", repo.TenantID(), scope, err)
		return http.StatusBadGateway, gin.H{"detail": "Database write failed"}
	}

	// Shapes only: the key is a style rule name, the value never appears here
	// and never reaches a log line (§7.4).
	audit(repo, user.ID, "memory.upsert", "memory_preference", entry.ID, jsonbOf(map[string]any{
		"scope": scope, "provenance": provenance, "state": state,
	}))
	memoryLog.Infof("Memory preference stored (tenant=%s scope=%s provenance=%s state=%s value_bytes=%d)",
		repo.TenantID(), scope, provenance, state, len(value))
	return http.StatusOK, entry
}

// DeleteMemory removes one entry. Deletion is real — an entry the user removed
// must stop influencing output immediately.
//
// A user deletes their own entries; a firm-scoped entry belongs to the cabinet
// and any member of the tenant may remove it. Nothing outside the tenant is
// reachable, and a row that is not visible answers 404 rather than 403: the
// two must be indistinguishable to the caller.
func DeleteMemory(c *gin.Context, id string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	user := middlewares.CurrentUser(c)

	res := repo.MemoryPreferences().
		Where("id = ?", id).
		Where("user_id = ? OR scope = ?", user.ID, types.MemoryScopeFirm).
		Delete(&types.MemoryPreference{})
	if res.Error != nil {
		memoryLog.Errorf("Memory delete failed (tenant=%s entry=%s): %v", repo.TenantID(), id, res.Error)
		return http.StatusBadGateway, gin.H{"detail": "Database delete failed"}
	}
	if res.RowsAffected == 0 {
		return http.StatusNotFound, gin.H{"detail": "Memory preference not found"}
	}

	audit(repo, user.ID, "memory.delete", "memory_preference", id, nil)
	memoryLog.Infof("Memory preference deleted (tenant=%s entry=%s)", repo.TenantID(), id)
	return http.StatusOK, gin.H{"id": id}
}
