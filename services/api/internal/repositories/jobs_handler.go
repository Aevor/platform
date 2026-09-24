package repositories

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/jobs"
)

// Job statuses accepted as listing filters. Only these strings are allowed so
// the queue can never be probed with arbitrary values.
var knownJobStatuses = []string{
	jobs.StatusQueued,
	jobs.StatusRunning,
	jobs.StatusRetrying,
	jobs.StatusSucceeded,
	jobs.StatusFailed,
	jobs.StatusCancelRequested,
	jobs.StatusCancelled,
}

func knownJobStatus(value string) bool {
	for _, known := range knownJobStatuses {
		if known == value {
			return true
		}
	}
	return false
}

// jobEntry is the SAFE external shape of one background job. It carries
// lifecycle and reference data only — never payloads, results, or error text.
type jobEntry struct {
	ID             uuid.UUID       `json:"id"`
	RepositoryID   uuid.UUID       `json:"repository_id"`
	RunID          *uuid.UUID      `json:"run_id,omitempty"`
	Type           string          `json:"type"`
	Description    string          `json:"description,omitempty"`
	Status         string          `json:"status"`
	Priority       int             `json:"priority"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	ScheduledAt    time.Time       `json:"scheduled_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	CancelledAt    *time.Time      `json:"cancelled_at,omitempty"`
	CurrentStage   string          `json:"current_stage"`
	ErrorCategory  string          `json:"error_category"`
	ErrorReference string          `json:"error_reference"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

func toJobEntry(job *jobs.Job, registry *jobs.Registry) jobEntry {
	entry := jobEntry{
		ID:             job.ID,
		RepositoryID:   job.SelectedRepositoryID,
		RunID:          job.RunID,
		Type:           job.Type,
		Status:         job.Status,
		Priority:       job.Priority,
		IdempotencyKey: job.IdempotencyKey,
		Attempts:       job.Attempts,
		MaxAttempts:    job.MaxAttempts,
		ScheduledAt:    job.ScheduledAt,
		StartedAt:      job.StartedAt,
		CompletedAt:    job.CompletedAt,
		CancelledAt:    job.CancelledAt,
		CurrentStage:   job.CurrentStage,
		ErrorCategory:  job.ErrorCategory,
		ErrorReference: job.ErrorReference,
		CreatedAt:      job.CreatedAt,
		UpdatedAt:      job.UpdatedAt,
	}

	if registry != nil {
		if def, err := registry.Get(job.Type); err == nil {
			entry.Description = def.Description
		}
	}

	if len(job.Result) > 0 && string(job.Result) != "{}" && string(job.Result) != "null" {
		entry.Result = json.RawMessage(job.Result)
	}

	return entry
}

// jobListResponse is the external shape of a job listing.
type jobListResponse struct {
	RepositoryID string     `json:"repository_id"`
	Jobs         []jobEntry `json:"jobs"`
	Count        int        `json:"count"`
	Page         int        `json:"page"`
	PerPage      int        `json:"per_page"`
	HasMore      bool       `json:"has_more"`
	Status       string     `json:"status"`
}

// jobDetailResponse is the external shape of one job.
type jobDetailResponse struct {
	RepositoryID string   `json:"repository_id"`
	Job          jobEntry `json:"job"`
	Status       string   `json:"status"`
}

// jobEventListResponse is the external shape of a job's event timeline (the
// linked engineering run's audit events). Jobs without a linked run have an
// empty timeline.
type jobEventListResponse struct {
	RepositoryID string                     `json:"repository_id"`
	JobID        uuid.UUID                  `json:"job_id"`
	Events       []EngineeringEventResponse `json:"events"`
	Status       string                     `json:"status"`
}

// jobsEnabled guards every job route: without a wired job engine the route is
// a server misconfiguration, never a foreign-state probe.
func (h *Handler) jobsEnabled(c *gin.Context) bool {
	if h.jobs != nil {
		return true
	}
	c.JSON(http.StatusNotImplemented, gin.H{"error": "jobs_not_initialized"})
	return false
}

// ListJobs handles GET /repositories/:id/jobs for the authenticated user: the
// repository's background-job history, newest first, with optional status or
// type filters. Ownership is resolved here so foreign repositories collapse to
// the same opaque 404 as unknown ones.
func (h *Handler) ListJobs(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.jobsEnabled(c) {
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	if _, err := h.service.store.FindByUserAndID(userID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}

	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	status := c.Query("status")
	if status != "" && !knownJobStatus(status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	typ := c.Query("type")
	if typ != "" {
		if _, err := h.jobs.Registry().Get(typ); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
	}

	jobsList, count, err := h.jobs.List(c.Request.Context(), userID, id, jobs.ListOptions{
		Status: status,
		Type:   typ,
		Limit:  perPage,
		Offset: (page - 1) * perPage,
		Count:  true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}

	entries := make([]jobEntry, 0, len(jobsList))
	for i := range jobsList {
		entries = append(entries, toJobEntry(&jobsList[i], h.jobs.Registry()))
	}

	c.JSON(http.StatusOK, jobListResponse{
		RepositoryID: id.String(),
		Jobs:         entries,
		Count:        count,
		Page:         page,
		PerPage:      perPage,
		HasMore:      count > page*perPage,
		Status:       "ok",
	})
}

// GetJob handles GET /repositories/:id/jobs/:jobID for the authenticated user.
// Foreign or unknown jobs collapse to job_not_found.
func (h *Handler) GetJob(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.jobsEnabled(c) {
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	jobID, err := uuid.Parse(c.Param("jobID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	if _, err := h.service.store.FindByUserAndID(userID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}

	job, err := h.jobs.Get(c.Request.Context(), userID, id, jobID)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "job_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	c.JSON(http.StatusOK, jobDetailResponse{
		RepositoryID: id.String(),
		Job:          toJobEntry(job, h.jobs.Registry()),
		Status:       "ok",
	})
}

// ListJobEvents handles GET /repositories/:id/jobs/:jobID/events: the audit
// timeline persisted for the job's linked engineering run, in time order. A
// job with no linked run (repository-wide work) has an empty timeline by
// design — its progress is surfaced through the job itself.
func (h *Handler) ListJobEvents(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.jobsEnabled(c) {
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	jobID, err := uuid.Parse(c.Param("jobID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	page, perPage, err := paginationParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	if _, err := h.service.store.FindByUserAndID(userID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}

	job, err := h.jobs.Get(c.Request.Context(), userID, id, jobID)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "job_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	var entries []EngineeringEventResponse
	if job.RunID != nil {
		events, _, listErr := h.service.ListRunEvents(c.Request.Context(), userID, id, *job.RunID, page, perPage)
		if listErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
			return
		}
		entries = make([]EngineeringEventResponse, 0, len(events))
		for _, event := range events {
			entries = append(entries, toEngineeringEventResponse(event))
		}
	}

	c.JSON(http.StatusOK, jobEventListResponse{
		RepositoryID: id.String(),
		JobID:        job.ID,
		Events:       entries,
		Status:       "ok",
	})
}

// CancelJob handles POST /repositories/:id/jobs/:jobID/cancel. A queued or
// retrying job is cancelled immediately; a running job is flagged and the
// worker completes the transition cooperatively. Terminal jobs are immutable.
func (h *Handler) CancelJob(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.jobsEnabled(c) {
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	jobID, err := uuid.Parse(c.Param("jobID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	if _, err := h.service.store.FindByUserAndID(userID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}

	job, err := h.jobs.Cancel(c.Request.Context(), userID, id, jobID)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "job_not_found"})
		case errors.Is(err, jobs.ErrNotActive):
			c.JSON(http.StatusConflict, gin.H{"error": "job_not_active"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	c.JSON(http.StatusOK, jobDetailResponse{
		RepositoryID: id.String(),
		Job:          toJobEntry(job, h.jobs.Registry()),
		Status:       "ok",
	})
}

// RetryJob handles POST /repositories/:id/jobs/:jobID/retry: re-queues a
// FAILED owned job so the worker picks it up for a fresh set of attempts.
func (h *Handler) RetryJob(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.jobsEnabled(c) {
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	jobID, err := uuid.Parse(c.Param("jobID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	if _, err := h.service.store.FindByUserAndID(userID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
		return
	}

	job, err := h.jobs.Retry(c.Request.Context(), userID, id, jobID)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "job_not_found"})
		case errors.Is(err, jobs.ErrNotFailed):
			c.JSON(http.StatusConflict, gin.H{"error": "job_not_failed"})
		case errors.Is(err, jobs.ErrNotActive), errors.Is(err, jobs.ErrNotTerminal):
			c.JSON(http.StatusConflict, gin.H{"error": "job_not_retryable"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}

	c.JSON(http.StatusOK, jobDetailResponse{
		RepositoryID: id.String(),
		Job:          toJobEntry(job, h.jobs.Registry()),
		Status:       "ok",
	})
}
