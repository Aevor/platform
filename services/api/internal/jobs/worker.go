package jobs

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

type WorkerOptions struct {
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	BatchSize      int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	Retention      time.Duration
}

type Worker struct {
	service  *Service
	workerID string
	options  WorkerOptions

	startOnce sync.Once
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewWorker(service *Service, workerID string, options WorkerOptions) *Worker {
	if options.PollInterval <= 0 { options.PollInterval = time.Second }
	if options.LeaseDuration <= 0 { options.LeaseDuration = 2 * time.Minute }
	if options.BatchSize <= 0 { options.BatchSize = 10 }
	if options.RetryBaseDelay <= 0 { options.RetryBaseDelay = 5 * time.Second }
	if options.RetryMaxDelay <= 0 { options.RetryMaxDelay = 5 * time.Minute }
	if options.Retention <= 0 { options.Retention = 30 * 24 * time.Hour }
	if workerID == "" { workerID = uuid.NewString() }
	return &Worker{service: service, workerID: workerID, options: options, done: make(chan struct{})}
}

// Start runs the polling loop until Shutdown is called. It is safe to call
// Start once from the server's background goroutine.
func (w *Worker) Start() {
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		defer close(w.done)

		lastPrune := time.Time{}
		ticker := time.NewTicker(w.options.PollInterval)
		defer ticker.Stop()
		for {
			_, _ = w.ProcessNow(ctx, w.options.BatchSize)
			if lastPrune.IsZero() || time.Since(lastPrune) >= 24*time.Hour {
				pruneCtx, pruneCancel := context.WithTimeout(ctx, 10*time.Second)
				_, _ = w.service.store.PruneCompleted(pruneCtx, time.Now().UTC().Add(-w.options.Retention))
				pruneCancel()
				lastPrune = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (w *Worker) Shutdown(ctx context.Context) {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil { cancel() }
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *Worker) ProcessNow(ctx context.Context, limit int) (int, error) {
	if limit <= 0 { limit = w.options.BatchSize }
	processed := 0
	for processed < limit {
		if err := ctx.Err(); err != nil { return processed, err }
		now := time.Now().UTC()
		job, err := w.service.store.Claim(ctx, w.workerID, now, now.Add(w.options.LeaseDuration))
		if err != nil { return processed, err }
		if job == nil { break }
		processed++
		if err := w.runJob(ctx, job); err != nil && !errors.Is(err, ErrLeaseLost) {
			return processed, err
		}
	}
	return processed, nil
}

func (w *Worker) runJob(parent context.Context, job *Job) error {
	definition, err := w.service.registry.Get(job.Type)
	if err != nil {
		job.Status = StatusFailed
		job.CompletedAt = timePtr(time.Now().UTC())
		job.ErrorCategory = CategoryValidationError
		job.ErrorReference = "job_definition_missing"
		job.LeaseOwner = ""
		return w.save(job)
	}

	runCtx := parent
	cancelTimeout := func() {}
	if definition.Timeout > 0 {
		runCtx, cancelTimeout = context.WithTimeout(parent, definition.Timeout)
	}
	runCtx, cancelRun := context.WithCancel(runCtx)
	defer cancelTimeout()
	defer cancelRun()
	w.service.registerActive(job.ID, cancelRun)
	defer w.service.unregisterActive(job.ID)

	started := time.Now()
	w.service.record(parent, job, "JOB_STARTED", "started", 0, "", "")

	heartbeatDone := make(chan struct{})
	heartbeatLost := make(chan struct{}, 1)
	go w.heartbeat(runCtx, job.ID, heartbeatDone, heartbeatLost)
	executor := newExecutor(w.service.maxResultBytes)
	runErr := definition.Run(runCtx, cloneJob(job), executor)
	close(heartbeatDone)
	select {
	case <-heartbeatLost:
		return ErrLeaseLost
	default:
	}

	now := time.Now().UTC()
	job.CurrentStage, job.Result = executor.snapshot()
	job.UpdatedAt = now
	job.LeaseOwner = ""
	job.LeaseExpiresAt = nil

	if runErr == nil {
		job.Status = StatusSucceeded
		job.CompletedAt = timePtr(now)
		job.ErrorCategory = ""
		job.ErrorReference = ""
	} else if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled) {
		job.Status = StatusCancelled
		job.CancelledAt = timePtr(now)
		job.CompletedAt = timePtr(now)
		job.ErrorCategory = ""
		job.ErrorReference = ""
	} else {
		category := CategoryInternalFailure
		if definition.Classify != nil { category = definition.Classify(runErr) }
		if category == "" { category = CategoryInternalFailure }
		job.ErrorCategory = category
		job.ErrorReference = safeErrorReference(category)
		if category.Retryable() && job.Attempts < job.MaxAttempts {
			job.Status = StatusRetrying
			job.ScheduledAt = now.Add(w.retryDelay(job.Attempts))
		} else {
			job.Status = StatusFailed
			job.CompletedAt = timePtr(now)
		}
	}

	if err := w.save(job); err != nil { return err }
	duration := time.Since(started).Milliseconds()
	if job.Status == StatusSucceeded {
		w.service.record(parent, job, "JOB_COMPLETED", "success", duration, "", "")
	} else if job.Status == StatusRetrying {
		w.service.record(parent, job, "JOB_RETRY_SCHEDULED", "failure", duration, job.ErrorCategory, job.ErrorReference)
	} else if job.Status == StatusFailed {
		w.service.record(parent, job, "JOB_FAILED", "failure", duration, job.ErrorCategory, job.ErrorReference)
	} else {
		w.service.record(parent, job, "JOB_CANCELLED", "success", duration, "", "")
	}
	return nil
}

func (w *Worker) heartbeat(ctx context.Context, jobID uuid.UUID, done <-chan struct{}, lost chan<- struct{}) {
	interval := w.options.LeaseDuration / 3
	if interval < time.Second { interval = time.Second }
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			err := w.service.store.RenewLease(ctx, jobID, w.workerID, time.Now().UTC().Add(w.options.LeaseDuration))
			if err != nil {
				select { case lost <- struct{}{}: default: }
				return
			}
		}
	}
}

func (w *Worker) retryDelay(attempt int) time.Duration {
	power := math.Pow(2, float64(max(0, attempt-1)))
	delay := time.Duration(float64(w.options.RetryBaseDelay) * power)
	if delay > w.options.RetryMaxDelay { return w.options.RetryMaxDelay }
	return delay
}

func safeErrorReference(category ErrorCategory) string {
	switch category {
	case CategoryAIServiceFailure: return "ai_service_failure"
	case CategoryTimeout: return "operation_timeout"
	case CategoryTransientExternalError: return "external_service_failure"
	case CategoryDatabaseFailure: return "database_operation_failed"
	case CategoryAuthorizationError: return "authorization_failed"
	case CategoryValidationError: return "validation_failed"
	case CategoryUserError: return "job_input_rejected"
	default: return "job_execution_failed"
	}
}

func (w *Worker) save(job *Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.service.store.Save(ctx, job, w.workerID)
}
