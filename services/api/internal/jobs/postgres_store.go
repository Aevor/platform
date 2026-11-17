package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PostgresStore struct {
	db *gorm.DB
}

func NewPostgresStore(db *gorm.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Migrate installs the durable job table and a partial idempotency index.
// Empty keys are deliberately excluded so ordinary jobs never collide.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&Job{}); err != nil {
		return err
	}
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_background_jobs_ready
		ON background_jobs (status, scheduled_at, priority DESC, created_at)`).Error; err != nil {
		return err
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_background_jobs_idempotency
		ON background_jobs (user_id, selected_repository_id, idempotency_key)
		WHERE idempotency_key <> ''`).Error
}

func (s *PostgresStore) Insert(ctx context.Context, job *Job) (*Job, bool, error) {
	if err := s.db.WithContext(ctx).Create(job).Error; err != nil {
		if job.IdempotencyKey != "" {
			var existing Job
			findErr := s.db.WithContext(ctx).
				Where("user_id = ? AND selected_repository_id = ? AND idempotency_key = ?",
					job.UserID, job.SelectedRepositoryID, job.IdempotencyKey).
				First(&existing).Error
			if findErr == nil {
				return cloneJob(&existing), false, nil
			}
		}
		return nil, false, err
	}
	return cloneJob(job), true, nil
}

func (s *PostgresStore) Get(ctx context.Context, userID, repositoryID, jobID uuid.UUID) (*Job, error) {
	var job Job
	err := s.db.WithContext(ctx).
		Where("id = ? AND user_id = ? AND selected_repository_id = ?", jobID, userID, repositoryID).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return cloneJob(&job), nil
}

func (s *PostgresStore) FindByIdempotencyKey(ctx context.Context, userID, repositoryID uuid.UUID, key string) (*Job, error) {
	if key == "" { return nil, ErrNotFound }
	var job Job
	err := s.db.WithContext(ctx).Where("user_id = ? AND selected_repository_id = ? AND idempotency_key = ?", userID, repositoryID, key).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) { return nil, ErrNotFound }
	if err != nil { return nil, err }
	return cloneJob(&job), nil
}

func (s *PostgresStore) List(ctx context.Context, userID, repositoryID uuid.UUID, options ListOptions) ([]Job, int, error) {
	query := s.db.WithContext(ctx).Model(&Job{}).
		Where("user_id = ? AND selected_repository_id = ?", userID, repositoryID)
	if options.Status != "" {
		query = query.Where("status = ?", options.Status)
	}
	if options.Type != "" {
		query = query.Where("type = ?", options.Type)
	}

	var count int64
	if err := query.Count(&count).Error; err != nil {
		return nil, 0, err
	}

	var jobs []Job
	result := query.Order("created_at DESC, id ASC")
	if options.Limit > 0 {
		result = result.Limit(options.Limit)
	}
	if options.Offset > 0 {
		result = result.Offset(options.Offset)
	}
	if err := result.Find(&jobs).Error; err != nil {
		return nil, 0, err
	}
	return jobs, int(count), nil
}

func (s *PostgresStore) Claim(ctx context.Context, workerID string, now, leaseUntil time.Time) (*Job, error) {
	if workerID == "" {
		return nil, errors.New("worker id is required")
	}
	var claimed *Job
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Reclaim leases in a transaction. SKIP LOCKED lets another worker
		// recover a different expired job without waiting on this row.
		var expired []Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status IN ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?",
				[]string{StatusRunning, StatusCancelRequested}, now).
			Limit(100).Find(&expired).Error; err != nil {
			return err
		}
		for i := range expired {
			job := expired[i]
			job.LeaseOwner = ""
			job.LeaseExpiresAt = nil
			job.UpdatedAt = now
			if job.Status == StatusCancelRequested {
				job.Status = StatusCancelled
				job.CancelledAt = timePtr(now)
				job.CompletedAt = timePtr(now)
			} else {
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
			if err := tx.Save(&job).Error; err != nil {
				return err
			}
		}

		var candidate Job
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status IN ? AND scheduled_at <= ?", []string{StatusQueued, StatusRetrying}, now).
			Order("priority DESC, scheduled_at ASC, created_at ASC, id ASC").
			Limit(1).Find(&candidate)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}

		candidate.Status = StatusRunning
		candidate.Attempts++
		candidate.LeaseOwner = workerID
		candidate.LeaseExpiresAt = timePtr(leaseUntil)
		if candidate.StartedAt == nil {
			candidate.StartedAt = timePtr(now)
		}
		candidate.UpdatedAt = now
		if err := tx.Save(&candidate).Error; err != nil {
			return err
		}
		claimed = cloneJob(&candidate)
		return nil
	})
	return claimed, err
}

func (s *PostgresStore) RenewLease(ctx context.Context, jobID uuid.UUID, workerID string, leaseUntil time.Time) error {
	result := s.db.WithContext(ctx).Model(&Job{}).
		Where("id = ? AND lease_owner = ? AND status IN ?", jobID, workerID, []string{StatusRunning, StatusCancelRequested}).
		Updates(map[string]any{"lease_expires_at": leaseUntil, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) Save(ctx context.Context, job *Job, workerID string) error {
	result := s.db.WithContext(ctx).Model(&Job{}).
		Where("id = ? AND lease_owner = ? AND status IN ? AND lease_expires_at > ?", job.ID, workerID, []string{StatusRunning, StatusCancelRequested}, time.Now().UTC()).
		Updates(map[string]any{
			"result":           job.Result,
			"status":           gorm.Expr("CASE WHEN status = ? THEN ? ELSE ? END", StatusCancelRequested, StatusCancelled, job.Status),
			"attempts":         job.Attempts,
			"max_attempts":     job.MaxAttempts,
			"scheduled_at":     job.ScheduledAt,
			"started_at":       job.StartedAt,
			"completed_at":     gorm.Expr("CASE WHEN status = ? THEN ? ELSE ? END", StatusCancelRequested, time.Now().UTC(), job.CompletedAt),
			"cancelled_at":     gorm.Expr("CASE WHEN status = ? THEN ? ELSE ? END", StatusCancelRequested, time.Now().UTC(), job.CancelledAt),
			"current_stage":    job.CurrentStage,
			"error_category":   job.ErrorCategory,
			"error_reference":  job.ErrorReference,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"updated_at":       time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) Cancel(ctx context.Context, userID, repositoryID, jobID uuid.UUID, now time.Time) (*Job, error) {
	var job Job
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND selected_repository_id = ?", jobID, userID, repositoryID).
			First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
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
			return ErrNotActive
		}
		job.UpdatedAt = now
		return tx.Save(&job).Error
	})
	if err != nil {
		return nil, err
	}
	return cloneJob(&job), nil
}

func (s *PostgresStore) Retry(ctx context.Context, userID, repositoryID, jobID uuid.UUID, now time.Time) (*Job, error) {
	var job Job
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND user_id = ? AND selected_repository_id = ?", jobID, userID, repositoryID).
			First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if job.Status != StatusFailed {
			return ErrNotFailed
		}
		job.Status = StatusQueued
		job.Attempts = 0
		job.ScheduledAt = now
		job.CompletedAt = nil
		job.CancelledAt = nil
		job.ErrorCategory = ""
		job.ErrorReference = ""
		job.CurrentStage = ""
		job.Result = nil
		job.UpdatedAt = now
		return tx.Save(&job).Error
	})
	if err != nil {
		return nil, err
	}
	return cloneJob(&job), nil
}

func (s *PostgresStore) PruneCompleted(ctx context.Context, before time.Time) (int64, error) {
	result := s.db.WithContext(ctx).Where(
		"status IN ? AND completed_at IS NOT NULL AND completed_at < ?",
		[]string{StatusSucceeded, StatusFailed, StatusCancelled}, before,
	).Delete(&Job{})
	return result.RowsAffected, result.Error
}
