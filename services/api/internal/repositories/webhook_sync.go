package repositories

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (s *Service) ProcessWebhookTarget(ctx context.Context, targetID uuid.UUID) (WebhookSyncResult, error) {
	if s.webhookStore == nil {
		return WebhookSyncResult{}, errors.New("webhook store is not configured")
	}
	target, err := s.webhookStore.GetWebhookTarget(ctx, targetID)
	if err != nil {
		return WebhookSyncResult{}, err
	}
	result := WebhookSyncResult{TargetID: target.ID, Operation: target.Operation}
	switch target.Operation {
	case WebhookOperationIssues:
		sync, err := s.SyncIssues(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		result.Synced = sync.Synced
	case WebhookOperationPullRequests:
		sync, err := s.SyncPullRequests(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		result.Synced = sync.Synced
	case WebhookOperationCommits:
		sync, err := s.SyncCommits(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		result.Synced = sync.Synced
	case WebhookOperationAll:
		issues, err := s.SyncIssues(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		pullRequests, err := s.SyncPullRequests(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		commits, err := s.SyncCommits(ctx, target.UserID, target.SelectedRepositoryID)
		if err != nil {
			return result, err
		}
		result.Synced = issues.Synced + pullRequests.Synced + commits.Synced
	case WebhookOperationFeedback:
		number := target.PullRequestNumber
		if number == nil && target.HeadSHA != "" {
			pullRequest, findErr := s.webhookStore.FindPullRequestByHeadSHA(ctx, target.SelectedRepositoryID, target.HeadSHA)
			if errors.Is(findErr, ErrWebhookPullRequestNotFound) {
				return result, nil
			}
			if findErr != nil {
				return result, findErr
			}
			number = &pullRequest.Number
		}
		if number != nil {
			result.PullRequest = *number
			if _, err := s.AnalyzePullRequestFeedback(ctx, target.UserID, target.SelectedRepositoryID, *number); err != nil {
				return result, err
			}
		}
	default:
		return result, errors.New("unsupported webhook operation")
	}
	return result, nil
}
