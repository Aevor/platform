package repositories

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
)

type webhookActivityListResponse struct {
	RepositoryID string            `json:"repository_id"`
	Activity     []WebhookActivity `json:"activity"`
	Count        int               `json:"count"`
	Page         int               `json:"page"`
	PerPage      int               `json:"per_page"`
	HasMore      bool              `json:"has_more"`
	Status       string            `json:"status"`
}

func (h *Handler) ListWebhookActivity(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	selectedRepositoryID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	if _, err := h.service.store.FindByUserAndID(userID, selectedRepositoryID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}
	if h.service.webhookStore == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "webhooks_not_initialized"})
		return
	}
	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	activity, count, err := h.service.ListWebhookActivity(c.Request.Context(), userID, selectedRepositoryID, page, perPage)
	if err != nil {
		if errors.Is(err, ErrWebhookStoreNotConfigured) {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "webhooks_not_initialized"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	hasMore := page*perPage < count
	c.JSON(http.StatusOK, webhookActivityListResponse{
		RepositoryID: selectedRepositoryID.String(),
		Activity:     activity,
		Count:        count,
		Page:         page,
		PerPage:      perPage,
		HasMore:      hasMore,
		Status:       "ok",
	})
}
