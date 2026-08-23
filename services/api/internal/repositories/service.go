package repositories

import (
	"context"
	"errors"
	"log"
	"time"

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

const (
	// syncPerPage is GitHub's maximum page size: fewer requests per sync.
	syncPerPage = 100

	// syncMaxPages bounds a single synchronous synchronization to a
	// deterministic amount of work (currently <= 1000 issues). Repositories
	// with more history simply sync their most recently updated issues first;
	// deeper backfill can revisit later without any duplicate risk.
	syncMaxPages = 10
)

// SyncResult summarizes one synchronization run. It deliberately contains no
// GitHub payload material — only Aevor identifiers and counts.
type SyncResult struct {
	RepositoryID string `json:"repository_id"`
	Synced       int    `json:"synced"`
}

// collectBoundedPages walks a GitHub list endpoint deterministically: page 1
// through syncMaxPages, following Link-header pagination and stopping at the
// first page without a successor. Any fetch error aborts with NOTHING
// collected (callers persist all-or-nothing).
func collectBoundedPages[T any](
	ctx context.Context,
	fetch func(ctx context.Context, page int) ([]T, bool, error),
) ([]T, error) {
	collected := make([]T, 0)

	for page := 1; page <= syncMaxPages; page++ {
		items, hasMore, err := fetch(ctx, page)

		if err != nil {
			return nil, err
		}

		collected = append(collected, items...)

		if !hasMore {
			break
		}
	}

	return collected, nil
}

// dedupByGitHubID removes items whose GitHub ID already appeared EARLIER in
// the collected list, keeping the first occurrence. Bounded pagination over a
// `sort=updated` listing is not snapshot-stable: an item updated between page
// fetches can shift across the window and be served on two pages. Persisting
// such a batch as one multi-row INSERT ... ON CONFLICT would fail with
// "cannot affect row a second time", so duplicates are collapsed up front.
func dedupByGitHubID[T any](items []T, githubID func(T) int64) []T {
	seen := make(map[int64]struct{}, len(items))
	deduped := make([]T, 0, len(items))

	for _, item := range items {
		id := githubID(item)

		if _, duplicate := seen[id]; duplicate {
			continue
		}

		seen[id] = struct{}{}
		deduped = append(deduped, item)
	}

	return deduped
}

// SyncIssues synchronizes issue metadata for ONE of the authenticated user's
// selected repositories. Ownership is resolved through the JWT-derived userID:
// unknown and foreign selected-repository IDs are indistinguishable
// (ErrSelectedNotFound). The owner/name pair used against GitHub comes from
// OUR stored authoritative metadata (captured at selection time with the
// user's own token) — a client can never redirect the sync at another
// repository.
func (s *Service) SyncIssues(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*SyncResult, error) {
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, err
	}

	collected, err := collectBoundedPages(ctx,
		func(ctx context.Context, page int) ([]github.Issue, bool, error) {
			return s.github.ListRepositoryIssues(
				ctx,
				token,
				selected.OwnerLogin,
				selected.Name,
				page,
				syncPerPage,
			)
		})

	if err != nil {
		log.Printf("github issue list failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	rows := make([]RepositoryIssue, 0, len(collected))

	now := time.Now()

	for _, issue := range dedupByGitHubID(collected, func(issue github.Issue) int64 {
		return issue.ID
	}) {
		rows = append(rows, RepositoryIssue{
			SelectedRepositoryID: selected.ID,
			GithubIssueID:        issue.ID,
			Number:               issue.Number,
			Title:                issue.Title,
			State:                issue.State,
			AuthorLogin:          issue.User.Login,
			HTMLURL:              issue.HTMLURL,
			GithubCreatedAt:      issue.CreatedAt,
			GithubUpdatedAt:      issue.UpdatedAt,
			GithubClosedAt:       issue.ClosedAt,
			SyncedAt:             now,
		})
	}

	if err := s.store.UpsertIssues(selected.ID, rows); err != nil {
		log.Printf("issue persistence failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	return &SyncResult{
		RepositoryID: selected.ID.String(),
		Synced:       len(rows),
	}, nil
}

// SyncPullRequests synchronizes pull-request metadata for ONE of the
// authenticated user's selected repositories, with the same ownership,
// credential, and bounded-pagination discipline as SyncIssues. The head/base
// branch names and draft/merge state are refreshed on every sync.
func (s *Service) SyncPullRequests(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*SyncResult, error) {
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, err
	}

	collected, err := collectBoundedPages(ctx,
		func(ctx context.Context, page int) ([]github.PullRequest, bool, error) {
			return s.github.ListRepositoryPullRequests(
				ctx,
				token,
				selected.OwnerLogin,
				selected.Name,
				page,
				syncPerPage,
			)
		})

	if err != nil {
		log.Printf("github pull request list failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	rows := make([]RepositoryPullRequest, 0, len(collected))

	now := time.Now()

	for _, pullRequest := range dedupByGitHubID(collected, func(pullRequest github.PullRequest) int64 {
		return pullRequest.ID
	}) {
		rows = append(rows, RepositoryPullRequest{
			SelectedRepositoryID: selected.ID,
			GithubPullRequestID:  pullRequest.ID,
			Number:               pullRequest.Number,
			Title:                pullRequest.Title,
			State:                pullRequest.State,
			AuthorLogin:          pullRequest.User.Login,
			HTMLURL:              pullRequest.HTMLURL,
			HeadRef:              pullRequest.Head.Ref,
			BaseRef:              pullRequest.Base.Ref,
			Draft:                pullRequest.Draft,
			Merged:               pullRequest.Merged,
			GithubCreatedAt:      pullRequest.CreatedAt,
			GithubUpdatedAt:      pullRequest.UpdatedAt,
			GithubClosedAt:       pullRequest.ClosedAt,
			GithubMergedAt:       pullRequest.MergedAt,
			SyncedAt:             now,
		})
	}

	if err := s.store.UpsertPullRequests(selected.ID, rows); err != nil {
		log.Printf("pull request persistence failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	return &SyncResult{
		RepositoryID: selected.ID.String(),
		Synced:       len(rows),
	}, nil
}
