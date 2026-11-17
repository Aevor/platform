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

	startOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	started    bool
	shutdown   bool
}

func NewWorker(service *Service, workerID string, options WorkerOptions) *Worker {
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 2 * time.Minute
	}
	// A lease must be long enough for heartbeat cadence, scheduler jitter,
	// and a short database round trip. Tiny test leases are clamped as well.
	if options.LeaseDuration < 300*time.Millisecond {
		options.LeaseDuration = 300 * time.Millisecond
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 10
	}
	if options.RetryBaseDelay <= 0 {
		options.RetryBaseDelay = 5 * time.Second
	}
	if options.RetryMaxDelay <= 0 {
		options.RetryMaxDelay = 5 * time.Minute
	}
	if options.Retention <= 0 {
		options.Retention = 30 * 24 * time.Hour
	}
	if workerID == "" {
		workerID = uuid.NewString()
	}
	return &Worker{service: service, workerID: workerID, options: options, done: make(chan struct{})}
}

// Start runs the polling loop until Shutdown is called. Start/Shutdown are
// safe even when Shutdown wins the race and is called before Start.
func (w *Worker) Start() {
	w.startOnce.Do(func() {
		w.mu.Lock()
		if w.shutdown {
			w.mu.Unlock()
			w.finishOnce.Do(func() { close(w.done) })
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		w.started = true
		w.mu.Unlock()

		defer w.finishOnce.Do(func() { close(w.done) })
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
	w.shutdown = true
	cancel := w.cancel
	started := w.started
	w.mu.Unlock()
	if !started {
		w.finishOnce.Do(func() { close(w.done) })
		return
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-w.done:
	case <-ctx.Done():
	}
}

func (w *Worker) ProcessNow(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = w.options.BatchSize
	}
	processed := 0
	for processed < limit {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		now := time.Now().UTC()
		job, err := w.service.store.Claim(ctx, w.workerID, now, now.Add(w.options.LeaseDuration))
		if err != nil {
			return processed, err
		}
		if job == nil {
			break
		}
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

	// Close the gap between a row being claimed and the active cancellation
	// callback being registered. Other API instances are observed by heartbeat.
	checkCtx, checkCancel := context.WithTimeout(parent, 2*time.Second)
	latest, checkErr := w.service.store.Get(checkCtx, job.UserID, job.SelectedRepositoryID, job.ID)
	checkCancel()
	if checkErr != nil {
		return checkErr
	}
	if latest.Status == StatusCancelRequested {
		cancelRun()
	}

	started := time.Now()
	w.service.record(parent, job, "JOB_STARTED", "started", 0, "", "")

	heartbeatDone := make(chan struct{})
	heartbeatLost := make(chan struct{}, 1)
	go w.heartbeat(runCtx, cancelRun, job, heartbeatDone, heartbeatLost)

	executor := newExecutor(w.service.maxResultBytes)
	runErr := executeSafely(runCtx, definition, cloneJob(job), executor)
	close(heartbeatDone)
	select {
	case <-heartbeatLost:
		return ErrLeaseLost
	default:
	}

	now := time.Now().UTC()
	job.CurrentStage, job.Result, err = executor.snapshot()
	if err != nil && runErr == nil {
		runErr = err
	}
	job.UpdatedAt = now

	// A handler that ignores its context cannot convert a timed-out execution
	// into success merely by returning nil after its deadline.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		runErr = context.DeadlineExceeded
	}
	if parent.Err() != nil {
		// A server shutdown leaves the lease intact. It will expire and be
		// reclaimed by another process; a stale worker does not write results.
		return nil
	}

	lookupCtx, lookupCancel := context.WithTimeout(parent, 2*time.Second)
	latest, latestErr := w.service.store.Get(lookupCtx, job.UserID, job.SelectedRepositoryID, job.ID)
	lookupCancel()
	if latestErr != nil {
		return latestErr
	}
	explicitlyCancelled := latest.Status == StatusCancelRequested

	job.LeaseOwner = ""
	job.LeaseExpiresAt = nil
	if explicitlyCancelled || errors.Is(runCtx.Err(), context.Canceled) {
		job.Status = StatusCancelled
		job.CancelledAt = timePtr(now)
		job.CompletedAt = timePtr(now)
		job.ErrorCategory = ""
		job.ErrorReference = ""
	} else if runErr == nil {
		job.Status = StatusSucceeded
		job.CompletedAt = timePtr(now)
		job.ErrorCategory = ""
		job.ErrorReference = ""
	} else {
		category := CategoryInternalFailure
		if errors.Is(runErr, context.DeadlineExceeded) {
			category = CategoryTimeout
		} else if definition.Classify != nil {
			category = definition.Classify(runErr)
		}
		if category == "" {
			category = CategoryInternalFailure
		}
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

	if err := w.save(job); err != nil {
		return err
	}
	duration := time.Since(started).Milliseconds()
	switch job.Status {
	case StatusSucceeded:
		w.service.record(parent, job, "JOB_COMPLETED", "success", duration, "", "")
	case StatusRetrying:
		w.service.record(parent, job, "JOB_RETRY_SCHEDULED", "failure", duration, job.ErrorCategory, job.ErrorReference)
	case StatusFailed:
		w.service.record(parent, job, "JOB_FAILED", "failure", duration, job.ErrorCategory, job.ErrorReference)
	default:
		w.service.record(parent, job, "JOB_CANCELLED", "success", duration, "", "")
	}
	return nil
}

func executeSafely(ctx context.Context, definition Definition, job *Job, executor *Executor) (err error) {
	defer func() {
		if recover() != nil {
			// Never persist panic values: they can contain secrets or arbitrary
			// user-controlled data. The worker stores a stable internal code.
			err = errors.New("job handler panicked")
		}
	}()
	return definition.Run(ctx, job, executor)
}

func (w *Worker) heartbeat(ctx context.Context, cancelRun context.CancelFunc, job *Job, done <-chan struct{}, lost chan<- struct{}) {
	interval := w.options.LeaseDuration / 3
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			err := w.service.store.RenewLease(ctx, job.ID, w.workerID, time.Now().UTC().Add(w.options.LeaseDuration))
			if err != nil {
				cancelRun()
				select {
				case lost <- struct{}{}:
				default:
				}
				return
			}
			lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			current, getErr := w.service.store.Get(lookupCtx, job.UserID, job.SelectedRepositoryID, job.ID)
			cancel()
			if getErr != nil {
				cancelRun()
				select {
				case lost <- struct{}{}:
				default:
				}
				return
			}
			if current.Status == StatusCancelRequested {
				cancelRun()
				return
			}
		}
	}
}

func (w *Worker) retryDelay(attempt int) time.Duration {
	power := math.Pow(2, float64(max(0, attempt-1)))
	delay := time.Duration(float64(w.options.RetryBaseDelay) * power)
	if delay > w.options.RetryMaxDelay {
		return w.options.RetryMaxDelay
	}
	return delay
}

func safeErrorReference(category ErrorCategory) string {
	switch category {
	case CategoryAIServiceFailure:
		return "ai_service_failure"
	case CategoryTimeout:
		return "operation_timeout"
	case CategoryTransientExternalError:
		return "external_service_failure"
	case CategoryDatabaseFailure:
		return "database_operation_failed"
	case CategoryAuthorizationError:
		return "authorization_failed"
	case CategoryValidationError:
		return "validation_failed"
	case CategoryUserError:
		return "job_input_rejected"
	default:
		return "job_execution_failed"
	}
}

func (w *Worker) save(job *Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return w.service.store.Save(ctx, job, w.workerID)
}
