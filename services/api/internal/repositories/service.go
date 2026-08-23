package repositories

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

// ErrSelectedNotFound is returned when a selected-repository record either
// does not exist or does not belong to the requesting user. Callers must map
// both cases to the same external error so ownership cannot be probed.
var ErrSelectedNotFound = errors.New("selected repository not found")

type Service struct {
	users         *users.Service
	github        *github.Client
	store         Store
	encryptionKey []byte
}

func NewService(
	userService *users.Service,
	githubClient *github.Client,
	store Store,
	encryptionKey []byte,
) *Service {
	return &Service{
		users:         userService,
		github:        githubClient,
		store:         store,
		encryptionKey: encryptionKey,
	}
}

// ListForUser retrieves one page of GitHub repositories accessible to the
// authenticated Aevor user (discovery). The identity (userID) always comes
// from the verified JWT — never from client input.
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

// SelectForUser verifies — through the authenticated user's OWN GitHub token —
// that the repository is accessible to their account, then persists the
// per-user repository context. Client-supplied metadata is never trusted:
// name/owner/visibility all come from the authoritative GitHub response.
func (s *Service) SelectForUser(
	ctx context.Context,
	userID uuid.UUID,
	githubRepositoryID int64,
) (*SelectedRepository, error) {
	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, err
	}

	repository, err := s.github.GetRepository(ctx, token, githubRepositoryID)

	if err != nil {
		log.Printf("github repository fetch failed for user %s: %v", userID, err)
		return nil, err
	}

	selected := &SelectedRepository{
		UserID:             userID,
		GithubRepositoryID: repository.ID,
		Name:               repository.Name,
		FullName:           repository.FullName,
		OwnerLogin:         repository.Owner.Login,
		Private:            repository.Private,
		DefaultBranch:      repository.DefaultBranch,
		HTMLURL:            repository.HTMLURL,
	}

	if err := s.store.UpsertSelected(selected); err != nil {
		log.Printf("selected repository persistence failed for user %s: %v", userID, err)
		return nil, err
	}

	return selected, nil
}

// ListSelected returns ONLY the authenticated user's stored repository
// contexts. There is no way to query another user's selections.
func (s *Service) ListSelected(userID uuid.UUID) ([]SelectedRepository, error) {
	return s.store.ListByUserID(userID)
}

// RemoveSelected deletes one of the authenticated user's own records; unknown
// IDs and other users' IDs are indistinguishable by design.
func (s *Service) RemoveSelected(userID uuid.UUID, id uuid.UUID) error {
	return s.store.DeleteByUserAndID(userID, id)
}
