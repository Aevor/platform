package users

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{
		db: db,
	}
}

func (r *Repository) Create(user *User) error {
	return r.db.Create(user).Error
}

func (r *Repository) GetByID(id uuid.UUID) (*User, error) {
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

func (r *Repository) GetByGitHubID(githubID int64) (*User, error) {
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

func (r *Repository) Update(user *User) error {
	return r.db.Save(user).Error
}

func (r *Repository) UpsertByGitHubID(user *User) error {
	return r.db.
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "github_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"username",
				"display_name",
				"email",
				"avatar_url",
				"github_access_token",
				"updated_at",
			}),
		}).
		Create(user).
		Error
}
