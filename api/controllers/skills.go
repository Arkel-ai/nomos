package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// ListSkills → GET /skills
func ListSkills(c *gin.Context) {
	var q types.ListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListSkills(c, q))
}

// CreateSkill → POST /skills
func CreateSkill(c *gin.Context) {
	var body types.SkillCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.CreateSkill(c, body))
}

// GetSkill → GET /skills/:id
func GetSkill(c *gin.Context) {
	c.JSON(services.GetSkill(c, c.Param("id")))
}

// DeleteSkill → DELETE /skills/:id
func DeleteSkill(c *gin.Context) {
	c.JSON(services.DeleteSkill(c, c.Param("id")))
}
