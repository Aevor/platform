package repositories

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
)

// ListRuns handles GET /repositories/:id/runs for the authenticated user:
// the repository's engineering-run history, newest first, with optional
// status / stage / issue / created-at filters.
func (h *Handler) ListRuns(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	filter, ok := parseRunFilter(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	runs, hasMore, err := h.service.ListRunsForRepository(c.Request.Context(), userID, id, page, perPage, filter)
	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign repositories map identically: another user's
			// run history existence is never revealed.
			c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	entries := make([]EngineeringRunResponse, 0, len(runs))
	for _, run := range runs {
		entries = append(entries, toEngineeringRunResponse(run))
	}

	c.JSON(http.StatusOK, runsListResponse{
		RepositoryID: id.String(),
		Runs:         entries,
		Page:         page,
		PerPage:      perPage,
		HasMore:      hasMore,
		Status:       "ok",
	})
}

// GetRun handles GET /repositories/:id/runs/:runID for the authenticated user.
// Foreign or unknown runs collapse to run_not_found; foreign repositories to
// repository_not_found.
func (h *Handler) GetRun(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	runID, err := uuid.Parse(c.Param("runID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	run, err := h.service.GetRun(c.Request.Context(), userID, id, runID)
	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		case errors.Is(err, ErrRunNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	c.JSON(http.StatusOK, runDetailsResponse{
		RepositoryID: id.String(),
		Run:          toEngineeringRunResponse(*run),
		Status:       "ok",
	})
}

// ListRunEvents handles GET /repositories/:id/runs/:runID/events for the
// authenticated user: the run's audit timeline in time order.
func (h *Handler) ListRunEvents(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	runID, err := uuid.Parse(c.Param("runID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	events, hasMore, err := h.service.ListRunEvents(c.Request.Context(), userID, id, runID, page, perPage)
	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		case errors.Is(err, ErrRunNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "run_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	entries := make([]EngineeringEventResponse, 0, len(events))
	for _, event := range events {
		entries = append(entries, toEngineeringEventResponse(event))
	}

	c.JSON(http.StatusOK, runEventListResponse{
		RunID:   runID.String(),
		Events:  entries,
		Page:    page,
		PerPage: perPage,
		HasMore: hasMore,
		Status:  "ok",
	})
}

// ListRepositoryActivity handles GET /repositories/:id/activity for the
// authenticated user: the repository's most-recently-active engineering runs
// with the issue each belongs to.
func (h *Handler) ListRepositoryActivity(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	items, hasMore, err := h.service.RepositoryActivity(c.Request.Context(), userID, id, page, perPage)
	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	entries := make([]activityItemResponse, 0, len(items))
	for _, item := range items {
		response := activityItemResponse{
			Run:         toEngineeringRunResponse(item.Run),
			IssueTitle:  item.IssueTitle,
			IssueNumber: item.IssueNumber,
		}
		entries = append(entries, response)
	}

	c.JSON(http.StatusOK, activityListResponse{
		RepositoryID: id.String(),
		Activity:     entries,
		HasMore:      hasMore,
		Status:       "ok",
	})
}

// parseRunFilter parses the optional listing filters for GET /repositories/:id/runs.
// Unknown status or stage values are rejected so the history can never be
// probed with arbitrary strings; issue_id and from are strictly validated.
func parseRunFilter(c *gin.Context) (RunFilter, bool) {
	var filter RunFilter

	if raw := c.Query("status"); raw != "" {
		if !inKnownRunStatuses(raw) {
			return RunFilter{}, false
		}
		filter.Status = raw
	}

	if raw := c.Query("stage"); raw != "" {
		if !inKnownRunStages(raw) {
			return RunFilter{}, false
		}
		filter.Stage = raw
	}

	if raw := c.Query("issue_id"); raw != "" {
		issueID, err := uuid.Parse(raw)
		if err != nil {
			return RunFilter{}, false
		}
		filter.IssueID = issueID
	}

	if raw := c.Query("from"); raw != "" {
		from, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return RunFilter{}, false
		}
		filter.CreatedAt = from
	}

	return filter, true
}

var knownRunStatuses = []string{
	RunStatusCreated,
	RunStatusAnalyzing,
	RunStatusPlanning,
	RunStatusGenerating,
	RunStatusReviewing,
	RunStatusApproved,
	RunStatusApplying,
	RunStatusValidating,
	RunStatusDelivering,
	RunStatusPrOpen,
	RunStatusIterating,
	RunStatusCompleted,
	RunStatusFailedAnalysis,
	RunStatusFailedGeneration,
	RunStatusFailedValidation,
	RunStatusFailedDelivery,
}

var knownRunStages = []string{
	"ANALYZING",
	"PLANNING",
	"GENERATING",
	"REVIEWING",
	"VALIDATING",
	"DELIVERING",
	"PR_OPEN",
	"ITERATING",
	"COMPLETED",
}

func inKnownRunStatuses(value string) bool {
	for _, known := range knownRunStatuses {
		if known == value {
			return true
		}
	}
	return false
}

func inKnownRunStages(value string) bool {
	for _, known := range knownRunStages {
		if known == value {
			return true
		}
	}
	return false
}
