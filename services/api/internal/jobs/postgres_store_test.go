package jobs

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This test is intentionally opt-in and requires a disposable PostgreSQL
// database. Set AEVOR_TEST_DATABASE_DSN; the test scopes cleanup to its own
// randomly generated user ID and never truncates application tables.
func TestPostgresStore_DurabilityIdempotencyAndConcurrentClaims(t *testing.T) {
	dsn := os.Getenv("AEVOR_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set AEVOR_TEST_DATABASE_DSN to run PostgreSQL integration coverage")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate jobs: %v", err)
	}

	ctx := context.Background()
	userID, repositoryID := uuid.New(), uuid.New()
	t.Cleanup(func() { _ = db.WithContext(context.Background()).Where("user_id = ?", userID).Delete(&Job{}).Error })

	storeA := NewPostgresStore(db)
	storeB := NewPostgresStore(db)
	now := time.Now().UTC()
	const jobCount = 16
	for i := 0; i < jobCount; i++ {
		job := &Job{
			ID: uuid.New(), UserID: userID, SelectedRepositoryID: repositoryID,
			Type: "integration.test", Payload: []byte(`{"reference":"only"}`),
			Status: StatusQueued, MaxAttempts: 3,
			IdempotencyKey: "integration-" + uuid.NewString(),
			ScheduledAt:    now, CreatedAt: now, UpdatedAt: now,
		}
		if _, inserted, err := storeA.Insert(ctx, job); err != nil || !inserted {
			t.Fatalf("insert job: inserted=%v err=%v", inserted, err)
		}
	}

	// A second store instance sees the same committed rows (restart/process
	// boundary) and duplicate idempotency keys resolve to the original row.
	var first Job
	if err := db.WithContext(ctx).Where("user_id = ?", userID).First(&first).Error; err != nil {
		t.Fatal(err)
	}
	reloaded, err := storeB.Get(ctx, userID, repositoryID, first.ID)
	if err != nil || reloaded.ID != first.ID {
		t.Fatalf("reload after store recreation: job=%v err=%v", reloaded, err)
	}
	duplicate, inserted, err := storeB.Insert(ctx, &first)
	if err != nil || inserted || duplicate.ID != first.ID {
		t.Fatalf("idempotent insert: inserted=%v job=%v err=%v", inserted, duplicate, err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := make(map[uuid.UUID]int)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := storeA
			if i%2 == 1 {
				store = storeB
			}
			for {
				job, err := store.Claim(ctx, "integration-worker-"+uuid.NewString(), time.Now().UTC(), time.Now().UTC().Add(time.Minute))
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(claimed) != jobCount {
		t.Fatalf("claimed %d/%d distinct jobs", len(claimed), jobCount)
	}
	for id, count := range claimed {
		if count != 1 {
			t.Fatalf("job %s was claimed %d times", id, count)
		}
	}

	// An abandoned lease is recoverable through a new store/worker instance.
	leaseJob := &Job{
		ID: uuid.New(), UserID: userID, SelectedRepositoryID: repositoryID,
		Type: "integration.lease", Payload: []byte(`{}`), Status: StatusQueued,
		MaxAttempts: 3, ScheduledAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := storeA.Insert(ctx, leaseJob); err != nil {
		t.Fatal(err)
	}
	firstClaim, err := storeA.Claim(ctx, "old-worker", now, now.Add(time.Second))
	if err != nil || firstClaim == nil || firstClaim.ID != leaseJob.ID {
		t.Fatalf("initial lease claim: job=%v err=%v", firstClaim, err)
	}
	reclaimed, err := storeB.Claim(ctx, "new-worker", now.Add(2*time.Second), now.Add(3*time.Second))
	if err != nil || reclaimed == nil || reclaimed.ID != leaseJob.ID || reclaimed.Attempts != 2 {
		t.Fatalf("expired lease was not recovered: job=%v err=%v", reclaimed, err)
	}
}
