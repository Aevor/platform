package users

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// integrationGitHubID is a reserved high github_id that must never collide
// with a real GitHub account used in development.
const integrationGitHubID int64 = 987654321987

// TestUpsertByGitHubID_Integration exercises the real ON CONFLICT SQL against
// a live PostgreSQL instance. It is skipped unless AEVOR_TEST_DATABASE_DSN is
// set, e.g.:
//
//	AEVOR_TEST_DATABASE_DSN="host=localhost port=5432 user=aevor password=aevor dbname=aevor sslmode=disable" go test ./internal/users -run Integration -v
//
// This test exists because fake-repository unit tests cannot catch SQL-level
// column mismatches (the git_hub_access_token incident).
func TestUpsertByGitHubID_Integration(t *testing.T) {
	dsn := os.Getenv("AEVOR_TEST_DATABASE_DSN")

	if dsn == "" {
		t.Skip("AEVOR_TEST_DATABASE_DSN not set; skipping real-Postgres integration test")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})

	if err != nil {
		t.Fatalf("could not connect to the test database: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	repository := NewRepository(db)
	cleanupIntegrationUser(t, db)

	firstToken := "encrypted-token-first-login"

	first := &User{
		GithubID:          integrationGitHubID,
		Username:          "integration-user",
		DisplayName:       "Integration First Login",
		Email:             "integration@example.com",
		AvatarURL:         "https://example.com/avatar.png",
		GitHubAccessToken: &firstToken,
	}

	if err := repository.UpsertByGitHubID(first); err != nil {
		t.Fatalf("first UpsertByGitHubID (insert path) error: %v", err)
	}

	if first.ID == uuid.Nil {
		t.Fatal("insert path did not backfill the Aevor UUID via RETURNING")
	}

	storedFirst, err := repository.GetByGitHubID(integrationGitHubID)

	if err != nil {
		t.Fatalf("GetByGitHubID after insert error: %v", err)
	}

	if storedFirst.GitHubAccessToken == nil || *storedFirst.GitHubAccessToken != firstToken {
		t.Fatalf("stored token after insert = %v, want %q", storedFirst.GitHubAccessToken, firstToken)
	}

	time.Sleep(10 * time.Millisecond)

	secondToken := "encrypted-token-second-login"

	second := &User{
		GithubID:          integrationGitHubID,
		Username:          "integration-user",
		DisplayName:       "Integration Re-Login",
		Email:             "relogin@example.com",
		AvatarURL:         "https://example.com/avatar2.png",
		GitHubAccessToken: &secondToken,
	}

	if err := repository.UpsertByGitHubID(second); err != nil {
		t.Fatalf("second UpsertByGitHubID (conflict path) error: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("conflict path assigned a new UUID %s; want the preserved Aevor UUID %s", second.ID, first.ID)
	}

	storedSecond, err := repository.GetByGitHubID(integrationGitHubID)

	if err != nil {
		t.Fatalf("GetByGitHubID after re-login error: %v", err)
	}

	if storedSecond.ID != first.ID {
		t.Errorf("stored user ID = %s, want the preserved Aevor UUID %s", storedSecond.ID, first.ID)
	}

	if storedSecond.DisplayName != "Integration Re-Login" {
		t.Errorf("stored display_name = %q, want the refreshed profile value", storedSecond.DisplayName)
	}

	if storedSecond.Email != "relogin@example.com" {
		t.Errorf("stored email = %q, want the refreshed profile value", storedSecond.Email)
	}

	if storedSecond.GitHubAccessToken == nil || *storedSecond.GitHubAccessToken != secondToken {
		t.Errorf("stored token after re-login = %v, want %q (token rotation)", storedSecond.GitHubAccessToken, secondToken)
	}

	if !storedSecond.UpdatedAt.After(storedSecond.CreatedAt) && !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Log("updated_at did not advance beyond created_at; acceptable if both writes landed in the same clock tick")
	}
}

func cleanupIntegrationUser(t *testing.T, db *gorm.DB) {
	t.Helper()

	if err := db.Where("github_id = ?", integrationGitHubID).Delete(&User{}).Error; err != nil {
		t.Fatalf("could not clean up integration fixture: %v", err)
	}

	t.Cleanup(func() {
		db.Where("github_id = ?", integrationGitHubID).Delete(&User{})
	})
}
