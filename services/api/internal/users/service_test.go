package users

import (
	"strings"
	"testing"
)

func TestNewService_ReturnsNonNil(t *testing.T) {
	if NewService(NewRepository(nil)) == nil {
		t.Fatal("NewService() returned nil")
	}
}

func TestCreateUser_RejectsInvalidGitHubID(t *testing.T) {
	service := NewService(NewRepository(nil))

	for _, githubID := range []int64{0, -1, -100} {
		user, err := service.CreateUser(githubID, "octocat", "The Octocat", "octocat@example.com", "https://example.com/avatar.png")

		if err == nil {
			t.Errorf("CreateUser(githubID=%d) succeeded, want an error", githubID)
		}

		if user != nil {
			t.Errorf("CreateUser(githubID=%d) returned a user, want nil", githubID)
		}

		if !strings.Contains(err.Error(), "github_id") {
			t.Errorf("CreateUser(githubID=%d) error = %v, want a message mentioning github_id", githubID, err)
		}
	}
}

func TestCreateUser_RejectsBlankUsername(t *testing.T) {
	service := NewService(NewRepository(nil))

	for _, username := range []string{"", "   ", "\t\n"} {
		user, err := service.CreateUser(123, username, "The Octocat", "octocat@example.com", "https://example.com/avatar.png")

		if err == nil {
			t.Errorf("CreateUser(username=%q) succeeded, want an error", username)
		}

		if user != nil {
			t.Errorf("CreateUser(username=%q) returned a user, want nil", username)
		}

		if !strings.Contains(err.Error(), "username") {
			t.Errorf("CreateUser(username=%q) error = %v, want a message mentioning username", username, err)
		}
	}
}
