package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

const (
	oauthCookieName   = "aevor_oauth_state"
	oauthCookiePath   = "/auth"
	oauthCookieMaxAge = 600
	oauthCookieSecure = false
)

type oauthStateCookie struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
}

type Handler struct {
	service *Service
}

func NewHandler(
	service *Service,
) *Handler {
	return &Handler{
		service: service,
	}
}

func (h *Handler) GitHubLogin(
	c *gin.Context,
) {
	loginURL, state, verifier, err := h.service.LoginURL()

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	payload, err := json.Marshal(oauthStateCookie{
		State:    state,
		Verifier: verifier,
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)

	c.SetCookie(
		oauthCookieName,
		string(payload),
		oauthCookieMaxAge,
		oauthCookiePath,
		"",
		oauthCookieSecure,
		true,
	)

	c.Redirect(http.StatusFound, loginURL)
}

func (h *Handler) GitHubCallback(
	c *gin.Context,
) {
	h.clearOAuthStateCookie(c)

	cookieValue, err := c.Cookie(oauthCookieName)

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_state",
		})
		return
	}

	var stored oauthStateCookie

	if err := json.Unmarshal([]byte(cookieValue), &stored); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_state",
		})
		return
	}

	if stored.State == "" || stored.Verifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid_state",
		})
		return
	}

	params := CallbackParams{
		Code:          c.Query("code"),
		ActualState:   c.Query("state"),
		ExpectedState: stored.State,
		CodeVerifier:  stored.Verifier,
		GitHubError:   c.Query("error"),
	}

	profile, err := h.service.HandleCallback(c.Request.Context(), params)

	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidState):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid_state",
			})
		case errors.Is(err, ErrAuthorizationDenied):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_authorization_denied",
			})
		case errors.Is(err, ErrInvalidCode):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid_code",
			})
		case errors.Is(err, github.ErrUnauthorized):
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "github_api_unauthorized",
			})
		case errors.Is(err, github.ErrRateLimited):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "github_rate_limited",
			})
		case errors.Is(err, github.ErrInvalidResponse):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_invalid_response",
			})
		case errors.Is(err, github.ErrAPIError):
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "github_api_error",
			})
		case errors.Is(err, ErrGitHubUnavailable) || errors.Is(err, github.ErrUnavailable):
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

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"user":   profile,
	})
}

func (h *Handler) GetMe(
	c *gin.Context,
) {
	userIDValue, ok := c.Get(string(UserIDKey))

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	userIDString, ok := userIDValue.(string)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	userID, err := uuid.Parse(userIDString)

	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	user, err := h.service.GetProfile(c.Request.Context(), userID)

	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "user_not_found",
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	c.JSON(http.StatusOK, users.ToUserResponse(user))
}

func (h *Handler) clearOAuthStateCookie(c *gin.Context) {
	c.SetCookie(
		oauthCookieName,
		"",
		-1,
		oauthCookiePath,
		"",
		oauthCookieSecure,
		true,
	)
}
