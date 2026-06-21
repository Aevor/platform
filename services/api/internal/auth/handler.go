package auth

import "github.com/gin-gonic/gin"

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
	loginURL := h.service.GetGitHubLoginURL()

	c.Redirect(
		302,
		loginURL,
	)
}
