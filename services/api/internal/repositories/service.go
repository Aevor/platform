package repositories

import (
	"context"
	"log"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

type Service struct {
	users         *users.Service
	github        *github.Client
	encryptionKey []byte
}

func NewService(
	userService *users.Service,
	githubClient *github.Client,
	encryptionKey []byte,
) *Service {
	return &Service{
		users:         userService,
		github:        githubClient,
		encryptionKey: encryptionKey,
	}
}

// ListForUser retrieves one page of GitHub repositories accessible to the
// authenticated Aevor user. The identity (userID) always comes from the
// verified JWT — never from client input — so a user can only ever use their
// own stored credentials.
func (s *Service) ListForUser(
	ctx context.Context,
	userID uuid.UUID,
	page int,
	perPage int,
) ([]github.Repository, bool, error) {
	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, false, err
	}

	repositories, hasMore, err := s.github.ListUserRepositories(ctx, token, page, perPage)

	if err != nil {
		// Typed GitHub errors carry status context only, never token material.
		log.Printf("github repository list failed for user %s: %v", userID, err)
		return nil, false, err
	}

	return repositories, hasMore, nil
}
