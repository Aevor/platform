package jobs

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This test is intentionally opt-in and requires a fresh, disposable PostgreSQL
// database whose name ends in "_test". Never point it at the application DB.
// The test scopes cleanup to its own randomly generated user ID.
func TestPostgresStore_DurabilityIdempotencyAndConcurrentClaims(t *testing.T) {
	dsn := os.Getenv("AEVOR_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set AEVOR_TEST_DATABASE_DSN to run PostgreSQL integration coverage")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	var databaseName string
	if err := db.Raw("SELECT current_database()").Scan(&databaseName).Error; err != nil {
		t.Fatalf("identify PostgreSQL test database: %v", err)
	}
	if !strings.HasSuffix(databaseName, "_test") {
		t.Fatalf("refusing to run integration test against database %q: database name must end in _test", databaseName)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate jobs: %v", err)
	}

	ctx := context.Background()
	userID, repositoryID := uuid.New(), uuid.New()
	t.Cleanup(func() { _ = db.WithContext(context.Background()).Where("user_id = ?", userID).Delete(&Job{}).Error })

	storeA := NewPostgresStore(db)
	dbB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open second PostgreSQL pool: %v", err)
	}
	sqlDBB, err := dbB.DB()
	if err != nil {
		t.Fatalf("get second SQL pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDBB.Close() })
	storeB := NewPostgresStore(dbB)
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
	duplicateInput := first
	duplicateInput.ID = uuid.New()
	duplicate, inserted, err := storeB.Insert(ctx, &duplicateInput)
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

	// Finish the concurrency fixture rows so the subsequent lease scenario is
	// isolated from any remaining ready/leased work for this test owner.
	if err := db.WithContext(ctx).Model(&Job{}).Where("user_id = ?", userID).Updates(map[string]any{
		"status": StatusSucceeded, "completed_at": now, "lease_owner": "", "lease_expires_at": nil,
	}).Error; err != nil {
		t.Fatalf("finish concurrent claim fixtures: %v", err)
	}

	// An abandoned lease is recoverable across two independently opened
	// PostgreSQL pools, survives a retry transition, and records exhaustion.
	leaseNow := time.Now().UTC().Add(time.Second)
	leaseJob := &Job{
		ID: uuid.New(), UserID: userID, SelectedRepositoryID: repositoryID,
		Type: "integration.lease", Payload: []byte(`{}`), Status: StatusQueued,
		MaxAttempts: 2, ScheduledAt: leaseNow, CreatedAt: leaseNow, UpdatedAt: leaseNow,
	}
	if _, _, err := storeA.Insert(ctx, leaseJob); err != nil {
		t.Fatal(err)
	}
	firstClaim, err := storeA.Claim(ctx, "old-worker", leaseNow, leaseNow.Add(time.Second))
	if err != nil || firstClaim == nil || firstClaim.ID != leaseJob.ID || firstClaim.Attempts != 1 {
		t.Fatalf("initial lease claim: job=%v err=%v", firstClaim, err)
	}
	reclaimed, err := storeB.Claim(ctx, "new-worker", leaseNow.Add(2*time.Second), leaseNow.Add(3*time.Second))
	if err != nil || reclaimed == nil || reclaimed.ID != leaseJob.ID || reclaimed.Attempts != 2 {
		t.Fatalf("expired lease was not recovered: job=%v err=%v", reclaimed, err)
	}
	if terminalClaim, err := storeA.Claim(ctx, "final-worker", leaseNow.Add(4*time.Second), leaseNow.Add(5*time.Second)); err != nil || terminalClaim != nil {
		t.Fatalf("exhausted lease claim = %v, err=%v; want no runnable job", terminalClaim, err)
	}
	failed, err := storeB.Get(ctx, userID, repositoryID, leaseJob.ID)
	if err != nil || failed.Status != StatusFailed || failed.Attempts != 2 || failed.ErrorReference != "worker_lease_expired" {
		t.Fatalf("persisted exhausted job = %+v err=%v", failed, err)
	}
	retried, err := storeB.Retry(ctx, userID, repositoryID, leaseJob.ID, leaseNow.Add(6*time.Second))
	if err != nil || retried.Status != StatusQueued || retried.Attempts != 0 || len(retried.Result) != 0 {
		t.Fatalf("manual retry = %+v err=%v; want clean queued attempt", retried, err)
	}
}
