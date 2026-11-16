package ailimit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAILimiter_Allow(t *testing.T) {
	ctx := context.Background()
	limiter := NewAILimiter(5, 10, 20, time.Minute)

	userID := uuid.New()
	repoID := uuid.New()

	for i := 0; i < 5; i++ {
		allowed, _, err := limiter.Allow(ctx, userID, repoID)
		if err != nil {
			t.Fatalf("unexpected error on request %d: %v", i+1, err)
		}
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	allowed, retryAfter, err := limiter.Allow(ctx, userID, repoID)
	if err == ErrAIRateLimited {
		// Expected rate limit error
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("request should be denied after per-user limit")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter should be positive, got %v", retryAfter)
	}
}

func TestAILimiter_PerRepoLimit(t *testing.T) {
	ctx := context.Background()
	limiter := NewAILimiter(10, 3, 20, time.Minute)

	userID := uuid.New()
	repoID := uuid.New()

	for i := 0; i < 3; i++ {
		allowed, _, err := limiter.Allow(ctx, userID, repoID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	allowed, _, err := limiter.Allow(ctx, userID, repoID)
	if err == ErrAIRateLimited {
		// Expected rate limit error
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("request should be denied after per-repo limit")
	}
}

func TestAILimiter_GlobalLimit(t *testing.T) {
	ctx := context.Background()
	limiter := NewAILimiter(10, 10, 5, time.Minute)

	for i := 0; i < 5; i++ {
		allowed, _, err := limiter.Allow(ctx, uuid.New(), uuid.New())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	allowed, _, err := limiter.Allow(ctx, uuid.New(), uuid.New())
	if err == ErrAIRateLimited {
		// Expected rate limit error
	} else if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Error("request should be denied after global limit")
	}
}

func TestAILimiter_WindowReset(t *testing.T) {
	limiter := NewAILimiter(2, 10, 20, 100*time.Millisecond)
	ctx := context.Background()

	userID := uuid.New()
	repoID := uuid.New()

	allowed, _, _ := limiter.Allow(ctx, userID, repoID)
	if !allowed {
		t.Error("first request should be allowed")
	}
	allowed, _, _ = limiter.Allow(ctx, userID, repoID)
	if !allowed {
		t.Error("second request should be allowed")
	}
	allowed, _, _ = limiter.Allow(ctx, userID, repoID)
	if allowed {
		t.Error("third request should be denied")
	}

	time.Sleep(150 * time.Millisecond)

	allowed, _, _ = limiter.Allow(ctx, userID, repoID)
	if !allowed {
		t.Error("request after window reset should be allowed")
	}
}
