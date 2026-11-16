package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testJob(now time.Time) *Job {
	return &Job{
		ID: uuid.New(),
		UserID: uuid.New(),
		SelectedRepositoryID: uuid.New(),
		Type: "test",
		Payload: []byte(`{}`),
		Status: StatusQueued,
		MaxAttempts: 3,
		ScheduledAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestMemoryStore_IdempotentInsert(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	job := testJob(now)
	job.IdempotencyKey = "same-request"

	first, inserted, err := store.Insert(context.Background(), job)
	if err != nil || !inserted { t.Fatalf("first insert: inserted=%v err=%v", inserted, err) }
	second := cloneJob(job)
	second.ID = uuid.New()
	stored, inserted, err := store.Insert(context.Background(), second)
	if err != nil || inserted { t.Fatalf("duplicate insert: inserted=%v err=%v", inserted, err) }
	if stored.ID != first.ID { t.Fatalf("duplicate returned id %s, want %s", stored.ID, first.ID) }
}

func TestMemoryStore_ClaimIsExclusive(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	job := testJob(now)
	if _, _, err := store.Insert(context.Background(), job); err != nil { t.Fatal(err) }

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := make(map[uuid.UUID]int)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := store.Claim(context.Background(), uuid.NewString(), now, now.Add(time.Minute))
			if err != nil { t.Errorf("claim: %v", err); return }
			if got != nil {
				mu.Lock()
				claimed[got.ID]++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(claimed) != 1 || claimed[job.ID] != 1 {
		t.Fatalf("claims = %v, expected exactly one claim for %s", claimed, job.ID)
	}
}

func TestMemoryStore_ExpiredLeaseIsRecoverableAndFencesOldWorker(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	job := testJob(now)
	if _, _, err := store.Insert(context.Background(), job); err != nil { t.Fatal(err) }

	first, err := store.Claim(context.Background(), "worker-a", now, now.Add(time.Second))
	if err != nil || first == nil { t.Fatalf("first claim: job=%v err=%v", first, err) }
	second, err := store.Claim(context.Background(), "worker-b", now.Add(2*time.Second), now.Add(3*time.Second))
	if err != nil || second == nil { t.Fatalf("recovered claim: job=%v err=%v", second, err) }
	if second.ID != job.ID || second.Attempts != 2 || second.LeaseOwner != "worker-b" {
		t.Fatalf("recovered job = %+v, want same job, attempt 2, worker-b", second)
	}
	first.Status = StatusSucceeded
	if err := store.Save(context.Background(), first, "worker-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale worker save error = %v, want ErrLeaseLost", err)
	}
}

func TestMemoryStore_RetriesThenMarksExpiredJobFailed(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	job := testJob(now)
	job.MaxAttempts = 1
	if _, _, err := store.Insert(context.Background(), job); err != nil { t.Fatal(err) }
	first, err := store.Claim(context.Background(), "worker-a", now, now.Add(time.Second))
	if err != nil || first == nil { t.Fatalf("claim: %v", err) }
	recovered, err := store.Claim(context.Background(), "worker-b", now.Add(2*time.Second), now.Add(3*time.Second))
	if err != nil { t.Fatal(err) }
	if recovered != nil { t.Fatalf("job with exhausted attempts should become failed, got %+v", recovered) }
	stored, err := store.Get(context.Background(), job.UserID, job.SelectedRepositoryID, job.ID)
	if err != nil { t.Fatal(err) }
	if stored.Status != StatusFailed || stored.ErrorReference != "worker_lease_expired" {
		t.Fatalf("recovered job = %+v, want terminal failure with bounded lease reference", stored)
	}
}
