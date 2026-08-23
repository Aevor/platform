package repositories

import (
	"gorm.io/gorm"
)

// Migrate creates/updates the selected_repositories and repository_issues
// tables via GORM AutoMigrate — the same deterministic, additive mechanism
// users.Migrate uses.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&SelectedRepository{}, &RepositoryIssue{})
}
