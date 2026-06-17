package users

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type User struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	GithubID    int64     `gorm:"uniqueIndex;not null"`
	Username    string    `gorm:"size:255;not null"`
	DisplayName string    `gorm:"size:255"`
	Email       string    `gorm:"size:255"`
	AvatarURL   string    `gorm:"type:text"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// BeforeCreate runs automatically before INSERT
func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}

	return nil
}
