package repositories

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SelectedRepository is the persistent Aevor context for one GitHub repository
// chosen by one Aevor user. The same GitHub repository can be selected by
// multiple Aevor users — each gets an independent ownership/context record,
// enforced by the unique (user_id, github_repository_id) index. The GitHub
// access token is deliberately NOT part of this model: tokens belong to the
// authenticated user (users.git_hub_access_token).
type SelectedRepository struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	UserID uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_selected_repositories_user_repo;not null" json:"-"`

	GithubRepositoryID int64 `gorm:"uniqueIndex:idx_selected_repositories_user_repo;not null" json:"github_repository_id"`

	Name     string `gorm:"size:255;not null" json:"name"`
	FullName string `gorm:"size:255;not null" json:"full_name"`

	OwnerLogin string `gorm:"size:255;not null" json:"owner_login"`

	Private bool `json:"private"`

	DefaultBranch string    `json:"default_branch"`
	HTMLURL       string    `json:"html_url"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (r *SelectedRepository) TableName() string {
	return "selected_repositories"
}

func (r *SelectedRepository) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}

	return nil
}

// RepositoryIssue is the persisted Aevor metadata for one GitHub issue
// belonging to ONE selected-repository context. The same GitHub issue can be
// synced under multiple users' contexts for the same repository — uniqueness
// is deliberately scoped to (selected_repository_id, github_issue_id),
// mirroring the per-user ownership model of selected_repositories. Issue
// bodies are intentionally NOT persisted yet (documented limitation; additive
// to introduce later). GitHub timestamps are kept under a Github* prefix so
// they are never confused with the Aevor row timestamps.
type RepositoryIssue struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	SelectedRepositoryID uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_repository_issues_selected_github;not null" json:"-"`

	GithubIssueID int64 `gorm:"uniqueIndex:idx_repository_issues_selected_github;not null" json:"github_issue_id"`

	Number int    `gorm:"not null" json:"number"`
	Title  string `gorm:"type:text;not null" json:"title"`

	State string `gorm:"size:16;not null" json:"state"`

	AuthorLogin string `gorm:"size:255;not null" json:"author_login"`

	HTMLURL string `gorm:"type:text" json:"html_url"`

	GithubCreatedAt time.Time  `json:"github_created_at"`
	GithubUpdatedAt time.Time  `json:"github_updated_at"`
	GithubClosedAt  *time.Time `json:"github_closed_at"`

	SyncedAt time.Time `json:"synced_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (i *RepositoryIssue) TableName() string {
	return "repository_issues"
}

func (i *RepositoryIssue) BeforeCreate(tx *gorm.DB) error {
	if i.ID == uuid.Nil {
		i.ID = uuid.New()
	}

	if i.SyncedAt.IsZero() {
		i.SyncedAt = time.Now()
	}

	return nil
}

// RepositoryPullRequest is the persisted Aevor metadata for one GitHub pull
// request belonging to ONE selected-repository context. Uniqueness mirrors
// RepositoryIssue: scoped to (selected_repository_id, github_pull_request_id)
// so the same GitHub PR syncs independently per user context and never
// duplicates within one. Bodies are intentionally NOT persisted yet (same
// documented limitation as issues). GitHub timestamps keep the Github* prefix.
type RepositoryPullRequest struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	SelectedRepositoryID uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_repository_pull_requests_selected_github;not null" json:"-"`

	GithubPullRequestID int64 `gorm:"uniqueIndex:idx_repository_pull_requests_selected_github;not null" json:"github_pull_request_id"`

	Number int    `gorm:"not null" json:"number"`
	Title  string `gorm:"type:text;not null" json:"title"`

	State string `gorm:"size:16;not null" json:"state"`

	AuthorLogin string `gorm:"size:255;not null" json:"author_login"`

	HTMLURL string `gorm:"type:text" json:"html_url"`

	HeadRef string `gorm:"size:255" json:"head_ref"`
	BaseRef string `gorm:"size:255" json:"base_ref"`

	Draft  bool `json:"draft"`
	Merged bool `json:"merged"`

	GithubCreatedAt time.Time  `json:"github_created_at"`
	GithubUpdatedAt time.Time  `json:"github_updated_at"`
	GithubClosedAt  *time.Time `json:"github_closed_at"`
	GithubMergedAt  *time.Time `json:"github_merged_at"`

	SyncedAt time.Time `json:"synced_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p *RepositoryPullRequest) TableName() string {
	return "repository_pull_requests"
}

func (p *RepositoryPullRequest) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}

	if p.SyncedAt.IsZero() {
		p.SyncedAt = time.Now()
	}

	return nil
}
