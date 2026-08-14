package auth

import "github.com/Aevor/platform/services/api/internal/github"

type GitHubUser = github.User

type contextKey string

const UserIDKey contextKey = "user_id"
