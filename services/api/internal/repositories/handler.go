package repositories

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
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
