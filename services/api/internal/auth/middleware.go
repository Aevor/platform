package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func RequireAuth(manager *JWTManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")

		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "unauthorized",
			})
			return
		}

		token := strings.TrimPrefix(header, "Bearer ")

		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "unauthorized",
			})
			return
		}

		userID, err := manager.Verify(token)

		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "unauthorized",
			})
			return
		}

		c.Set(string(UserIDKey), userID)

		c.Next()
	}
}

func GetAuthenticatedUserID(c *gin.Context) (uuid.UUID, bool) {
	value, ok := c.Get(string(UserIDKey))

	if !ok {
		return uuid.Nil, false
	}

	userID, ok := value.(uuid.UUID)

	if !ok {
		return uuid.Nil, false
	}

	return userID, true
}
