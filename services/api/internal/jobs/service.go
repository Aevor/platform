package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Options struct {
	Registry        *Registry
	Recorder        EventRecorder
	MaxPayloadBytes int
	MaxResultBytes  int
}

type Service struct {
	store           Store
	registry        *Registry
	recorder        EventRecorder
	maxPayloadBytes int
	maxResultBytes  int

	activeMu sync.Mutex
	active   map[uuid.UUID]context.CancelFunc
}

func NewService(store Store, options Options) (*Service, error) {
	if store == nil {
		return nil, errors.New("job store is required")
	}
	if options.Registry == nil {
		options.Registry = NewRegistry()
	}
	if options.MaxPayloadBytes <= 0 {
		options.MaxPayloadBytes = 64 * 1024
	}
	if options.MaxResultBytes <= 0 {
		options.MaxResultBytes = 64 * 1024
	}
	return &Service{
		store:           store,
		registry:        options.Registry,
		recorder:        options.Recorder,
		maxPayloadBytes: options.MaxPayloadBytes,
		maxResultBytes:  options.MaxResultBytes,
		active:          make(map[uuid.UUID]context.CancelFunc),
	}, nil
}

func (s *Service) Registry() *Registry { return s.registry }

func (s *Service) Enqueue(ctx context.Context, options EnqueueOptions) (*Job, error) {
	if options.UserID == uuid.Nil || options.SelectedRepositoryID == uuid.Nil || options.Type == "" {
		return nil, fmt.Errorf("%w: owner, repository and type are required", ErrInvalidPayload)
	}
	if len(options.IdempotencyKey) > 128 {
		return nil, fmt.Errorf("%w: idempotency key exceeds 128 characters", ErrInvalidPayload)
	}
	definition, err := s.registry.Get(options.Type)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(options.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: payload must be valid JSON", ErrInvalidPayload)
	}
	if len(payload) == 0 {
		payload = []byte("null")
	}
	if len(payload) > s.maxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}

	if definition.ValidatePayload != nil {
		validated, validationErr := definition.ValidatePayload(payload)
		if validationErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPayload, validationErr)
		}
		if validated != nil {
			payload, err = json.Marshal(validated)
			if err != nil {
				return nil, fmt.Errorf("%w: validated payload is not JSON", ErrInvalidPayload)
			}
		}
	}
	if len(payload) > s.maxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}

	maxAttempts := options.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = definition.MaxAttempts
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	now := time.Now().UTC()
	job := &Job{
		ID:                   uuid.New(),
		UserID:               options.UserID,
		SelectedRepositoryID: options.SelectedRepositoryID,
		RunID:                options.RunID,
		Type:                 options.Type,
		Payload:              append(json.RawMessage(nil), payload...),
		Status:               StatusQueued,
		Priority:             options.Priority,
		IdempotencyKey:       options.IdempotencyKey,
		MaxAttempts:          maxAttempts,
		ScheduledAt:          now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	stored, inserted, err := s.store.Insert(ctx, job)
	if err != nil {
		return nil, err
	}
	if inserted {
		s.record(ctx, stored, "JOB_ENQUEUED", "success", 0, "", "")
	}
	return stored, nil
}

func (s *Service) Get(ctx context.Context, userID, repositoryID, jobID uuid.UUID) (*Job, error) {
	return s.store.Get(ctx, userID, repositoryID, jobID)
}

func (s *Service) FindByIdempotencyKey(ctx context.Context, userID, repositoryID uuid.UUID, key string) (*Job, error) {
	return s.store.FindByIdempotencyKey(ctx, userID, repositoryID, key)
}

func (s *Service) List(ctx context.Context, userID, repositoryID uuid.UUID, options ListOptions) ([]Job, int, error) {
	if options.Limit < 0 || options.Offset < 0 {
		return nil, 0, ErrInvalidPayload
	}
	return s.store.List(ctx, userID, repositoryID, options)
}

func (s *Service) Cancel(ctx context.Context, userID, repositoryID, jobID uuid.UUID) (*Job, error) {
	job, err := s.store.Cancel(ctx, userID, repositoryID, jobID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if job.Status == StatusCancelRequested {
		s.activeMu.Lock()
		cancel := s.active[jobID]
		s.activeMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	if job.Status == StatusCancelled {
		s.record(ctx, job, "JOB_CANCELLED", "success", 0, "", "")
	} else {
		s.record(ctx, job, "JOB_CANCEL_REQUESTED", "success", 0, "", "")
	}
	return job, nil
}

func (s *Service) Retry(ctx context.Context, userID, repositoryID, jobID uuid.UUID) (*Job, error) {
	job, err := s.store.Retry(ctx, userID, repositoryID, jobID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	s.record(ctx, job, "JOB_RETRIED", "success", 0, "", "")
	return job, nil
}

func (s *Service) registerActive(id uuid.UUID, cancel context.CancelFunc) {
	s.activeMu.Lock()
	s.active[id] = cancel
	s.activeMu.Unlock()
}

func (s *Service) unregisterActive(id uuid.UUID) {
	s.activeMu.Lock()
	delete(s.active, id)
	s.activeMu.Unlock()
}

func (s *Service) record(ctx context.Context, job *Job, eventType, status string, duration int64, category ErrorCategory, reference string) {
	if s.recorder == nil || job == nil {
		return
	}
	_ = s.recorder.RecordJobEvent(ctx, &JobEvent{
		JobID:          job.ID,
		JobType:        job.Type,
		RunID:          job.RunID,
		Type:           eventType,
		Status:         status,
		DurationMs:     duration,
		ErrorCategory:  category,
		ErrorReference: reference,
	})
}

type Executor struct {
	mu             sync.Mutex
	stage          string
	result         json.RawMessage
	maxResultBytes int
	resultTooLarge bool
}

func newExecutor(maxResultBytes int) *Executor {
	return &Executor{maxResultBytes: maxResultBytes}
}

func (e *Executor) SetStage(stage string) {
	if len(stage) > 64 {
		stage = stage[:64]
	}
	e.mu.Lock()
	e.stage = stage
	e.mu.Unlock()
}

func (e *Executor) SetResult(value any) {
	raw, err := json.Marshal(value)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil || len(raw) > e.maxResultBytes {
		e.result = nil
		e.resultTooLarge = len(raw) > e.maxResultBytes
		return
	}
	e.result = append(json.RawMessage(nil), raw...)
}

func (e *Executor) snapshot() (string, json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stage, append(json.RawMessage(nil), e.result...)
}
