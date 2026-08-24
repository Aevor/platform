package repositories

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/discovery"
	"github.com/Aevor/platform/services/api/internal/extraction"
	"github.com/Aevor/platform/services/api/internal/filtering"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

const (
	defaultPage    = 1
	defaultPerPage = 30
	maxPerPage     = 100
)

var errInvalidPagination = errors.New("invalid pagination parameters")

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{
		service: service,
	}
}

// List handles GET /repositories. The Aevor identity comes exclusively from
// the RequireAuth middleware (verified JWT); query parameters or headers can
// never select another user's credentials.
func (h *Handler) List(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	page, perPage, err := paginationParams(c)

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_pagination",
		})
		return
	}

	repositories, hasMore, err := h.service.ListForUser(c.Request.Context(), userID, page, perPage)

	if err != nil {
		switch {
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrUnauthorized):
			// The stored GitHub token was rejected by GitHub (revoked or
			// expired): the remedy is a fresh OAuth login.
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		default:
			// Covers decryption failures and anything unexpected: internal
			// detail (key/ciphertext problems) is never surfaced.
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, ToListResponse(repositories, page, perPage, hasMore))
}

func paginationParams(
	c *gin.Context,
) (int, int, error) {
	page := defaultPage
	perPage := defaultPerPage

	if raw := c.Query("page"); raw != "" {
		value, err := strconv.Atoi(raw)

		if err != nil || value < 1 {
			return 0, 0, errInvalidPagination
		}

		page = value
	}

	if raw := c.Query("per_page"); raw != "" {
		value, err := strconv.Atoi(raw)

		if err != nil || value < 1 {
			return 0, 0, errInvalidPagination
		}

		if value > maxPerPage {
			value = maxPerPage
		}

		perPage = value
	}

	return page, perPage, nil
}

// Select handles POST /repositories. The authenticated user's own GitHub
// token decides whether the repository may be selected; the request body only
// names the GitHub repository ID.
func (h *Handler) Select(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	var request SelectRequest

	if err := c.ShouldBindJSON(&request); err != nil || request.GithubRepositoryID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	selected, err := h.service.SelectForUser(c.Request.Context(), userID, request.GithubRepositoryID)

	if err != nil {
		switch {
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrRepositoryNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, github.ErrUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, ToSelectedRepositoryResponse(*selected))
}

// ListSelected handles GET /repositories/selected: only the authenticated
// user's persisted repository contexts.
func (h *Handler) ListSelected(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	selected, err := h.service.ListSelected(userID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	c.JSON(http.StatusOK, ToSelectedListResponse(selected))
}

// Delete handles DELETE /repositories/:id where :id is the Aevor record ID.
// Unknown and foreign IDs are deliberately indistinguishable (404).
func (h *Handler) Delete(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	err = h.service.RemoveSelected(userID, id)

	if err != nil {
		if errors.Is(err, ErrSelectedNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// SyncIssues handles POST /repositories/:id/issues/sync where :id is the
// authenticated user's Aevor selected-repository UUID. The GitHub repository,
// credentials, and ownership are all resolved server-side; the client names
// nothing but its own verified identity.
func (h *Handler) SyncIssues(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.SyncIssues(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrRepositoryNotFound):
			// Repository deleted/renamed on GitHub since selection.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, github.ErrUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// SyncPullRequests handles POST /repositories/:id/pull-requests/sync where
// :id is the authenticated user's Aevor selected-repository UUID. Ownership,
// credentials, and the GitHub repository identity are all resolved
// server-side; the client names nothing but its own verified identity.
func (h *Handler) SyncPullRequests(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.SyncPullRequests(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrRepositoryNotFound):
			// Repository deleted/renamed on GitHub since selection.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, github.ErrUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// SyncCommits handles POST /repositories/:id/commits/sync for the
// authenticated user. Error mapping is identical to SyncPullRequests.
func (h *Handler) SyncCommits(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.SyncCommits(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrRepositoryNotFound):
			// Repository deleted/renamed on GitHub since selection.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, github.ErrUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// Clone handles POST /repositories/:id/clone for the authenticated user.
// The response never contains filesystem paths, clone URLs, or tokens.
func (h *Handler) Clone(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.CloneRepository(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, users.ErrGitHubTokenMissing):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "github_token_missing",
			})
		case errors.Is(err, github.ErrRepositoryNotFound):
			// Repository deleted/renamed on GitHub since selection.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, github.ErrUnauthorized):
		case errors.Is(err, workspace.ErrAuthRejected):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_token_invalid",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrUnavailable),
			errors.Is(err, github.ErrInvalidResponse),
			errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_unavailable",
			})
		case errors.Is(err, workspace.ErrTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error": "clone_timeout",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// discoverResponse is the SAFE external shape of a discovery result:
// aggregates and relative names only — never absolute filesystem paths,
// never file contents.
type discoverResponse struct {
	RepositoryID   string         `json:"repository_id"`
	Files          int            `json:"files"`
	Directories    int            `json:"directories"`
	Languages      map[string]int `json:"languages"`
	ImportantFiles []string       `json:"important_files,omitempty"`
	Truncated      bool           `json:"truncated"`
	Status         string         `json:"status"`
}

// Discover handles POST /repositories/:id/discover for the authenticated
// user. The workspace must already exist (clone first); discovery itself is
// read-only and metadata-only.
func (h *Handler) Discover(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	summary, err := h.service.DiscoverRepository(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, ErrWorkspaceNotReady):
			c.JSON(http.StatusConflict, gin.H{
				"error": "workspace_not_ready",
			})
		case errors.Is(err, discovery.ErrTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error": "discovery_timeout",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	if summary.Languages == nil {
		summary.Languages = make(map[string]int)
	}

	c.JSON(http.StatusOK, discoverResponse{
		RepositoryID:   id.String(),
		Files:          summary.Files,
		Directories:    summary.Directories,
		Languages:      summary.Languages,
		ImportantFiles: summary.ImportantFiles,
		Truncated:      summary.Truncated,
		Status:         "discovered",
	})
}

// filterResponse is the SAFE external shape of a filtering result: aggregate
// counts, per-file DECISIONS (path/size/extension/language/category/included/
// reason), and flags — never absolute filesystem paths, never file contents.
type filterResponse struct {
	RepositoryID       string               `json:"repository_id"`
	TotalFiles         int                  `json:"total_files"`
	IncludedFiles      int                  `json:"included_files"`
	ExcludedFiles      int                  `json:"excluded_files"`
	TotalSelectedBytes int64                `json:"total_selected_bytes"`
	Languages          map[string]int       `json:"languages"`
	ExclusionSummary   map[string]int       `json:"exclusion_summary"`
	Files              []filterFileDecision `json:"files"`
	FilesTruncated     bool                 `json:"files_truncated"`
	IgnoredDirectories int                  `json:"ignored_directories"`
	SymlinksSkipped    int                  `json:"symlinks_skipped"`
	Truncated          bool                 `json:"truncated"`
	Status             string               `json:"status"`
}

type filterFileDecision struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Extension string `json:"extension"`
	Language  string `json:"language"`
	Category  string `json:"category"`
	Included  bool   `json:"included"`
	Reason    string `json:"reason"`
}

func toFilterResponse(repositoryID string, result *filtering.Result) filterResponse {
	if result.Languages == nil {
		result.Languages = make(map[string]int)
	}

	if result.ExclusionSummary == nil {
		result.ExclusionSummary = make(map[string]int)
	}

	files := make([]filterFileDecision, 0, len(result.Files))

	for _, decision := range result.Files {
		files = append(files, filterFileDecision{
			Path:      decision.Path,
			Size:      decision.Size,
			Extension: decision.Extension,
			Language:  decision.Language,
			Category:  decision.Category,
			Included:  decision.Included,
			Reason:    decision.Reason,
		})
	}

	return filterResponse{
		RepositoryID:       repositoryID,
		TotalFiles:         result.TotalFiles,
		IncludedFiles:      result.IncludedFiles,
		ExcludedFiles:      result.ExcludedFiles,
		TotalSelectedBytes: result.TotalSelectedBytes,
		Languages:          result.Languages,
		ExclusionSummary:   result.ExclusionSummary,
		Files:              files,
		FilesTruncated:     result.FilesTruncated,
		IgnoredDirectories: result.IgnoredDirectories,
		SymlinksSkipped:    result.SymlinksSkipped,
		Truncated:          result.Truncated,
		Status:             result.Status,
	}
}

// Filter handles POST /repositories/:id/filter for the authenticated user.
// The workspace must already exist (clone first); filtering is read-only and
// metadata-only. The response carries decisions and counts — never source
// contents and never absolute paths.
func (h *Handler) Filter(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.FilterRepository(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, ErrWorkspaceNotReady):
			c.JSON(http.StatusConflict, gin.H{
				"error": "workspace_not_ready",
			})
		case errors.Is(err, filtering.ErrTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error": "filter_timeout",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, toFilterResponse(id.String(), result))
}

// maxExtractedFilesInResponse caps how many per-file entries the extract
// endpoint returns. The internal representation stays complete; only the
// external list is bounded.
const maxExtractedFilesInResponse = 1000

// extractFileMeta is the SAFE external shape of one extracted file:
// metadata and hash only — never content.
type extractFileMeta struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Extension   string `json:"extension"`
	Language    string `json:"language"`
	ContentHash string `json:"content_hash"`
}

// extractResponse is the SAFE external shape of an extraction result.
// There is deliberately NO content field: extracted contents are an internal
// representation for future pipeline stages and never cross HTTP.
type extractResponse struct {
	RepositoryID    string            `json:"repository_id"`
	TotalCandidates int               `json:"total_candidates"`
	ExtractedCount  int               `json:"extracted_count"`
	ExtractedBytes  int64             `json:"extracted_bytes"`
	Skipped         map[string]int    `json:"skipped"`
	SkippedCount    int               `json:"skipped_count"`
	Complete        bool              `json:"complete"`
	Files           []extractFileMeta `json:"files"`
	FilesTruncated  bool              `json:"files_truncated"`
	Status          string            `json:"status"`
}

func toExtractResponse(repositoryID string, result *extraction.Result) extractResponse {
	if result.Skipped == nil {
		result.Skipped = make(map[string]int)
	}

	limit := maxExtractedFilesInResponse

	if len(result.Files) < limit {
		limit = len(result.Files)
	}

	files := make([]extractFileMeta, 0, limit)

	for _, file := range result.Files[:limit] {
		files = append(files, extractFileMeta{
			Path:        file.Path,
			Size:        file.Size,
			Extension:   file.Extension,
			Language:    file.Language,
			ContentHash: file.ContentHash,
		})
	}

	return extractResponse{
		RepositoryID:    repositoryID,
		TotalCandidates: result.TotalCandidates,
		ExtractedCount:  result.ExtractedCount,
		ExtractedBytes:  result.ExtractedBytes,
		Skipped:         result.Skipped,
		SkippedCount:    result.SkippedCount,
		Complete:        result.Complete,
		Files:           files,
		FilesTruncated:  len(result.Files) > maxExtractedFilesInResponse,
		Status:          result.Status,
	}
}

// Extract handles POST /repositories/:id/extract for the authenticated user.
// Extraction is read-only and bounded; the response carries counts plus
// per-file METADATA (path/size/extension/language/hash) — never source
// contents, never absolute filesystem paths.
func (h *Handler) Extract(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.ExtractRepositoryContent(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, ErrWorkspaceNotReady):
			c.JSON(http.StatusConflict, gin.H{
				"error": "workspace_not_ready",
			})
		case errors.Is(err, filtering.ErrTimeout), errors.Is(err, extraction.ErrTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error": "extract_timeout",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, toExtractResponse(id.String(), result))
}

// maxChunkFilesInResponse caps how many per-file summaries the chunk
// endpoint returns. The internal result stays complete; only the external
// list is bounded.
const maxChunkFilesInResponse = 1000

// chunkFileSummary is the SAFE external shape of one chunked file: counts
// and flags only. Chunk CONTENTS, hashes, line ranges, and symbol metadata
// are internal to the future pipeline stages.
type chunkFileSummary struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	Chunks    int    `json:"chunks"`
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// chunkResponse is the SAFE external shape of a chunking run. There is
// deliberately NO content field: chunks are an internal representation for
// future pipeline stages and never cross HTTP.
type chunkResponse struct {
	RepositoryID   string             `json:"repository_id"`
	TotalFiles     int                `json:"total_files"`
	FilesChunked   int                `json:"files_chunked"`
	EmptyFiles     int                `json:"empty_files"`
	TotalChunks    int                `json:"total_chunks"`
	TotalBytes     int64              `json:"total_bytes"`
	Truncated      bool               `json:"truncated"`
	SkippedSummary map[string]int     `json:"skipped_summary"`
	Files          []chunkFileSummary `json:"files"`
	FilesTruncated bool               `json:"files_truncated"`
	Status         string             `json:"status"`
}

func toChunkResponse(repositoryID string, result *chunking.Result) chunkResponse {
	if result.SkippedSummary == nil {
		result.SkippedSummary = make(map[string]int)
	}

	limit := maxChunkFilesInResponse

	if len(result.Files) < limit {
		limit = len(result.Files)
	}

	files := make([]chunkFileSummary, 0, limit)

	for _, summary := range result.Files[:limit] {
		files = append(files, chunkFileSummary{
			Path:      summary.Path,
			Language:  summary.Language,
			Chunks:    summary.Chunks,
			Bytes:     summary.Bytes,
			Truncated: summary.Truncated,
		})
	}

	return chunkResponse{
		RepositoryID:   repositoryID,
		TotalFiles:     result.TotalFiles,
		FilesChunked:   result.FilesChunked,
		EmptyFiles:     result.EmptyFiles,
		TotalChunks:    result.TotalChunks,
		TotalBytes:     result.TotalBytes,
		Truncated:      result.Truncated,
		SkippedSummary: result.SkippedSummary,
		Files:          files,
		FilesTruncated: len(result.Files) > maxChunkFilesInResponse,
		Status:         result.Status,
	}
}

// Chunk handles POST /repositories/:id/chunk for the authenticated user. It
// reuses the extract flow (ownership first, readiness gate, bounded reads)
// and returns aggregate + per-file METADATA — never chunk contents, never
// absolute filesystem paths.
func (h *Handler) Chunk(
	c *gin.Context,
) {
	userID, ok := auth.GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	id, err := uuid.Parse(c.Param("id"))

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_request",
		})
		return
	}

	result, err := h.service.ChunkRepositoryContent(c.Request.Context(), userID, id)

	if err != nil {
		switch {
		case errors.Is(err, ErrSelectedNotFound):
			// Unknown AND foreign records map identically: existence of
			// another user's repository context is never revealed.
			c.JSON(http.StatusNotFound, gin.H{
				"error": "repository_not_found",
			})
		case errors.Is(err, users.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
		case errors.Is(err, ErrWorkspaceNotReady):
			c.JSON(http.StatusConflict, gin.H{
				"error": "workspace_not_ready",
			})
		case errors.Is(err, filtering.ErrTimeout), errors.Is(err, extraction.ErrTimeout):
			c.JSON(http.StatusGatewayTimeout, gin.H{
				"error": "extract_timeout",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal",
			})
		}
		return
	}

	c.JSON(http.StatusOK, toChunkResponse(id.String(), result))
}
