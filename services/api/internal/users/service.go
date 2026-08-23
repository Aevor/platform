package users

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/github"
)

var (
	ErrNotFound           = errors.New("user not found")
	ErrInvalidProfile     = errors.New("invalid github profile")
	ErrGitHubTokenMissing = errors.New("github access token missing")
)

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{
		repository: repository,
	}
}

func (s *Service) CreateUser(
	githubID int64,
	username string,
	displayName string,
	email string,
	avatarURL string,
) (*User, error) {

	if githubID <= 0 {
		return nil, errors.New("github_id is required")
	}

	if strings.TrimSpace(username) == "" {
		return nil, errors.New("username is required")
	}

	user := &User{
		GithubID:    githubID,
		Username:    username,
		DisplayName: displayName,
		Email:       email,
		AvatarURL:   avatarURL,
	}

	err := s.repository.Create(user)

	if err != nil {
		return nil, err
	}

	return user, nil
}

func (s *Service) GetUserByID(id uuid.UUID) (*User, error) {
	user, err := s.repository.GetByID(id)

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}

		return nil, err
	}

	return user, nil
}

func (s *Service) GetUserByGitHubID(githubID int64) (*User, error) {
	return s.repository.GetByGitHubID(githubID)
}

// DecryptedGitHubToken returns the plaintext GitHub access token for the user,
// decrypted with the provided 32-byte key. The token is only ever returned to
// in-process callers (GitHub API requests); it must never be logged or
// serialized into a response.
func (s *Service) DecryptedGitHubToken(id uuid.UUID, encryptionKey []byte) (string, error) {
	user, err := s.GetUserByID(id)

	if err != nil {
		return "", err
	}

	if user.GitHubAccessToken == nil || strings.TrimSpace(*user.GitHubAccessToken) == "" {
		return "", ErrGitHubTokenMissing
	}

	return Decrypt(*user.GitHubAccessToken, encryptionKey)
}

func (s *Service) FindOrCreateByGitHubID(profile github.User, encryptedToken string) (*User, error) {
	if profile.ID <= 0 {
		return nil, fmt.Errorf("%w: github_id must be positive", ErrInvalidProfile)
	}

	if strings.TrimSpace(profile.Login) == "" {
		return nil, fmt.Errorf("%w: username is required", ErrInvalidProfile)
	}

	user := &User{
		GithubID:          profile.ID,
		Username:          profile.Login,
		DisplayName:       profile.Name,
		Email:             profile.Email,
		AvatarURL:         profile.AvatarURL,
		GitHubAccessToken: &encryptedToken,
	}

	if err := s.repository.UpsertByGitHubID(user); err != nil {
		return nil, err
	}

	return user, nil
}
