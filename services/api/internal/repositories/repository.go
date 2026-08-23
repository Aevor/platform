package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// upsertConflictColumns is the conflict target for UpsertSelected: one
// ownership record per (user, github repository).
var upsertConflictColumns = []clause.Column{
	{Name: "user_id"},
	{Name: "github_repository_id"},
}

// upsertAssignmentColumns lists the columns refreshed when the same user
// re-selects a known repository. These strings must exactly match the column
// names mapped by the SelectedRepository model (see
// TestUpsertSelected_ColumnsMatchModelSchema).
var upsertAssignmentColumns = []string{
	"name",
	"full_name",
	"owner_login",
	"private",
	"default_branch",
	"html_url",
	"updated_at",
}

// issueUpsertConflictColumns is the conflict target for UpsertIssues: one
// issue row per (selected repository context, GitHub issue).
var issueUpsertConflictColumns = []clause.Column{
	{Name: "selected_repository_id"},
	{Name: "github_issue_id"},
}

// issueUpsertAssignmentColumns lists the columns refreshed when an already
// synced issue arrives again (title edits, state transitions, closed_at,
// updated_at, sync bookkeeping). These strings must exactly match the column
// names mapped by the RepositoryIssue model (see
// TestUpsertIssues_ColumnsMatchModelSchema).
var issueUpsertAssignmentColumns = []string{
	"number",
	"title",
	"state",
	"author_login",
	"html_url",
	"github_created_at",
	"github_updated_at",
	"github_closed_at",
	"synced_at",
	"updated_at",
}

// issueBatchSize bounds each INSERT statement inside a sync transaction so a
// full page of issues can never approach PostgreSQL's parameter limits.
const issueBatchSize = 250

// pullRequestUpsertConflictColumns is the conflict target for
// UpsertPullRequests: one PR row per (selected repository context, GitHub
// pull request).
var pullRequestUpsertConflictColumns = []clause.Column{
	{Name: "selected_repository_id"},
	{Name: "github_pull_request_id"},
}

// pullRequestUpsertAssignmentColumns lists the columns refreshed when an
// already synced PR arrives again (title edits, state transitions, draft and
// merge bookkeeping, branch moves, updated_at, sync bookkeeping). These
// strings must exactly match the column names mapped by the
// RepositoryPullRequest model (see
// TestUpsertPullRequests_ColumnsMatchModelSchema).
var pullRequestUpsertAssignmentColumns = []string{
	"number",
	"title",
	"state",
	"author_login",
	"html_url",
	"head_ref",
	"base_ref",
	"draft",
	"merged",
	"github_created_at",
	"github_updated_at",
	"github_closed_at",
	"github_merged_at",
	"synced_at",
	"updated_at",
}

type Store interface {
	UpsertSelected(repository *SelectedRepository) error
	ListByUserID(userID uuid.UUID) ([]SelectedRepository, error)
	DeleteByUserAndID(userID uuid.UUID, id uuid.UUID) error

	// FindByUserAndID resolves one selected-repository context owned by the
	// given user; ErrSelectedNotFound covers unknown AND foreign records.
	FindByUserAndID(userID uuid.UUID, id uuid.UUID) (*SelectedRepository, error)

	// UpsertIssues persists one bounded batch of issues for a selected
	// repository in a single transaction: existing rows are refreshed with
	// authoritative metadata, new rows are inserted — never duplicated.
	UpsertIssues(selectedRepositoryID uuid.UUID, issues []RepositoryIssue) error

	// UpsertPullRequests is the pull-request counterpart of UpsertIssues.
	UpsertPullRequests(selectedRepositoryID uuid.UUID, pullRequests []RepositoryPullRequest) error
}

type gormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) Store {
	return &gormStore{
		db: db,
	}
}

func (s *gormStore) UpsertSelected(repository *SelectedRepository) error {
	return s.db.
		Clauses(
			clause.OnConflict{
				Columns:   upsertConflictColumns,
				DoUpdates: clause.AssignmentColumns(upsertAssignmentColumns),
			},
			clause.Returning{},
		).
		Create(repository).
		Error
}

func (s *gormStore) ListByUserID(userID uuid.UUID) ([]SelectedRepository, error) {
	var selected []SelectedRepository

	err := s.db.
		Where("user_id = ?", userID).
		Order("created_at ASC").
		Find(&selected).
		Error

	return selected, err
}

func (s *gormStore) DeleteByUserAndID(userID uuid.UUID, id uuid.UUID) error {
	result := s.db.
		Where("user_id = ? AND id = ?", userID, id).
		Delete(&SelectedRepository{})

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return ErrSelectedNotFound
	}

	return nil
}

func (s *gormStore) FindByUserAndID(userID uuid.UUID, id uuid.UUID) (*SelectedRepository, error) {
	var selected SelectedRepository

	err := s.db.
		Where("user_id = ? AND id = ?", userID, id).
		First(&selected).
		Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSelectedNotFound
	}

	if err != nil {
		return nil, err
	}

	return &selected, nil
}

func (s *gormStore) UpsertIssues(selectedRepositoryID uuid.UUID, issues []RepositoryIssue) error {
	if len(issues) == 0 {
		return nil
	}

	// Bind every row to this repository context authoritatively and discard
	// any caller-supplied surrogate ID: identity comes from the unique
	// (selected_repository_id, github_issue_id) pair, so a reused row struct
	// can never collide on the primary key across syncs or contexts.
	for i := range issues {
		issues[i].SelectedRepositoryID = selectedRepositoryID
		issues[i].ID = uuid.Nil
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(
			clause.OnConflict{
				Columns:   issueUpsertConflictColumns,
				DoUpdates: clause.AssignmentColumns(issueUpsertAssignmentColumns),
			},
		).
			CreateInBatches(&issues, issueBatchSize).
			Error
	})
}

func (s *gormStore) UpsertPullRequests(selectedRepositoryID uuid.UUID, pullRequests []RepositoryPullRequest) error {
	if len(pullRequests) == 0 {
		return nil
	}

	for i := range pullRequests {
		pullRequests[i].SelectedRepositoryID = selectedRepositoryID
		pullRequests[i].ID = uuid.Nil
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(
			clause.OnConflict{
				Columns:   pullRequestUpsertConflictColumns,
				DoUpdates: clause.AssignmentColumns(pullRequestUpsertAssignmentColumns),
			},
		).
			CreateInBatches(&pullRequests, issueBatchSize).
			Error
	})
}
