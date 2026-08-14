package users

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/github"
)

var ErrNotFound = errors.New("user not found")

type Service struct {
	repository *Repository
}

func NewService(repository *Repository) *Service {
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

func (s *Service) FindOrCreateByGitHubID(profile github.User, encryptedToken string) (*User, error) {
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
