package users

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type User struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	GithubID int64 `gorm:"uniqueIndex;not null" json:"github_id"`

	Username string `gorm:"size:255;not null" json:"username"`

	DisplayName string `gorm:"size:255" json:"display_name"`

	Email string `gorm:"size:255" json:"email"`

	AvatarURL string `gorm:"type:text" json:"avatar_url"`

	// Column is pinned explicitly: GORM's default naming maps
	// GitHubAccessToken to git_hub_access_token (not github_access_token).
	GitHubAccessToken *string `gorm:"column:git_hub_access_token;type:text" json:"-"`

	CreatedAt time.Time `json:"created_at"`

	UpdatedAt time.Time `json:"updated_at"`
}

func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}

	return nil
}
