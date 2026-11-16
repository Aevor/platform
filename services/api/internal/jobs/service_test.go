package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestService_EnqueueValidatesAndBoundsPayload(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(Definition{
		Type: "issue.analysis",
		MaxAttempts: 3,
		ValidatePayload: func(raw json.RawMessage) (any, error) {
			if string(raw) != `{"issue_id":"00000000-0000-4000-8000-000000000001"}` {
				return nil, errors.New("invalid issue reference")
			}
			return nil, nil
		},
		Run: func(context.Context, *Job, *Executor) error { return nil },
	})
	service, err := NewService(NewMemoryStore(), Options{Registry: registry, MaxPayloadBytes: 128})
	if err != nil { t.Fatal(err) }

	_, err = service.Enqueue(context.Background(), EnqueueOptions{
		UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "unknown",
	})
	if !errors.Is(err, ErrUnknownType) { t.Fatalf("unknown type error = %v", err) }

	_, err = service.Enqueue(context.Background(), EnqueueOptions{
		UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "issue.analysis",
		Payload: strings.Repeat("x", 256),
	})
	if !errors.Is(err, ErrPayloadTooLarge) { t.Fatalf("oversized payload error = %v", err) }
}

func TestService_EnqueueReturnsExistingJobForIdempotencyKey(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(Definition{Type: "test", Run: func(context.Context, *Job, *Executor) error { return nil }})
	service, err := NewService(NewMemoryStore(), Options{Registry: registry})
	if err != nil { t.Fatal(err) }
	options := EnqueueOptions{UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "test", Payload: map[string]any{"reference":"only"}, IdempotencyKey:"stable-key"}
	first, err := service.Enqueue(context.Background(), options)
	if err != nil { t.Fatal(err) }
	second, err := service.Enqueue(context.Background(), options)
	if err != nil { t.Fatal(err) }
	if first.ID != second.ID { t.Fatalf("duplicate enqueue created %s; want %s", second.ID, first.ID) }
}

func TestWorker_RetryAndSanitizeFailure(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(Definition{
		Type: "test",
		MaxAttempts: 2,
		Classify: func(error) ErrorCategory { return CategoryTransientExternalError },
		Run: func(context.Context, *Job, *Executor) error { return errors.New("token=ghp_secret and raw response body") },
	})
	store := NewMemoryStore()
	service, err := NewService(store, Options{Registry: registry})
	if err != nil { t.Fatal(err) }
	now := time.Now().UTC()
	job, err := service.Enqueue(context.Background(), EnqueueOptions{UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type:"test"})
	if err != nil { t.Fatal(err) }

	worker := NewWorker(service, "test-worker", WorkerOptions{RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond})
	if _, err := worker.ProcessNow(context.Background(), 1); err != nil { t.Fatal(err) }
	stored, err := service.Get(context.Background(), job.UserID, job.SelectedRepositoryID, job.ID)
	if err != nil { t.Fatal(err) }
	if stored.Status != StatusRetrying { t.Fatalf("status = %s, want retrying", stored.Status) }
	if strings.Contains(string(stored.Payload), "ghp_secret") || strings.Contains(string(stored.Result), "ghp_secret") {
		t.Fatal("job payload/result contains raw secret")
	}

	// Advance the schedule without depending on a wall-clock sleep.
	stored.ScheduledAt = now.Add(-time.Second)
	if err := store.(*MemoryStore).Save(context.Background(), stored, "test-worker"); err == nil {
		t.Fatal("unowned mutation unexpectedly succeeded")
	}
}
