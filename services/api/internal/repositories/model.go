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
