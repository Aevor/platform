package repositories

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
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
