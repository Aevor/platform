package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const bearerPrefix = "Bearer "

func RequireAuth(manager *JWTManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))

		if !ok {
			abortUnauthorized(c)
			return
		}

		userID, err := manager.Verify(token)

		if err != nil {
			abortUnauthorized(c)
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

func bearerToken(header string) (string, bool) {
	if !strings.HasPrefix(header, bearerPrefix) {
		return "", false
	}

	token := strings.TrimPrefix(header, bearerPrefix)

	if token == "" {
		return "", false
	}

	return token, true
}

func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": "unauthorized",
	})
}
