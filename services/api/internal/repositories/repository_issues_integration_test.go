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

// TestRepositoryIssueStore_Integration exercises the real issue upsert SQL,
// the (selected_repository_id, github_issue_id) unique index, and ownership
// isolation against a live PostgreSQL instance. Skipped unless
// AEVOR_TEST_DATABASE_DSN is set, e.g.:
//
//	AEVOR_TEST_DATABASE_DSN="host=localhost port=5432 user=aevor password=aevor dbname=aevor sslmode=disable" go test ./internal/repositories -run Integration -v
func TestRepositoryIssueStore_Integration(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa03-0000-0000-0000-000000000003")
	userB := uuid.MustParse("aaaaaa04-0000-0000-0000-000000000004")

	contextA := uuid.MustParse("bbbbbb01-0000-0000-0000-000000000001")
	contextB := uuid.MustParse("bbbbbb02-0000-0000-0000-000000000002")

	store := NewStore(db)

	t.Cleanup(func() {
		db.Where("github_issue_id IN ?", []int64{424242, 424243, 424244}).Delete(&RepositoryIssue{})
		db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).Delete(&RepositoryIssue{})
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	})

	db.Where("github_issue_id IN ?", []int64{424242, 424243, 424244}).Delete(&RepositoryIssue{})
	db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).Delete(&RepositoryIssue{})
	db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})

	seeded := &SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}

	if err := db.Create(seeded).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	seededB := &SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 1296269, Name: "hello-world", FullName: "octocat/hello-world", OwnerLogin: "octocat"}

	if err := db.Create(seededB).Error; err != nil {
		t.Fatalf("could not seed selected repository B: %v", err)
	}

	githubClosedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	batch := []RepositoryIssue{
		{
			GithubIssueID:   424242,
			Number:          1,
			Title:           "Fix login bug",
			State:           "open",
			AuthorLogin:     "octocat",
			HTMLURL:         "https://github.com/octocat/hello-world/issues/1",
			GithubCreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
			SyncedAt:        time.Now(),
		},
		{
			GithubIssueID:   424243,
			Number:          2,
			Title:           "Add dark mode",
			State:           "closed",
			AuthorLogin:     "octocat",
			HTMLURL:         "https://github.com/octocat/hello-world/issues/2",
			GithubCreatedAt: time.Date(2026, 1, 9, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt: time.Date(2026, 2, 8, 4, 5, 6, 0, time.UTC),
			GithubClosedAt:  &githubClosedAt,
			SyncedAt:        time.Now(),
		},
	}

	if err := store.UpsertIssues(contextA, batch); err != nil {
		t.Fatalf("first sync (insert path) error: %v", err)
	}

	var stored []RepositoryIssue

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_issue_id ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 2 {
		t.Fatalf("rows = %d, want 2 after first sync", len(stored))
	}

	firstID := stored[0].ID

	time.Sleep(10 * time.Millisecond)

	newClosedAt := time.Date(2026, 7, 7, 7, 7, 7, 0, time.UTC)

	refreshed := []RepositoryIssue{
		{
			GithubIssueID:   424242,
			Number:          1,
			Title:           "Fix login bug (renamed)",
			State:           "closed",
			AuthorLogin:     "octocat",
			HTMLURL:         "https://github.com/octocat/hello-world/issues/1",
			GithubCreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			GithubUpdatedAt: time.Date(2026, 6, 6, 6, 6, 6, 0, time.UTC),
			GithubClosedAt:  &newClosedAt,
			SyncedAt:        time.Now(),
		},
		{
			GithubIssueID:   424244,
			Number:          3,
			Title:           "New issue between syncs",
			State:           "open",
			AuthorLogin:     "octocat",
			HTMLURL:         "https://github.com/octocat/hello-world/issues/3",
			GithubCreatedAt: time.Date(2026, 6, 1, 1, 1, 1, 0, time.UTC),
			GithubUpdatedAt: time.Date(2026, 6, 2, 2, 2, 2, 0, time.UTC),
			SyncedAt:        time.Now(),
		},
	}

	if err := store.UpsertIssues(contextA, refreshed); err != nil {
		t.Fatalf("second sync (conflict path) error: %v", err)
	}

	if err := db.Where("selected_repository_id = ?", contextA).Order("github_issue_id ASC").Find(&stored).Error; err != nil {
		t.Fatalf("read-back error: %v", err)
	}

	if len(stored) != 3 {
		t.Fatalf("rows = %d, want exactly 3 after repeated sync (no duplicates)", len(stored))
	}

	if stored[0].ID != firstID {
		t.Errorf("conflict path assigned a new UUID %s; want preserved %s", stored[0].ID, firstID)
	}

	if stored[0].Title != "Fix login bug (renamed)" || stored[0].State != "closed" || stored[0].GithubClosedAt == nil {
		t.Errorf("re-sync did not refresh metadata: %+v", stored[0])
	}

	// The SAME GitHub issue under ANOTHER user's repository context must be
	// independent — per-context uniqueness, not global.
	sameIssueOtherContext := []RepositoryIssue{batch[0]}

	if err := store.UpsertIssues(contextB, sameIssueOtherContext); err != nil {
		t.Fatalf("cross-context insert error: %v", err)
	}

	var countForB int64

	db.Model(&RepositoryIssue{}).Where("selected_repository_id = ?", contextB).Count(&countForB)

	if countForB != 1 {
		t.Errorf("context B rows = %d, want an independent record for the same GitHub issue", countForB)
	}

	var countTotal int64

	db.Model(&RepositoryIssue{}).Where("github_issue_id = ?", 424242).Count(&countTotal)

	if countTotal != 2 {
		t.Errorf("global rows for github issue 424242 = %d, want one per context", countTotal)
	}
}

func TestRepositoryIssueStore_FindByUserAndID_OwnershipIsolation(t *testing.T) {
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

	userA := uuid.MustParse("aaaaaa05-0000-0000-0000-000000000005")
	userB := uuid.MustParse("aaaaaa06-0000-0000-0000-000000000006")

	contextA := uuid.MustParse("bbbbbb03-0000-0000-0000-000000000003")
	contextB := uuid.MustParse("bbbbbb04-0000-0000-0000-000000000004")

	store := NewStore(db)

	t.Cleanup(func() {
		db.Where("github_issue_id IN ?", []int64{424242, 424243, 424244}).Delete(&RepositoryIssue{})
		db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).Delete(&RepositoryIssue{})
		db.Where("id IN ?", []uuid.UUID{contextA, contextB}).Delete(&SelectedRepository{})
		db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})
	})

	db.Where("github_issue_id IN ?", []int64{424242, 424243, 424244}).Delete(&RepositoryIssue{})
	db.Where("selected_repository_id IN ?", []uuid.UUID{contextA, contextB}).Delete(&RepositoryIssue{})
	db.Where("user_id IN ?", []uuid.UUID{userA, userB}).Delete(&SelectedRepository{})

	if err := db.Create(&SelectedRepository{ID: contextA, UserID: userA, GithubRepositoryID: 111, Name: "a-repo", FullName: "someone/a-repo", OwnerLogin: "someone"}).Error; err != nil {
		t.Fatalf("could not seed selected repository A: %v", err)
	}

	if err := db.Create(&SelectedRepository{ID: contextB, UserID: userB, GithubRepositoryID: 222, Name: "b-repo", FullName: "someone/b-repo", OwnerLogin: "someone"}).Error; err != nil {
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
