package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

const (
	oauthCookieName   = "aevor_oauth_state"
	oauthCookiePath   = "/auth"
	oauthCookieMaxAge = 600
)

type oauthStateCookie struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
}

type Handler struct {
	service      *Service
	frontendURL  string
	cookieSecure bool
}

// NewHandler wires the OAuth HTTP surface.
//
// frontendURL identifies the browser application that completes the login
// (it is recorded for diagnostics and CORS configuration, never trusted for
// identity). cookieSecure is OPTIONAL and defaults to false for local HTTP
// development; pass true for any https deployment so the PKCE state cookie is
// never sent over plaintext.
func NewHandler(service *Service, frontendURL ...string) *Handler {
	frontend := ""
	if len(frontendURL) > 0 {
		frontend = frontendURL[0]
	}

	return &Handler{
		service:     service,
		frontendURL: frontend,
	}
}

// WithCookieSecure sets the Secure attribute on the OAuth state cookie. It
// MUST be enabled for any https deployment: the cookie carries the PKCE
// verifier and CSRF state, which must never travel over plaintext.
func (h *Handler) WithCookieSecure(secure bool) *Handler {
	h.cookieSecure = secure
	return h
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
		h.cookieSecure,
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

	user, err := h.service.HandleCallback(c.Request.Context(), params)

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

	// The Aevor session token is minted here, after GitHub authentication has
	// fully succeeded. It is the ONLY credential handed to the browser: the
	// GitHub access token never leaves the backend.
	token, err := h.service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal",
		})
		return
	}

	// Hand the session token to the browser application. The redirect target is
	// the CONFIGURED frontend, never a request-supplied URL, so this cannot be
	// turned into an open redirect that leaks the token to an attacker's site.
	location := h.frontendURL + "/auth/callback?token=" + url.QueryEscape(token)

	c.Redirect(http.StatusFound, location)
}

func (h *Handler) clearOAuthStateCookie(c *gin.Context) {
	c.SetCookie(
		oauthCookieName,
		"",
		-1,
		oauthCookiePath,
		"",
		h.cookieSecure,
		true,
	)
}

func (h *Handler) Me(c *gin.Context) {
	userID, ok := GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	user, err := h.service.users.GetUserByID(userID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}

	c.JSON(http.StatusOK, users.ToUserResponse(user))
}
