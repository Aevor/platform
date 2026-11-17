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
		Type:        "issue.analysis",
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
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Enqueue(context.Background(), EnqueueOptions{
		UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "unknown",
	})
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown type error = %v", err)
	}

	ownerID, repositoryID := uuid.New(), uuid.New()
	_, err = service.Enqueue(context.Background(), EnqueueOptions{
		UserID: ownerID, SelectedRepositoryID: repositoryID, Type: "issue.analysis",
		Payload: map[string]string{"issue_id": "not-a-uuid"},
	})
	if !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("invalid under-limit payload error = %v, want ErrInvalidPayload", err)
	}
	valid, err := service.Enqueue(context.Background(), EnqueueOptions{
		UserID: ownerID, SelectedRepositoryID: repositoryID, Type: "issue.analysis",
		Payload: map[string]string{"issue_id": "00000000-0000-4000-8000-000000000001"},
	})
	if err != nil || valid == nil {
		t.Fatalf("valid payload was rejected: job=%v err=%v", valid, err)
	}
	_, err = service.Enqueue(context.Background(), EnqueueOptions{
		UserID: ownerID, SelectedRepositoryID: repositoryID, Type: "issue.analysis",
		Payload: strings.Repeat("x", 256),
	})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("oversized payload error = %v", err)
	}
}

func TestService_EnqueueReturnsExistingJobForIdempotencyKey(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(Definition{Type: "test", Run: func(context.Context, *Job, *Executor) error { return nil }})
	service, err := NewService(NewMemoryStore(), Options{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	options := EnqueueOptions{UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "test", Payload: map[string]any{"reference": "only"}, IdempotencyKey: "stable-key"}
	first, err := service.Enqueue(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Enqueue(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate enqueue created %s; want %s", second.ID, first.ID)
	}
}

func TestWorker_RetryAndSanitizeFailure(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(Definition{
		Type:        "test",
		MaxAttempts: 2,
		Classify:    func(error) ErrorCategory { return CategoryTransientExternalError },
		Run: func(context.Context, *Job, *Executor) error {
			return errors.New("token=ghp_secret and raw response body")
		},
	})
	store := NewMemoryStore()
	service, err := NewService(store, Options{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(context.Background(), EnqueueOptions{UserID: uuid.New(), SelectedRepositoryID: uuid.New(), Type: "test"})
	if err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(service, "test-worker", WorkerOptions{RetryBaseDelay: time.Millisecond, RetryMaxDelay: time.Millisecond})
	if _, err := worker.ProcessNow(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	stored, err := service.Get(context.Background(), job.UserID, job.SelectedRepositoryID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusRetrying {
		t.Fatalf("first status = %s, want retrying", stored.Status)
	}
	if stored.ErrorReference != "external_service_failure" || strings.Contains(stored.ErrorReference, "ghp_secret") || strings.Contains(stored.ErrorReference, "raw response") {
		t.Fatalf("failure metadata is not sanitized: category=%q reference=%q", stored.ErrorCategory, stored.ErrorReference)
	}
	if strings.Contains(string(stored.Payload), "ghp_secret") || strings.Contains(string(stored.Result), "ghp_secret") {
		t.Fatal("job payload/result contains raw secret")
	}

	// The backoff is intentionally tiny in this test so a second attempt can
	// be observed without editing persisted job state out of band.
	time.Sleep(10 * time.Millisecond)
	if _, err := worker.ProcessNow(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	stored, err = service.Get(context.Background(), job.UserID, job.SelectedRepositoryID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusFailed || stored.Attempts != 2 {
		t.Fatalf("second status=%s attempts=%d, want failed after two attempts", stored.Status, stored.Attempts)
	}
	if stored.ErrorReference != "external_service_failure" {
		t.Fatalf("terminal error reference = %q, want sanitized stable reference", stored.ErrorReference)
	}
}
