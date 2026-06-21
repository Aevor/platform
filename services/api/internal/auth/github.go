package auth

import (
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

func NewGitHubOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),

		RedirectURL: "http://localhost:8080/auth/github/callback",

		Scopes: []string{
			"read:user",
			"user:email",
		},

		Endpoint: github.Endpoint,
	}
}
