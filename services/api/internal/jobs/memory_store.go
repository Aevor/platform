package jobs

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	jobs map[uuid.UUID]*Job
	keys map[string]uuid.UUID
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: make(map[uuid.UUID]*Job), keys: make(map[string]uuid.UUID)}
}

func idempotencyScope(job *Job) string {
	if job.IdempotencyKey == "" { return "" }
	return job.UserID.String() + ":" + job.SelectedRepositoryID.String() + ":" + job.IdempotencyKey
}

func (s *MemoryStore) Insert(ctx context.Context, job *Job) (*Job, bool, error) {
	if err := ctx.Err(); err != nil { return nil, false, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	key := idempotencyScope(job)
	if key != "" {
		if id, ok := s.keys[key]; ok {
			return cloneJob(s.jobs[id]), false, nil
		}
	}
	copy := cloneJob(job)
	s.jobs[copy.ID] = copy
	if key != "" { s.keys[key] = copy.ID }
	return cloneJob(copy), true, nil
}

func (s *MemoryStore) Get(ctx context.Context, userID, repositoryID, jobID uuid.UUID) (*Job, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.UserID != userID || job.SelectedRepositoryID != repositoryID {
		return nil, ErrNotFound
	}
	return cloneJob(job), nil
}

func (s *MemoryStore) List(ctx context.Context, userID, repositoryID uuid.UUID, options ListOptions) ([]Job, int, error) {
	if err := ctx.Err(); err != nil { return nil, 0, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]Job, 0)
	for _, job := range s.jobs {
		if job.UserID != userID || job.SelectedRepositoryID != repositoryID { continue }
		if options.Status != "" && job.Status != options.Status { continue }
		if options.Type != "" && job.Type != options.Type { continue }
		all = append(all, *cloneJob(job))
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Priority != all[j].Priority { return all[i].Priority > all[j].Priority }
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) { return all[i].CreatedAt.After(all[j].CreatedAt) }
		return all[i].ID.String() < all[j].ID.String()
	})
	count := len(all)
	if options.Offset >= len(all) { return []Job{}, count, nil }
	if options.Offset > 0 { all = all[options.Offset:] }
	if options.Limit > 0 && len(all) > options.Limit { all = all[:options.Limit] }
	return all, count, nil
}

func (s *MemoryStore) recoverExpired(now time.Time) {
	for _, job := range s.jobs {
		if (job.Status != StatusRunning && job.Status != StatusCancelRequested) || job.LeaseExpiresAt == nil || job.LeaseExpiresAt.After(now) {
			continue
		}
		job.LeaseOwner = ""
		job.LeaseExpiresAt = nil
		job.UpdatedAt = now
		if job.Status == StatusCancelRequested {
			job.Status = StatusCancelled
			job.CancelledAt = timePtr(now)
			job.CompletedAt = timePtr(now)
			continue
		}
		job.ErrorCategory = CategoryInternalFailure
		job.ErrorReference = "worker_lease_expired"
		if job.Attempts >= job.MaxAttempts {
			job.Status = StatusFailed
			job.CompletedAt = timePtr(now)
		} else {
			job.Status = StatusRetrying
			job.ScheduledAt = now
		}
	}
}

func (s *MemoryStore) Claim(ctx context.Context, workerID string, now, leaseUntil time.Time) (*Job, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverExpired(now)
	candidates := make([]*Job, 0)
	for _, job := range s.jobs {
		if (job.Status == StatusQueued || job.Status == StatusRetrying) && !job.ScheduledAt.After(now) {
			candidates = append(candidates, job)
		}
	}
	if len(candidates) == 0 { return nil, nil }
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority { return candidates[i].Priority > candidates[j].Priority }
		if !candidates[i].ScheduledAt.Equal(candidates[j].ScheduledAt) { return candidates[i].ScheduledAt.Before(candidates[j].ScheduledAt) }
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	job := candidates[0]
	job.Status = StatusRunning
	job.Attempts++
	job.LeaseOwner = workerID
	job.LeaseExpiresAt = timePtr(leaseUntil)
	if job.StartedAt == nil { job.StartedAt = timePtr(now) }
	job.UpdatedAt = now
	return cloneJob(job), nil
}

func (s *MemoryStore) RenewLease(ctx context.Context, jobID uuid.UUID, workerID string, leaseUntil time.Time) error {
	if err := ctx.Err(); err != nil { return err }
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.LeaseOwner != workerID || job.Status != StatusRunning { return ErrLeaseLost }
	job.LeaseExpiresAt = timePtr(leaseUntil)
	job.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *MemoryStore) Save(ctx context.Context, job *Job, workerID string) error {
	if err := ctx.Err(); err != nil { return err }
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.jobs[job.ID]
	if !ok || current.LeaseOwner != workerID || current.UserID != job.UserID || current.SelectedRepositoryID != job.SelectedRepositoryID {
		return ErrLeaseLost
	}
	if current.Status != StatusRunning && current.Status != StatusCancelRequested { return ErrLeaseLost }
	copy := cloneJob(job)
	copy.LeaseOwner = ""
	copy.LeaseExpiresAt = nil
	copy.UpdatedAt = time.Now().UTC()
	s.jobs[job.ID] = copy
	return nil
}

func (s *MemoryStore) Cancel(ctx context.Context, userID, repositoryID, jobID uuid.UUID, now time.Time) (*Job, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.UserID != userID || job.SelectedRepositoryID != repositoryID { return nil, ErrNotFound }
	switch job.Status {
	case StatusQueued, StatusRetrying:
		job.Status = StatusCancelled
		job.CancelledAt = timePtr(now)
		job.CompletedAt = timePtr(now)
		job.LeaseOwner = ""
		job.LeaseExpiresAt = nil
	case StatusRunning:
		job.Status = StatusCancelRequested
	default:
		return nil, ErrNotActive
	}
	job.UpdatedAt = now
	return cloneJob(job), nil
}

func (s *MemoryStore) Retry(ctx context.Context, userID, repositoryID, jobID uuid.UUID, now time.Time) (*Job, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok || job.UserID != userID || job.SelectedRepositoryID != repositoryID { return nil, ErrNotFound }
	if job.Status != StatusFailed { return nil, ErrNotFailed }
	job.Status = StatusQueued
	job.Attempts = 0
	job.ScheduledAt = now
	job.CompletedAt = nil
	job.CancelledAt = nil
	job.ErrorCategory = ""
	job.ErrorReference = ""
	job.CurrentStage = ""
	job.UpdatedAt = now
	return cloneJob(job), nil
}

func (s *MemoryStore) PruneCompleted(ctx context.Context, before time.Time) (int64, error) {
	if err := ctx.Err(); err != nil { return 0, err }
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for id, job := range s.jobs {
		if terminal(job.Status) && job.CompletedAt != nil && job.CompletedAt.Before(before) {
			key := idempotencyScope(job)
			if key != "" { delete(s.keys, key) }
			delete(s.jobs, id)
			deleted++
		}
	}
	return deleted, nil
}

func timePtr(value time.Time) *time.Time { v := value; return &v }
