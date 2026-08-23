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

const (
	integrationSHAA = "3a1f9c4b7d2e8f6051a4b3c2d1e0f9876543210a"
	integrationSHAB = "bb29c8d7e6f504132a1b0c9d8e7f6a5b4c3d2e1f"
	integrationSHAC = "cc37b6a5d4c3b2a1908172635445362718290a1b"
)

// TestRepositoryCommitStore_Integration exercises the real commit upsert
// SQL, the (selected_repository_id, github_commit_sha) unique index, and
// cross-context independence against a live PostgreSQL instance. Skipped
// unless AEVOR_TEST_DATABASE_DSN is set.
func TestRepositoryCommitStore_Integration(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa0b-0000-0000-0000-00000000000b")
	userB := uuid.MustParse("aaaaaa0c-0000-0000-0000-00000000000c")

	contextA := uuid.MustParse("bbbbbb09-0000-0000-0000-000000000009")
	contextB := uuid.MustParse("bbbbbb0a-0000-0000-0000-00000000000a")

	store := NewStore(db)

	cleanupCommits := func() {
		db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).
			Delete(&RepositoryCommit{})
		db.Where("github_commit_sha IN ?", []string{integrationSHAA, integrationSHAB, integrationSHAC}).
			Delete(&RepositoryCommit{})
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	}

	t.Cleanup(cleanupCommits)
	cleanupCommits()

	if err := db.Create(&SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	if err := db.Create(&SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}).Error; err != nil {
		t.Fatalf("could not seed selected repository B: %v", err)
	}

	authoredAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	committedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

	batch := []RepositoryCommit{
		{
			GithubCommitSha:   integrationSHAA,
			Message:           "Add pagination",
			AuthorName:        "Alice Author",
			AuthorEmail:       "alice@example.com",
			AuthorLogin:       "",
			CommitterName:     "Committer One",
			HTMLURL:           "https://github.com/octocat/hello-world/commit/" + integrationSHAA,
			GithubAuthoredAt:  authoredAt,
			GithubCommittedAt: committedAt,
			SyncedAt:          time.Now(),
		},
		{
			GithubCommitSha:   integrationSHAB,
			Message:           "Fix race",
			AuthorName:        "Bob Builder",
			AuthorEmail:       "bob@example.com",
			AuthorLogin:       "bobbuilder",
			CommitterName:     "Committer One",
			HTMLURL:           "https://github.com/octocat/hello-world/commit/" + integrationSHAB,
			GithubAuthoredAt:  authoredAt,
			GithubCommittedAt: committedAt,
			SyncedAt:          time.Now(),
		},
	}

	if err := store.UpsertCommits(contextA, batch); err != nil {
		t.Fatalf("first sync (insert path) error: %v", err)
	}

	var stored []RepositoryCommit

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_commit_sha ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 2 {
		t.Fatalf("rows = %d, want 2 after first sync", len(stored))
	}

	firstID := stored[0].ID

	time.Sleep(10 * time.Millisecond)

	// Second sync: SHA A re-arrives with GitHub-side metadata changed (the
	// author linked their email to an account), plus a brand-new SHA.
	refreshed := []RepositoryCommit{
		{
			GithubCommitSha:   integrationSHAA,
			Message:           "Add pagination",
			AuthorName:        "Alice Author",
			AuthorEmail:       "alice@example.com",
			AuthorLogin:       "newly-linked",
			CommitterName:     "Committer One",
			HTMLURL:           "https://github.com/octocat/hello-world/commit/" + integrationSHAA,
			GithubAuthoredAt:  authoredAt,
			GithubCommittedAt: committedAt,
			SyncedAt:          time.Now(),
		},
		{
			GithubCommitSha:   integrationSHAC,
			Message:           "New commit between syncs",
			AuthorName:        "Alice Author",
			AuthorEmail:       "alice@example.com",
			CommitterName:     "Committer One",
			HTMLURL:           "https://github.com/octocat/hello-world/commit/" + integrationSHAC,
			GithubAuthoredAt:  authoredAt,
			GithubCommittedAt: committedAt,
			SyncedAt:          time.Now(),
		},
	}

	if err := store.UpsertCommits(contextA, refreshed); err != nil {
		t.Fatalf("second sync (conflict path) error: %v", err)
	}

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_commit_sha ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 3 {
		t.Fatalf("rows = %d, want exactly 3 after repeated sync (no duplicates)", len(stored))
	}

	if stored[0].ID != firstID {
		t.Errorf("conflict path assigned a new UUID %s; want preserved %s", stored[0].ID, firstID)
	}

	if stored[0].GithubCommitSha != integrationSHAA || stored[0].AuthorLogin != "newly-linked" {
		t.Errorf("re-sync did not refresh the linked account: %+v", stored[0])
	}

	// The SAME SHA under ANOTHER user's repository context must be
	// independent — per-context uniqueness, not global.
	if err := store.UpsertCommits(contextB, []RepositoryCommit{batch[0]}); err != nil {
		t.Fatalf("cross-context insert error: %v", err)
	}

	var countForB int64

	db.Model(&RepositoryCommit{}).Where("selected_repository_id = ?", contextB).Count(&countForB)

	if countForB != 1 {
		t.Errorf("context B rows = %d, want an independent record for the same SHA", countForB)
	}

	var countTotal int64

	db.Model(&RepositoryCommit{}).Where("github_commit_sha = ?", integrationSHAA).Count(&countTotal)

	if countTotal != 2 {
		t.Errorf("global rows for SHA %s = %d, want one per context", integrationSHAA, countTotal)
	}
}

func TestRepositoryCommitStore_FindByUserAndID_OwnershipIsolation(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa0d-0000-0000-0000-00000000000d")
	userB := uuid.MustParse("aaaaaa0e-0000-0000-0000-00000000000e")

	contextA := uuid.MustParse("bbbbbb0b-0000-0000-0000-00000000000b")
	contextB := uuid.MustParse("bbbbbb0c-0000-0000-0000-00000000000c")

	store := NewStore(db)

	cleanupOwnership := func() {
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	}

	t.Cleanup(cleanupOwnership)
	cleanupOwnership()

	if err := db.Create(&SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 555, Name: "a-repo", FullName: "someone/a-repo", OwnerLogin: "someone"}).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	if err := db.Create(&SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 666, Name: "b-repo", FullName: "someone/b-repo", OwnerLogin: "someone"}).Error; err != nil {
		t.Fatalf("could not seed selected repository B: %v", err)
	}

	found, err := store.FindByUserAndID(userA, contextA)

	if err != nil || found.ID != contextA {
		t.Fatalf("owner lookup returned (%+v, %v), want the owned context", found, err)
	}

	if _, err := store.FindByUserAndID(userA, contextB); !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("foreign lookup error = %v, want ErrSelectedNotFound", err)
	}

	if _, err := store.FindByUserAndID(userA, uuid.New()); !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("unknown lookup error = %v, want ErrSelectedNotFound", err)
	}
}
