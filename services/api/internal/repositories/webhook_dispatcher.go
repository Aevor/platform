package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/Aevor/platform/services/api/internal/jobs"
)

const (
	webhookDispatchInterval    = 2 * time.Second
	webhookDispatchLease       = 2 * time.Minute
	webhookDispatchBatch       = 30
	webhookDispatchMaxAttempts = 10
)

type WebhookDispatcher struct {
	store      WebhookStore
	jobs       *jobs.Service
	instanceID string
	now        func() time.Time
}

func NewWebhookDispatcher(store WebhookStore, jobsService *jobs.Service, instanceID string) *WebhookDispatcher {
	return &WebhookDispatcher{
		store:      store,
		jobs:       jobsService,
		instanceID: instanceID,
		now:        time.Now,
	}
}

func (d *WebhookDispatcher) Run(ctx context.Context) {
	if d == nil || d.store == nil || d.jobs == nil || d.instanceID == "" {
		return
	}
	if err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
		d.logDispatchError()
	}
	ticker := time.NewTicker(webhookDispatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
				d.logDispatchError()
			}
		}
	}
}

func (d *WebhookDispatcher) DispatchOnce(ctx context.Context) error {
	targets, err := d.store.ListWebhookTargetsForReconciliation(ctx, d.now(), webhookDispatchBatch)
	if err != nil {
		return err
	}
	var firstErr error
	for i := range targets {
		if err := d.processTarget(ctx, &targets[i]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (d *WebhookDispatcher) processTarget(ctx context.Context, snapshot *GitHubWebhookTarget) error {
	if snapshot.Status == WebhookTargetStatusEnqueued || snapshot.Status == WebhookTargetStatusProcessing {
		return d.reconcileTarget(ctx, snapshot)
	}
	target, claimed, err := d.store.ClaimWebhookTarget(ctx, snapshot.ID, d.instanceID, d.now(), webhookDispatchLease)
	if err != nil || !claimed {
		return err
	}
	if target.Attempts > webhookDispatchMaxAttempts {
		_, markErr := d.store.MarkWebhookTargetDispatchFailed(ctx, target.ID, d.instanceID, ErrCategoryUnknown, "dispatch_attempts_exhausted", d.now())
		return markErr
	}

	idempotencyKey := "webhook-target:" + target.ID.String()
	job, err := d.jobs.FindByIdempotencyKey(ctx, target.UserID, target.SelectedRepositoryID, idempotencyKey)
	if err != nil {
		_, _ = d.store.ReleaseWebhookTarget(ctx, target.ID, d.instanceID, ErrCategoryDatabase, "dispatch_lookup_failed")
		return err
	}
	if job == nil {
		job, err = d.jobs.Enqueue(ctx, jobs.EnqueueOptions{
			UserID:               target.UserID,
			SelectedRepositoryID: target.SelectedRepositoryID,
			Type:                 JobTypeRepositorySync,
			Payload:              WebhookSyncPayload{TargetID: target.ID},
			IdempotencyKey:       idempotencyKey,
		})
		if err != nil {
			_, _ = d.store.ReleaseWebhookTarget(ctx, target.ID, d.instanceID, ErrCategoryDatabase, "enqueue_failed")
			return err
		}
	}
	if jobs.IsTerminal(job.Status) {
		marked, markErr := d.store.MarkWebhookTargetEnqueued(ctx, target.ID, job.ID, d.instanceID, d.now())
		if markErr != nil {
			return markErr
		}
		if !marked {
			return nil
		}
		return d.recordTerminalJob(ctx, target, job)
	}
	_, err = d.store.MarkWebhookTargetEnqueued(ctx, target.ID, job.ID, d.instanceID, d.now())
	return err
}

func (d *WebhookDispatcher) reconcileTarget(ctx context.Context, target *GitHubWebhookTarget) error {
	if target.JobID == nil {
		_, err := d.store.ResetWebhookTarget(ctx, target.ID, d.now())
		return err
	}
	job, err := d.jobs.FindByIdempotencyKey(ctx, target.UserID, target.SelectedRepositoryID, "webhook-target:"+target.ID.String())
	if errors.Is(err, jobs.ErrNotFound) {
		_, resetErr := d.store.ResetWebhookTarget(ctx, target.ID, d.now())
		return resetErr
	}
	if err != nil {
		return err
	}
	if job == nil {
		_, resetErr := d.store.ResetWebhookTarget(ctx, target.ID, d.now())
		return resetErr
	}
	if !jobs.IsTerminal(job.Status) {
		return nil
	}
	return d.recordTerminalJob(ctx, target, job)
}

func (d *WebhookDispatcher) recordTerminalJob(ctx context.Context, target *GitHubWebhookTarget, job *jobs.Job) error {
	now := d.now()
	if job.Status == jobs.StatusSucceeded {
		_, err := d.store.MarkWebhookTargetSucceeded(ctx, target.ID, job.ID, now)
		return err
	}
	category := job.ErrorCategory
	if category == "" {
		category = string(jobs.CategoryInternalFailure)
	}
	reference := job.ErrorReference
	if reference == "" {
		reference = "job_" + job.Status
	}
	_, err := d.store.MarkWebhookTargetFailed(ctx, target.ID, job.ID, string(category), reference, now)
	return err
}

func (d *WebhookDispatcher) logDispatchError() {
}
