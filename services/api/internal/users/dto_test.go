package users

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestToUserResponse_MapsAllFields(t *testing.T) {
	id := uuid.New()
	token := "encrypted-token"

	user := &User{
		ID:                id,
		GithubID:          583231,
		Username:          "octocat",
		DisplayName:       "The Octocat",
		Email:             "octocat@example.com",
		AvatarURL:         "https://avatars.githubusercontent.com/u/583231?v=4",
		GitHubAccessToken: &token,
	}

	got := ToUserResponse(user)

	if got.ID != id.String() {
		t.Errorf("ID = %q, want %q", got.ID, id.String())
	}

	if got.GithubID != user.GithubID {
		t.Errorf("GithubID = %d, want %d", got.GithubID, user.GithubID)
	}

	if got.Username != user.Username {
		t.Errorf("Username = %q, want %q", got.Username, user.Username)
	}

	if got.DisplayName != user.DisplayName {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, user.DisplayName)
	}

	if got.Email != user.Email {
		t.Errorf("Email = %q, want %q", got.Email, user.Email)
	}

	if got.AvatarURL != user.AvatarURL {
		t.Errorf("AvatarURL = %q, want %q", got.AvatarURL, user.AvatarURL)
	}
}

func TestToUserResponse_JSONExcludesAccessToken(t *testing.T) {
	token := "ghs_secret-token-do-not-leak"

	user := &User{
		ID:                uuid.New(),
		GithubID:          583231,
		Username:          "octocat",
		GitHubAccessToken: &token,
	}

	raw, err := json.Marshal(ToUserResponse(user))

	if err != nil {
		t.Fatalf("json.Marshal(ToUserResponse(user)) error: %v", err)
	}

	if strings.Contains(string(raw), token) {
		t.Error("UserResponse JSON contains the GitHub access token")
	}

	if strings.Contains(string(raw), "github_access_token") {
		t.Error("UserResponse JSON contains the github_access_token field")
	}
}
