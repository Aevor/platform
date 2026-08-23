package repositories

import (
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

type Store interface {
	UpsertSelected(repository *SelectedRepository) error
	ListByUserID(userID uuid.UUID) ([]SelectedRepository, error)
	DeleteByUserAndID(userID uuid.UUID, id uuid.UUID) error
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
