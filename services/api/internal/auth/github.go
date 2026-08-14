package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"

	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"

	"github.com/Aevor/platform/services/api/pkg/config"
)

func NewGitHubOAuthConfig(cfg *config.AppConfig) *oauth2.Config {
	endpoint := githuboauth.Endpoint
	endpoint.AuthStyle = oauth2.AuthStyleInParams

	return &oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
		Scopes: []string{
			"read:user",
		},
		Endpoint: endpoint,
	}
}

func GenerateState() (string, error) {
	b := make([]byte, 32)

	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

func VerifyState(expected, actual string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
