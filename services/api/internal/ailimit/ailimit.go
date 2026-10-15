package ailimit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrAIRateLimited = errors.New("ai_rate_limited")
)

type AILimiter struct {
	mu           sync.Mutex
	perUserLimit int
	perRepoLimit int
	globalLimit  int
	window       time.Duration
	userCounts   map[uuid.UUID]int
	repoCounts   map[uuid.UUID]int
	globalCount  int
	windowStart  time.Time
}

func NewAILimiter(perUserLimit, perRepoLimit, globalLimit int, window time.Duration) *AILimiter {
	if window <= 0 {
		window = time.Minute
	}
	return &AILimiter{
		perUserLimit: perUserLimit,
		perRepoLimit: perRepoLimit,
		globalLimit:  globalLimit,
		window:       window,
		userCounts:   make(map[uuid.UUID]int),
		repoCounts:   make(map[uuid.UUID]int),
		windowStart:  time.Now(),
	}
}

func (l *AILimiter) Allow(ctx context.Context, userID, repoID uuid.UUID) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.windowStart) >= l.window {
		l.userCounts = make(map[uuid.UUID]int)
		l.repoCounts = make(map[uuid.UUID]int)
		l.globalCount = 0
		l.windowStart = now
	}

	if l.globalLimit > 0 && l.globalCount >= l.globalLimit {
		return false, l.window - now.Sub(l.windowStart), ErrAIRateLimited
	}

	if l.perUserLimit > 0 {
		if l.userCounts[userID] >= l.perUserLimit {
			return false, l.window - now.Sub(l.windowStart), ErrAIRateLimited
		}
	}

	if l.perRepoLimit > 0 {
		if l.repoCounts[repoID] >= l.perRepoLimit {
			return false, l.window - now.Sub(l.windowStart), ErrAIRateLimited
		}
	}

	l.globalCount++
	l.userCounts[userID]++
	l.repoCounts[repoID]++
	return true, 0, nil
}

func (l *AILimiter) Stats() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{
		"global_count":   l.globalCount,
		"global_limit":   l.globalLimit,
		"per_user_limit": l.perUserLimit,
		"per_repo_limit": l.perRepoLimit,
		"window_seconds": l.window.Seconds(),
		"user_counts":    l.userCounts,
		"repo_counts":    l.repoCounts,
	}
}
