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

// TestRepositoryPullRequestStore_Integration exercises the real PR upsert
// SQL, the (selected_repository_id, github_pull_request_id) unique index, and
// ownership isolation against a live PostgreSQL instance. Skipped unless
// AEVOR_TEST_DATABASE_DSN is set.
func TestRepositoryPullRequestStore_Integration(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa07-0000-0000-0000-000000000007")
	userB := uuid.MustParse("aaaaaa08-0000-0000-0000-000000000008")

	contextA := uuid.MustParse("bbbbbb05-0000-0000-0000-000000000005")
	contextB := uuid.MustParse("bbbbbb06-0000-0000-0000-000000000006")

	store := NewStore(db)

	cleanupPRs := func() {
		db.Where("github_pull_request_id IN ?", []int64{555001, 555002, 555003}).Delete(&RepositoryPullRequest{})
		db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).Delete(&RepositoryPullRequest{})
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	}

	t.Cleanup(cleanupPRs)
	cleanupPRs()

	if err := db.Create(&SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	if err := db.Create(&SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}).Error; err != nil {
		t.Fatalf("could not seed selected repository B: %v", err)
	}

	githubClosedAt := time.Date(2026, 4, 4, 4, 4, 4, 0, time.UTC)

	batch := []RepositoryPullRequest{
		{
			GithubPullRequestID: 555001,
			Number:              10,
			Title:               "Add pagination",
			State:               "open",
			AuthorLogin:         "octocat",
			HTMLURL:             "https://github.com/octocat/hello-world/pull/10",
			HeadRef:             "feature-pagination",
			BaseRef:             "main",
			Draft:               true,
			GithubCreatedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt:     time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
			SyncedAt:            time.Now(),
		},
		{
			GithubPullRequestID: 555002,
			Number:              11,
			Title:               "Fix race",
			State:               "closed",
			AuthorLogin:         "octocat",
			HTMLURL:             "https://github.com/octocat/hello-world/pull/11",
			HeadRef:             "fix-race",
			BaseRef:             "main",
			Merged:              true,
			GithubCreatedAt:     time.Date(2026, 1, 9, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt:     time.Date(2026, 2, 8, 4, 5, 6, 0, time.UTC),
			GithubClosedAt:      &githubClosedAt,
			GithubMergedAt:      &githubClosedAt,
			SyncedAt:            time.Now(),
		},
	}

	if err := store.UpsertPullRequests(contextA, batch); err != nil {
		t.Fatalf("first sync (insert path) error: %v", err)
	}

	var stored []RepositoryPullRequest

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_pull_request_id ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 2 {
		t.Fatalf("rows = %d, want 2 after first sync", len(stored))
	}

	firstID := stored[0].ID

	time.Sleep(10 * time.Millisecond)

	mergedAt := time.Date(2026, 7, 7, 7, 7, 7, 0, time.UTC)

	refreshed := []RepositoryPullRequest{
		{
			GithubPullRequestID: 555001,
			Number:              10,
			Title:               "Add pagination (v2)",
			State:               "closed",
			AuthorLogin:         "octocat",
			HTMLURL:             "https://github.com/octocat/hello-world/pull/10",
			HeadRef:             "feature-pagination-v2",
			BaseRef:             "develop",
			Merged:              true,
			GithubCreatedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt:     time.Date(2026, 6, 6, 6, 6, 6, 0, time.UTC),
			GithubClosedAt:      &mergedAt,
			GithubMergedAt:      &mergedAt,
			SyncedAt:            time.Now(),
		},
		{
			GithubPullRequestID: 555003,
			Number:              12,
			Title:               "New PR between syncs",
			State:               "open",
			AuthorLogin:         "octocat",
			HTMLURL:             "https://github.com/octocat/hello-world/pull/12",
			HeadRef:             "new-pr",
			BaseRef:             "main",
			Draft:               true,
			GithubCreatedAt:     time.Date(2026, 6, 1, 1, 1, 1, 0, time.UTC),
			GithubUpdatedAt:     time.Date(2026, 6, 2, 2, 2, 2, 0, time.UTC),
			SyncedAt:            time.Now(),
		},
	}

	if err := store.UpsertPullRequests(contextA, refreshed); err != nil {
		t.Fatalf("second sync (conflict path) error: %v", err)
	}

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_pull_request_id ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 3 {
		t.Fatalf("rows = %d, want exactly 3 after repeated sync (no duplicates)", len(stored))
	}

	if stored[0].ID != firstID {
		t.Errorf("conflict path assigned a new UUID %s; want preserved %s", stored[0].ID, firstID)
	}

	if stored[0].Title != "Add pagination (v2)" || stored[0].State != "closed" ||
		!stored[0].Merged || stored[0].Draft ||
		stored[0].GithubMergedAt == nil || stored[0].GithubClosedAt == nil ||
		stored[0].HeadRef != "feature-pagination-v2" || stored[0].BaseRef != "develop" {
		t.Errorf("re-sync did not refresh metadata: %+v", stored[0])
	}

	// The SAME GitHub PR under ANOTHER user's repository context must be
	// independent — per-context uniqueness, not global.
	if err := store.UpsertPullRequests(contextB, []RepositoryPullRequest{batch[0]}); err != nil {
		t.Fatalf("cross-context insert error: %v", err)
	}

	var countForB int64

	db.Model(&RepositoryPullRequest{}).Where("selected_repository_id = ?", contextB).Count(&countForB)

	if countForB != 1 {
		t.Errorf("context B rows = %d, want an independent record for the same GitHub PR", countForB)
	}

	var countTotal int64

	db.Model(&RepositoryPullRequest{}).Where("github_pull_request_id = ?", 555001).Count(&countTotal)

	if countTotal != 2 {
		t.Errorf("global rows for github PR 555001 = %d, want one per context", countTotal)
	}
}

func TestRepositoryPullRequestStore_FindByUserAndID_OwnershipIsolation(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa09-0000-0000-0000-000000000009")
	userB := uuid.MustParse("aaaaaa0a-0000-0000-0000-00000000000a")

	contextA := uuid.MustParse("bbbbbb07-0000-0000-0000-000000000007")
	contextB := uuid.MustParse("bbbbbb08-0000-0000-0000-000000000008")

	store := NewStore(db)

	cleanupOwnership := func() {
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	}

	t.Cleanup(cleanupOwnership)
	cleanupOwnership()

	if err := db.Create(&SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 333, Name: "a-repo", FullName: "someone/a-repo", OwnerLogin: "someone"}).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	if err := db.Create(&SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 444, Name: "b-repo", FullName: "someone/b-repo", OwnerLogin: "someone"}).Error; err != nil {
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
