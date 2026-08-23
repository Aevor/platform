package repositories

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestSelectedRepositoryStore_Integration exercises the real ON CONFLICT SQL
// and unique index against a live PostgreSQL instance. It is skipped unless
// AEVOR_TEST_DATABASE_DSN is set, e.g.:
//
//	AEVOR_TEST_DATABASE_DSN="host=localhost port=5432 user=aevor password=aevor dbname=aevor sslmode=disable" go test ./internal/repositories -run Integration -v
func TestSelectedRepositoryStore_Integration(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa01-0000-0000-0000-000000000001")
	userB := uuid.MustParse("aaaaaa02-0000-0000-0000-000000000002")
	githubRepoID := int64(987654321100)

	store := NewStore(db)

	t.Cleanup(func() {
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	})

	db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})

	first := &SelectedRepository{
		UserID:             userA,
		GithubRepositoryID: githubRepoID,
		Name:               "repo",
		FullName:           "someone/repo",
		OwnerLogin:         "someone",
		DefaultBranch:      "master",
		HTMLURL:            "https://github.com/someone/repo",
	}

	if err := store.UpsertSelected(first); err != nil {
		t.Fatalf("first upsert (insert path) error: %v", err)
	}

	if first.ID == uuid.Nil {
		t.Fatal("insert path did not backfill the Aevor UUID via RETURNING")
	}

	time.Sleep(10 * time.Millisecond)

	second := &SelectedRepository{
		UserID:             userA,
		GithubRepositoryID: githubRepoID,
		Name:               "repo-renamed",
		FullName:           "someone/repo-renamed",
		OwnerLogin:         "someone",
		Private:            true,
		DefaultBranch:      "main",
		HTMLURL:            "https://github.com/someone/repo-renamed",
	}

	if err := store.UpsertSelected(second); err != nil {
		t.Fatalf("second upsert (conflict path) error: %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("conflict path assigned a new UUID %s; want preserved %s", second.ID, first.ID)
	}

	listA, err := store.ListByUserID(userA)

	if err != nil {
		t.Fatalf("ListByUserID(user A) error: %v", err)
	}

	if len(listA) != 1 {
		t.Fatalf("user A selected list length = %d, want 1 (no duplicate rows)", len(listA))
	}

	stored := listA[0]

	if stored.Name != "repo-renamed" || stored.FullName != "someone/repo-renamed" ||
		!stored.Private || stored.DefaultBranch != "main" || stored.HTMLURL != "https://github.com/someone/repo-renamed" {
		t.Errorf("stored record = %+v, want refreshed authoritative metadata", stored)
	}

	sameGitHubRepoForUserB := &SelectedRepository{
		UserID:             userB,
		GithubRepositoryID: githubRepoID,
		Name:               "repo",
		FullName:           "someone/repo-renamed",
		OwnerLogin:         "someone",
	}

	if err := store.UpsertSelected(sameGitHubRepoForUserB); err != nil {
		t.Fatalf("user B upsert of the same GitHub repository error: %v", err)
	}

	listB, err := store.ListByUserID(userB)

	if err != nil {
		t.Fatalf("ListByUserID(user B) error: %v", err)
	}

	if len(listB) != 1 || listB[0].UserID != userB || listB[0].ID == first.ID {
		t.Errorf("user B records = %+v, want an independent ownership record for the same GitHub repository", listB)
	}

	if err := store.DeleteByUserAndID(userA, first.ID); err != nil {
		t.Fatalf("owner delete error: %v", err)
	}

	listAAfter, _ := store.ListByUserID(userA)

	if len(listAAfter) != 0 {
		t.Errorf("user A records after delete = %d, want 0", len(listAAfter))
	}

	if err := store.DeleteByUserAndID(userA, sameGitHubRepoForUserB.ID); !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("deleting another user's record returned %v, want ErrSelectedNotFound", err)
	}
}
