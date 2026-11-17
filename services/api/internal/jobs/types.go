package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	StatusQueued          = "queued"
	StatusRunning         = "running"
	StatusRetrying        = "retrying"
	StatusSucceeded       = "succeeded"
	StatusFailed          = "failed"
	StatusCancelRequested = "cancel_requested"
	StatusCancelled       = "cancelled"
)

var (
	ErrNotFound        = errors.New("job not found")
	ErrAlreadyExists   = errors.New("job already exists")
	ErrNotActive       = errors.New("job is not active")
	ErrNotFailed       = errors.New("job is not failed")
	ErrNotTerminal     = errors.New("job is not terminal")
	ErrUnknownType     = errors.New("unknown job type")
	ErrInvalidPayload  = errors.New("invalid job payload")
	ErrLeaseLost       = errors.New("job lease is no longer owned by this worker")
	ErrPayloadTooLarge = errors.New("job payload exceeds configured limit")
	ErrResultTooLarge  = errors.New("job result exceeds configured limit")
)

type ErrorCategory string

const (
	CategoryAIServiceFailure       ErrorCategory = "AI_SERVICE_FAILURE"
	CategoryTimeout                ErrorCategory = "TIMEOUT"
	CategoryTransientExternalError ErrorCategory = "TRANSIENT_EXTERNAL"
	CategoryDatabaseFailure        ErrorCategory = "DATABASE_FAILURE"
	CategoryInternalFailure        ErrorCategory = "INTERNAL_FAILURE"
	CategoryAuthorizationError     ErrorCategory = "AUTHORIZATION"
	CategoryValidationError        ErrorCategory = "VALIDATION"
	CategoryUserError              ErrorCategory = "USER_ERROR"
)

func (c ErrorCategory) Retryable() bool {
	switch c {
	case CategoryAIServiceFailure, CategoryTimeout, CategoryTransientExternalError,
		CategoryDatabaseFailure, CategoryInternalFailure:
		return true
	default:
		return false
	}
}

// Job stores bounded references and lifecycle metadata. Payloads are limited
// at enqueue time; callers should put identifiers in them, never source files,
// GitHub tokens, or model output.
type Job struct {
	ID                   uuid.UUID       `gorm:"type:uuid;primaryKey" json:"id"`
	UserID               uuid.UUID       `gorm:"type:uuid;not null;index:idx_jobs_owner_status,priority:1" json:"-"`
	SelectedRepositoryID uuid.UUID       `gorm:"type:uuid;not null;index:idx_jobs_owner_status,priority:2" json:"repository_id"`
	RunID                *uuid.UUID      `gorm:"type:uuid;index" json:"run_id,omitempty"`
	Type                 string          `gorm:"size:96;not null;index" json:"type"`
	Payload              json.RawMessage `gorm:"type:jsonb;not null" json:"-"`
	Result               json.RawMessage `gorm:"type:jsonb" json:"-"`
	Status               string          `gorm:"size:24;not null;index:idx_jobs_owner_status,priority:3" json:"status"`
	Priority             int             `gorm:"not null;default:0" json:"priority"`
	IdempotencyKey       string          `gorm:"size:128;not null;default:''" json:"idempotency_key,omitempty"`
	Attempts             int             `gorm:"not null;default:0" json:"attempts"`
	MaxAttempts          int             `gorm:"not null;default:3" json:"max_attempts"`
	ScheduledAt          time.Time       `gorm:"not null;index:idx_jobs_schedule" json:"scheduled_at"`
	StartedAt            *time.Time      `json:"started_at,omitempty"`
	CompletedAt          *time.Time      `gorm:"index" json:"completed_at,omitempty"`
	CancelledAt          *time.Time      `json:"cancelled_at,omitempty"`
	CurrentStage         string          `gorm:"size:64;not null;default:''" json:"current_stage"`
	ErrorCategory        ErrorCategory   `gorm:"size:32;not null;default:''" json:"error_category"`
	ErrorReference       string          `gorm:"size:255;not null;default:''" json:"error_reference"`
	LeaseOwner           string          `gorm:"size:128;not null;default:'';index" json:"-"`
	LeaseExpiresAt       *time.Time      `gorm:"index" json:"-"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

func (Job) TableName() string { return "background_jobs" }

type EnqueueOptions struct {
	UserID               uuid.UUID
	SelectedRepositoryID uuid.UUID
	RunID                *uuid.UUID
	Type                 string
	Payload              any
	Priority             int
	IdempotencyKey       string
	MaxAttempts          int
}

type ListOptions struct {
	Status string
	Type   string
	Limit  int
	Offset int
}

type Definition struct {
	Type            string
	Description     string
	ValidatePayload func(json.RawMessage) (any, error)
	Timeout         time.Duration
	MaxAttempts     int
	Classify        func(error) ErrorCategory
	Run             func(context.Context, *Job, *Executor) error
}

type JobEvent struct {
	JobID          uuid.UUID
	JobType        string
	RunID          *uuid.UUID
	Type           string
	Status         string
	DurationMs     int64
	ErrorCategory  ErrorCategory
	ErrorReference string
}

type EventRecorder interface {
	RecordJobEvent(context.Context, *JobEvent) error
}

type Store interface {
	Insert(context.Context, *Job) (*Job, bool, error)
	Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*Job, error)
	FindByIdempotencyKey(context.Context, uuid.UUID, uuid.UUID, string) (*Job, error)
	List(context.Context, uuid.UUID, uuid.UUID, ListOptions) ([]Job, int, error)
	Claim(context.Context, string, time.Time, time.Time) (*Job, error)
	RenewLease(context.Context, uuid.UUID, string, time.Time) error
	Save(context.Context, *Job, string) error
	Cancel(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (*Job, error)
	Retry(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (*Job, error)
	PruneCompleted(context.Context, time.Time) (int64, error)
}

func cloneJob(job *Job) *Job {
	if job == nil {
		return nil
	}
	out := *job
	out.Payload = append(json.RawMessage(nil), job.Payload...)
	out.Result = append(json.RawMessage(nil), job.Result...)
	if job.RunID != nil {
		value := *job.RunID
		out.RunID = &value
	}
	if job.StartedAt != nil {
		value := *job.StartedAt
		out.StartedAt = &value
	}
	if job.CompletedAt != nil {
		value := *job.CompletedAt
		out.CompletedAt = &value
	}
	if job.CancelledAt != nil {
		value := *job.CancelledAt
		out.CancelledAt = &value
	}
	if job.LeaseExpiresAt != nil {
		value := *job.LeaseExpiresAt
		out.LeaseExpiresAt = &value
	}
	return &out
}

func IsTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled
}

func terminal(status string) bool { return IsTerminal(status) }
