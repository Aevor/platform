package users

import (
	"github.com/google/uuid"
)

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
	return s.repository.GetByID(id)
}

func (s *Service) GetUserByGitHubID(githubID int64) (*User, error) {
	return s.repository.GetByGitHubID(githubID)
}
