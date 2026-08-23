package users

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository interface {
	Create(user *User) error
	GetByID(id uuid.UUID) (*User, error)
	GetByGitHubID(githubID int64) (*User, error)
	Update(user *User) error
	UpsertByGitHubID(user *User) error
}

// upsertConflictColumns is the conflict target for UpsertByGitHubID.
var upsertConflictColumns = []clause.Column{
	{Name: "github_id"},
}

// upsertAssignmentColumns lists the columns refreshed when an existing
// github_id conflicts. These strings must exactly match the DB column names
// mapped by the User model (see TestUpsertByGitHubID_ColumnsMatchModelSchema).
var upsertAssignmentColumns = []string{
	"username",
	"display_name",
	"email",
	"avatar_url",
	"git_hub_access_token",
	"updated_at",
}

type gormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{
		db: db,
	}
}

func (r *gormRepository) Create(user *User) error {
	return r.db.Create(user).Error
}

func (r *gormRepository) GetByID(id uuid.UUID) (*User, error) {
	var user User

	err := r.db.
		Where("id = ?", id).
		First(&user).
		Error

	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (r *gormRepository) GetByGitHubID(githubID int64) (*User, error) {
	var user User

	err := r.db.
		Where("github_id = ?", githubID).
		First(&user).
		Error

	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (r *gormRepository) Update(user *User) error {
	return r.db.Save(user).Error
}

func (r *gormRepository) UpsertByGitHubID(user *User) error {
	return r.db.
		Clauses(
			clause.OnConflict{
				Columns:   upsertConflictColumns,
				DoUpdates: clause.AssignmentColumns(upsertAssignmentColumns),
			},
			clause.Returning{},
		).
		Create(user).
		Error
}
