package repositories

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/discovery"
	"github.com/Aevor/platform/services/api/internal/extraction"
	"github.com/Aevor/platform/services/api/internal/filtering"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/indexing"
	"github.com/Aevor/platform/services/api/internal/representation"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// ErrSelectedNotFound is returned when a selected-repository record either
// does not exist or does not belong to the requesting user. Callers must map
// both cases to the same external error so ownership cannot be probed.
var ErrSelectedNotFound = errors.New("selected repository not found")

// ErrWorkspaceNotReady is returned when a selected repository has no usable
// local workspace yet (never cloned, clone failed earlier, or corrupted).
// The remedy for the legitimate owner is POST /repositories/:id/clone.
var ErrWorkspaceNotReady = errors.New("workspace not ready")

type Service struct {
	users         *users.Service
	github        *github.Client
	store         Store
	encryptionKey []byte

	// Workspace access for repository cloning (Task 3a). workspaces is nil
	// only in legacy constructions that predate cloning; CloneRepository
	// fails closed in that case.
	workspaces        *workspace.Manager
	cloner            workspace.Cloner
	cloneURLValidator func(string) error
	cloneTimeout      time.Duration

	// Read-only codebase discovery over prepared workspaces (Task 3b).
	discoverer *discovery.Service

	// Deterministic file filtering over prepared workspaces (Task 3c).
	filterer *filtering.Service

	// Bounded content extraction over filtered selections (Task 3d).
	extractor *extraction.Service

	// Deterministic chunking over extracted content (Task 3e).
	chunker *chunking.Service

	// Traceable code representation over chunked content (Task 3f).
	representer *representation.Service

	// Bounded, metadata-only lookup over represented chunks (Task 3g).
	index *indexing.Index

	// External AI analysis service client (Task 3h). nil when AI analysis
	// is not configured; AnalyzeRepositoryContent fails closed in that case.
	aiClient *ai.Client
}

func NewService(
	userService *users.Service,
	githubClient *github.Client,
	store Store,
	encryptionKey []byte,
	workspaces *workspace.Manager,
	cloner workspace.Cloner,
	discoverer *discovery.Service,
	filterer *filtering.Service,
	extractor *extraction.Service,
	chunker *chunking.Service,
	representer *representation.Service,
	index *indexing.Index,
	aiClient *ai.Client,
) *Service {
	return &Service{
		users:             userService,
		github:            githubClient,
		store:             store,
		encryptionKey:     encryptionKey,
		workspaces:        workspaces,
		cloner:            cloner,
		cloneURLValidator: workspace.MakeCloneURLValidator(workspace.DefaultAllowedHosts, false),
		cloneTimeout:      workspace.DefaultCloneTimeout,
		discoverer:        discoverer,
		filterer:          filterer,
		extractor:         extractor,
		chunker:           chunker,
		representer:       representer,
		index:             index,
		aiClient:          aiClient,
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

// dedupByKey removes items whose key already appeared EARLIER in the
// collected list, keeping the first occurrence. Bounded pagination over a
// GitHub listing is not snapshot-stable: an item changed between page
// fetches can shift across the window and be served on two pages. Persisting
// such a batch as one multi-row INSERT ... ON CONFLICT would fail with
// "cannot affect row a second time", so duplicates are collapsed up front.
func dedupByKey[T any, K comparable](items []T, key func(T) K) []T {
	seen := make(map[K]struct{}, len(items))
	deduped := make([]T, 0, len(items))

	for _, item := range items {
		k := key(item)

		if _, duplicate := seen[k]; duplicate {
			continue
		}

		seen[k] = struct{}{}
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

	for _, issue := range dedupByKey(collected, func(issue github.Issue) int64 {
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

	for _, pullRequest := range dedupByKey(collected, func(pullRequest github.PullRequest) int64 {
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

// SyncCommits synchronizes Git commit metadata for ONE of the authenticated
// user's selected repositories, with the same ownership, credential, and
// bounded-pagination discipline as the issue and pull-request syncs. The SHA
// is immutable, so repeated syncs never duplicate; refreshed metadata covers
// GitHub-side changes such as a linked account appearing for an author.
func (s *Service) SyncCommits(
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
		func(ctx context.Context, page int) ([]github.Commit, bool, error) {
			return s.github.ListRepositoryCommits(
				ctx,
				token,
				selected.OwnerLogin,
				selected.Name,
				page,
				syncPerPage,
			)
		})

	if err != nil {
		log.Printf("github commit list failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	rows := make([]RepositoryCommit, 0, len(collected))

	now := time.Now()

	for _, commit := range dedupByKey(collected, func(commit github.Commit) string {
		return commit.SHA
	}) {
		var authorLogin string

		if commit.Author != nil {
			authorLogin = strings.TrimSpace(commit.Author.Login)
		}

		rows = append(rows, RepositoryCommit{
			SelectedRepositoryID: selected.ID,
			GithubCommitSha:      commit.SHA,
			Message:              commit.Commit.Message,
			AuthorName:           commit.Commit.Author.Name,
			AuthorEmail:          commit.Commit.Author.Email,
			AuthorLogin:          authorLogin,
			CommitterName:        commit.Commit.Committer.Name,
			HTMLURL:              commit.HTMLURL,
			GithubAuthoredAt:     commit.Commit.Author.Date,
			GithubCommittedAt:    commit.Commit.Committer.Date,
			SyncedAt:             now,
		})
	}

	if err := s.store.UpsertCommits(selected.ID, rows); err != nil {
		log.Printf("commit persistence failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	return &SyncResult{
		RepositoryID: selected.ID.String(),
		Synced:       len(rows),
	}, nil
}

// CloneResult reports the outcome of a workspace operation. It contains only
// Aevor identifiers and a status word — never filesystem paths, clone URLs,
// or token material.
type CloneResult struct {
	RepositoryID string `json:"repository_id"`
	Status       string `json:"status"`
}

const (
	cloneStatusReady = "ready"
)

// CloneRepository ensures the authenticated user's selected repository exists
// as a private local Git workspace under the server-controlled root.
//
// Authorization chain: JWT-derived userID -> owned SelectedRepository ->
// user's decrypted GitHub token -> AUTHORITATIVE GitHub repository fetch
// (validates credentials and refreshes the clone target before any
// filesystem work) -> validated https clone URL -> isolated shallow clone.
//
// Idempotency: an existing, verified workspace is left untouched and reported
// ready; a partial or corrupted directory from an earlier failure is wiped
// before retrying; any failure discards the partial workspace. The plaintext
// token exists ONLY in memory for the duration of the operation — it is never
// embedded in the stored clone URL (the clean URL is what lands in
// .git/config), never logged, and never returned.
func (s *Service) CloneRepository(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*CloneResult, error) {
	if s.workspaces == nil || s.cloner == nil {
		return nil, fmt.Errorf("workspace subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, err
	}

	authoritative, err := s.github.GetRepository(ctx, token, selected.GithubRepositoryID)

	if err != nil {
		log.Printf("github repository fetch failed for user %s repository %s: %v", userID, selected.ID, err)
		return nil, err
	}

	if err := s.cloneURLValidator(authoritative.CloneURL); err != nil {
		log.Printf("clone url rejected for repository %s: policy violation", selected.ID)
		return nil, workspace.ErrInvalidCloneURL
	}

	unlock := s.workspaces.LockFor(selected.ID)
	defer unlock()

	ready, err := s.workspaces.Ready(selected.ID)

	if err != nil {
		log.Printf("workspace inspection failed for repository %s: %v", selected.ID, err)
		return nil, workspace.ErrCloneFailed
	}

	if ready {
		// Idempotent: never destroy or blindly re-clone an existing,
		// verified workspace.
		return &CloneResult{RepositoryID: selected.ID.String(), Status: cloneStatusReady}, nil
	}

	dir, err := s.workspaces.Reset(selected.ID)

	if err != nil {
		log.Printf("workspace preparation failed for repository %s: %v", selected.ID, err)
		return nil, workspace.ErrCloneFailed
	}

	started := time.Now()

	cloneCtx, cancel := context.WithTimeout(ctx, s.cloneTimeout)
	defer cancel()

	err = s.cloner.Clone(cloneCtx, authoritative.CloneURL, authoritative.DefaultBranch, token, dir)
	err = workspace.VerifyTimeout(started, s.cloneTimeout, err)

	if err != nil {
		// Never leave corrupted partial repositories behind. The underlying
		// cause stays in the SERVER log only.
		s.workspaces.Discard(selected.ID)
		log.Printf("clone failed for user %s repository %s after %s: %v",
			userID, selected.ID, time.Since(started).Round(time.Millisecond), err)
		return nil, err
	}

	if verified, err := s.workspaces.Ready(selected.ID); !verified || err != nil {
		s.workspaces.Discard(selected.ID)
		log.Printf("clone verification failed for user %s repository %s", userID, selected.ID)
		return nil, workspace.ErrCloneFailed
	}

	log.Printf("clone succeeded for user %s repository %s in %s", userID, selected.ID, time.Since(started).Round(time.Millisecond))

	return &CloneResult{
		RepositoryID: selected.ID.String(),
		Status:       cloneStatusReady,
	}, nil
}

// ConfigureCloneURLPolicy replaces the clone-URL validator from application
// configuration (production default: https to github.com only; file:// is an
// explicit local-development opt-in). It exists so cmd/server can translate
// configuration into policy without exposing mutable internals.
func (s *Service) ConfigureCloneURLPolicy(allowedHosts []string, allowFileTransport bool) {
	s.cloneURLValidator = workspace.MakeCloneURLValidator(allowedHosts, allowFileTransport)
}

// DiscoverRepository performs READ-ONLY codebase discovery over the
// authenticated user's prepared workspace (Task 3b).
//
// Authorization chain mirrors CloneRepository: JWT-derived userID ->
// owned SelectedRepository -> workspace readiness -> metadata-only walk.
// Nothing is executed, no file contents are read, symlinks are never
// followed, and only aggregate metadata + RELATIVE paths are returned.
func (s *Service) DiscoverRepository(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*discovery.Summary, error) {
	if s.workspaces == nil || s.discoverer == nil {
		return nil, fmt.Errorf("workspace subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	ready, err := s.workspaces.Ready(selected.ID)

	if err != nil {
		log.Printf("workspace inspection failed for repository %s: %v", selected.ID, err)
		return nil, ErrWorkspaceNotReady
	}

	if !ready {
		return nil, ErrWorkspaceNotReady
	}

	started := time.Now()

	summary, err := s.discoverer.Discover(ctx, s.workspaces.Dir(selected.ID))

	if err != nil {
		log.Printf("discovery failed for user %s repository %s after %s: %v",
			userID, selected.ID, time.Since(started).Round(time.Millisecond), err)
		return nil, err
	}

	log.Printf("discovery succeeded for user %s repository %s in %s (%d files)",
		userID, selected.ID, time.Since(started).Round(time.Millisecond), summary.Files)

	return summary, nil
}

// FilterRepository deterministically selects the files from the authenticated
// user's prepared workspace that are suitable for future codebase analysis
// (Task 3c). The flow mirrors DiscoverRepository: ownership FIRST, workspace
// readiness gate, then a read-only metadata-only pass. Nothing is executed,
// no file contents are read, symlinks are never followed, and only aggregate
// counts plus RELATIVE-path decisions are returned. A bounded default timeout
// keeps the synchronous endpoint from hanging on adversarial trees.
func (s *Service) FilterRepository(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*filtering.Result, error) {
	if s.workspaces == nil || s.filterer == nil {
		return nil, fmt.Errorf("workspace subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	ready, err := s.workspaces.Ready(selected.ID)

	if err != nil {
		log.Printf("workspace inspection failed for repository %s: %v", selected.ID, err)
		return nil, ErrWorkspaceNotReady
	}

	if !ready {
		return nil, ErrWorkspaceNotReady
	}

	started := time.Now()

	filterCtx, cancel := context.WithTimeout(ctx, filtering.DefaultTimeout)
	defer cancel()

	result, err := s.filterer.Filter(filterCtx, s.workspaces.Dir(selected.ID))

	if err != nil {
		log.Printf("filtering failed for user %s repository %s after %s: %v",
			userID, selected.ID, time.Since(started).Round(time.Millisecond), err)
		return nil, err
	}

	log.Printf("filtering succeeded for user %s repository %s in %s (%d included of %d candidates)",
		userID, selected.ID, time.Since(started).Round(time.Millisecond),
		result.IncludedFiles, result.TotalFiles)

	return result, nil
}

// ExtractRepositoryContent turns the authenticated user's filtered selection
// into the internal content representation (Task 3d). The flow mirrors
// FilterRepository: ownership FIRST, workspace readiness gate, then a bounded
// read-only pass over exactly the files filtering selected. File contents are
// returned only in-process for future pipeline stages — they are never logged
// and never serialized through HTTP by the handler.
func (s *Service) ExtractRepositoryContent(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*extraction.Result, error) {
	if s.workspaces == nil || s.extractor == nil {
		return nil, fmt.Errorf("workspace subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	ready, err := s.workspaces.Ready(selected.ID)

	if err != nil {
		log.Printf("workspace inspection failed for repository %s: %v", selected.ID, err)
		return nil, ErrWorkspaceNotReady
	}

	if !ready {
		return nil, ErrWorkspaceNotReady
	}

	started := time.Now()

	extractCtx, cancel := context.WithTimeout(ctx, extraction.DefaultTimeout)
	defer cancel()

	result, err := s.extractor.Extract(extractCtx, s.workspaces.Dir(selected.ID))

	if err != nil {
		log.Printf("extraction failed for user %s repository %s after %s: %v",
			userID, selected.ID, time.Since(started).Round(time.Millisecond), err)
		return nil, err
	}

	// Counts and bytes only — never paths and never contents.
	log.Printf("extraction succeeded for user %s repository %s in %s (%d extracted of %d candidates, %d bytes)",
		userID, selected.ID, time.Since(started).Round(time.Millisecond),
		result.ExtractedCount, result.TotalCandidates, result.ExtractedBytes)

	return result, nil
}

// ChunkRepositoryContent segments the authenticated user's extracted content
// into bounded, deterministic chunks (Task 3e). It REUSES
// ExtractRepositoryContent wholesale — ownership, workspace readiness, and
// timeout enforcement are identical by construction. Chunking itself is a
// pure transformation with no filesystem access; the repository identity is
// attached here because the chunker is deliberately repository-agnostic.
// Chunk CONTENTS stay in-process for future pipeline stages — they are never
// logged and never serialized through HTTP.
func (s *Service) ChunkRepositoryContent(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*chunking.Result, error) {
	_, chunkResult, err := s.extractAndChunk(ctx, userID, selectedRepositoryID)

	return chunkResult, err
}

// RepresentRepositoryContent produces the Task 3f representation for the
// authenticated user's selected repository: extraction → chunking →
// representation, reusing every bounded, ownership-checked stage.
func (s *Service) RepresentRepositoryContent(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*representation.Result, error) {
	if s.representer == nil {
		return nil, fmt.Errorf("representation subsystem is not configured")
	}

	extractionResult, chunkResult, err := s.extractAndChunk(ctx, userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	started := time.Now()

	result := s.representer.Represent(chunkResult, extractionResult)

	// Counts only — never paths and never contents.
	log.Printf("representation succeeded for user %s repository %s in %s (%d chunks, %d bytes)",
		userID, selectedRepositoryID, time.Since(started).Round(time.Millisecond),
		result.TotalChunks, result.TotalBytes)

	return result, nil
}

// IndexResult summarizes an authorized metadata-index replacement. It never
// contains source content; callers needing individual metadata records must
// use LookupIndexedContent after ownership has been verified again.
type IndexResult struct {
	RepositoryID string
	Files        int
	Chunks       int
}

// IndexRepositoryContent rebuilds the metadata-only index for one owned
// selected repository. The existing Task 3f representation flow is reused
// wholesale, so workspace readiness, ownership, extraction limits, chunking,
// and traceability all remain unchanged. Replace atomically handles unchanged,
// changed, deleted, and new files within the next repository snapshot.
func (s *Service) IndexRepositoryContent(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*IndexResult, error) {
	if s.index == nil {
		return nil, fmt.Errorf("indexing subsystem is not configured")
	}

	// Resolve ownership before the representation flow and use only OUR stored
	// selected-repository ID as the index identity. The client cannot choose a
	// repository ID for a different user's data.
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	represented, err := s.RepresentRepositoryContent(ctx, userID, selected.ID)
	if err != nil {
		return nil, err
	}

	if err := s.index.Replace(selected.ID.String(), represented.Chunks); err != nil {
		return nil, err
	}

	return &IndexResult{
		RepositoryID: selected.ID.String(),
		Files:        represented.TotalFiles,
		Chunks:       represented.TotalChunks,
	}, nil
}

// IndexedFiles returns repository-relative indexed file paths only after the
// caller's ownership of the selected repository has been established. Missing
// indexes are represented as an empty result; no repository existence outside
// the authenticated user's scope is observable.
func (s *Service) IndexedFiles(
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) ([]string, error) {
	if s.index == nil {
		return nil, fmt.Errorf("indexing subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	return s.index.Files(selected.ID.String()), nil
}

// LookupIndexedContent returns metadata-only records for one owned selected
// repository. query.RepositoryID is deliberately overwritten from the
// ownership-checked record: client or future internal callers cannot use this
// method to select another repository's index.
func (s *Service) LookupIndexedContent(
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	query indexing.Query,
) ([]indexing.Record, error) {
	if s.index == nil {
		return nil, fmt.Errorf("indexing subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	query.RepositoryID = selected.ID.String()

	return s.index.Lookup(query), nil
}

// AnalyzeResult wraps the structured response from the external AI analysis
// service. It never contains raw AI output; the response is parsed and
// validated by the ai.Client before reaching business logic.
type AnalyzeResult struct {
	Response *ai.AnalyzeResponse
}

// AnalyzeRepositoryContent sends bounded repository context to the external
// AI analysis service and returns structured insights. The flow:
//
//  1. Verify ownership via JWT-derived userID (same as every other method)
//  2. Fail closed if aiClient is nil (AI not configured)
//  3. Look up indexed metadata via index.Lookup (no filesystem access)
//  4. Assemble bounded ContextChunks from index records (metadata only)
//  5. Build AnalyzeRequest with repository identity + query + chunks
//  6. Call aiClient.Analyze (HTTP to separate AI service)
//  7. Return wrapped response
//
// Security: the request carries ONLY repository identity and bounded metadata
// chunks. NEVER GitHub tokens, JWT secrets, encryption keys, credentials,
// or environment variables. Raw source code is NOT included — only indexed
// metadata (file paths, languages, symbols, line ranges).
func (s *Service) AnalyzeRepositoryContent(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	query string,
) (*AnalyzeResult, error) {
	if s.aiClient == nil {
		return nil, fmt.Errorf("ai analysis subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	records := s.index.Lookup(indexing.Query{
		RepositoryID: selected.ID.String(),
	})

	chunks := make([]ai.ContextChunk, 0, len(records))

	for i := range records {
		record := &records[i]

		chunk := ai.ContextChunk{
			ID:         record.ID,
			FilePath:   record.FilePath,
			Language:   record.Language,
			FileRole:   record.FileRole,
			ChunkIndex: record.ChunkIndex,
			StartLine:  record.StartLine,
			EndLine:    record.EndLine,
			SymbolType: record.SymbolType,
		}

		if record.SymbolName != nil {
			chunk.SymbolName = record.SymbolName
		}

		if record.ParentSymbol != nil {
			chunk.ParentSymbol = record.ParentSymbol
		}

		chunks = append(chunks, chunk)
	}

	request := &ai.AnalyzeRequest{
		RepositoryID:   selected.ID.String(),
		RepositoryName: selected.Name,
		Query:          query,
		ContextChunks:  chunks,
	}

	response, err := s.aiClient.Analyze(ctx, request)
	if err != nil {
		log.Printf("ai analysis failed for user %s repository %s: %v",
			userID, selected.ID, err)
		return nil, err
	}

	return &AnalyzeResult{Response: response}, nil
}

// extractAndChunk runs the shared bounded pipeline stages (Task 3d + 3e),
// attaching repository identity to chunks. All security properties are those
// of the upstream stages; this helper adds none and removes none.
func (s *Service) extractAndChunk(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*extraction.Result, *chunking.Result, error) {
	if s.chunker == nil {
		return nil, nil, fmt.Errorf("chunking subsystem is not configured")
	}

	extractionResult, err := s.ExtractRepositoryContent(ctx, userID, selectedRepositoryID)

	if err != nil {
		return nil, nil, err
	}

	started := time.Now()

	chunkResult := s.chunker.Chunk(extractionResult)

	for index := range chunkResult.Chunks {
		chunkResult.Chunks[index].RepositoryID = selectedRepositoryID.String()
	}

	// Counts only — never paths and never contents.
	log.Printf("chunking succeeded for user %s repository %s in %s (%d chunks, %d bytes)",
		userID, selectedRepositoryID, time.Since(started).Round(time.Millisecond),
		chunkResult.TotalChunks, chunkResult.TotalBytes)

	return extractionResult, chunkResult, nil
}
