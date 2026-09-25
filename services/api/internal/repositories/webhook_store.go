package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrWebhookDeliveryNotFound    = errors.New("webhook delivery not found")
	ErrWebhookTargetNotFound      = errors.New("webhook target not found")
	ErrWebhookPullRequestNotFound = errors.New("webhook pull request not found")
	ErrWebhookStoreNotConfigured  = errors.New("webhook store is not configured")
)

type WebhookActivity struct {
	RepositoryID      uuid.UUID  `json:"repository_id"`
	TargetID          uuid.UUID  `json:"id"`
	DeliveryID        uuid.UUID  `json:"delivery_id"`
	GitHubDeliveryID  string     `json:"github_delivery_id"`
	Event             string     `json:"event"`
	Action            string     `json:"action,omitempty"`
	Operation         string     `json:"operation"`
	Status            string     `json:"status"`
	IssueNumber       *int       `json:"issue_number,omitempty"`
	PullRequestNumber *int       `json:"pull_request_number,omitempty"`
	JobID             *uuid.UUID `json:"job_id,omitempty"`
	Attempts          int        `json:"attempts"`
	ErrorCategory     string     `json:"error_category,omitempty"`
	ErrorReference    string     `json:"error_reference,omitempty"`
	ReceivedAt        time.Time  `json:"received_at"`
	EnqueuedAt        *time.Time `json:"enqueued_at,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type WebhookStore interface {
	CreateWebhookDelivery(ctx context.Context, delivery *GitHubWebhookDelivery, targets []GitHubWebhookTarget) (*GitHubWebhookDelivery, bool, error)
	FindWebhookDelivery(ctx context.Context, deliveryID string) (*GitHubWebhookDelivery, error)
	ListSelectedRepositoriesByGithubRepositoryID(ctx context.Context, githubRepositoryID int64) ([]SelectedRepository, error)
	ListWebhookActivity(ctx context.Context, userID, selectedRepositoryID uuid.UUID, limit, offset int) ([]WebhookActivity, int, error)
	GetWebhookTarget(ctx context.Context, targetID uuid.UUID) (*GitHubWebhookTarget, error)
	FindPullRequestByHeadSHA(ctx context.Context, selectedRepositoryID uuid.UUID, headSHA string) (*RepositoryPullRequest, error)
	ListWebhookTargetsForReconciliation(ctx context.Context, now time.Time, limit int) ([]GitHubWebhookTarget, error)
	ClaimWebhookTarget(ctx context.Context, targetID uuid.UUID, owner string, now time.Time, leaseFor time.Duration) (*GitHubWebhookTarget, bool, error)
	MarkWebhookTargetEnqueued(ctx context.Context, targetID, jobID uuid.UUID, owner string, now time.Time) (bool, error)
	MarkWebhookTargetProcessing(ctx context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error)
	MarkWebhookTargetSucceeded(ctx context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error)
	MarkWebhookTargetFailed(ctx context.Context, targetID, jobID uuid.UUID, category, reference string, now time.Time) (bool, error)
	MarkWebhookTargetDispatchFailed(ctx context.Context, targetID uuid.UUID, owner, category, reference string, now time.Time) (bool, error)
	ResetWebhookTarget(ctx context.Context, targetID uuid.UUID, now time.Time) (bool, error)
	ReleaseWebhookTarget(ctx context.Context, targetID uuid.UUID, owner, category, reference string) (bool, error)
}

type GormWebhookStore struct {
	db *gorm.DB
}

func NewGormWebhookStore(db *gorm.DB) WebhookStore {
	return &GormWebhookStore{db: db}
}

func (s *GormWebhookStore) CreateWebhookDelivery(ctx context.Context, delivery *GitHubWebhookDelivery, targets []GitHubWebhookTarget) (*GitHubWebhookDelivery, bool, error) {
	if delivery.ID == uuid.Nil {
		delivery.ID = uuid.New()
	}
	if delivery.ReceivedAt.IsZero() {
		delivery.ReceivedAt = time.Now()
	}
	for i := range targets {
		if targets[i].ID == uuid.Nil {
			targets[i].ID = uuid.New()
		}
		targets[i].DeliveryID = delivery.ID
		if targets[i].Status == "" {
			targets[i].Status = WebhookTargetStatusReceived
		}
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(delivery).Error; err != nil {
			return err
		}
		if len(targets) == 0 {
			return nil
		}
		return tx.Create(&targets).Error
	})
	if err == nil {
		return delivery, false, nil
	}
	existing, findErr := s.FindWebhookDelivery(ctx, delivery.DeliveryID)
	if findErr == nil && existing != nil {
		return existing, true, nil
	}
	return nil, false, err
}

func (s *GormWebhookStore) FindWebhookDelivery(ctx context.Context, deliveryID string) (*GitHubWebhookDelivery, error) {
	var delivery GitHubWebhookDelivery
	if err := s.db.WithContext(ctx).Where("delivery_id = ?", deliveryID).First(&delivery).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWebhookDeliveryNotFound
		}
		return nil, err
	}
	return &delivery, nil
}

func (s *GormWebhookStore) ListSelectedRepositoriesByGithubRepositoryID(ctx context.Context, githubRepositoryID int64) ([]SelectedRepository, error) {
	var repositories []SelectedRepository
	if err := s.db.WithContext(ctx).
		Where("github_repository_id = ?", githubRepositoryID).
		Order("id ASC").
		Find(&repositories).Error; err != nil {
		return nil, err
	}
	return repositories, nil
}

func (s *GormWebhookStore) ListWebhookActivity(ctx context.Context, userID, selectedRepositoryID uuid.UUID, limit, offset int) ([]WebhookActivity, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	base := s.db.WithContext(ctx).
		Table("github_webhook_targets AS targets").
		Joins("JOIN github_webhook_deliveries AS deliveries ON deliveries.id = targets.delivery_id").
		Where("targets.user_id = ? AND targets.selected_repository_id = ?", userID, selectedRepositoryID)
	var count int64
	if err := base.Count(&count).Error; err != nil {
		return nil, 0, err
	}
	var activity []WebhookActivity
	if err := base.
		Select(`targets.selected_repository_id AS repository_id,
			targets.id AS target_id,
			targets.delivery_id AS delivery_id,
			deliveries.delivery_id AS github_delivery_id,
			deliveries.event,
			deliveries.action,
			targets.operation,
			targets.status,
			targets.issue_number,
			targets.pull_request_number,
			targets.job_id,
			targets.attempts,
			targets.error_category,
			targets.error_reference,
			deliveries.received_at,
			targets.enqueued_at,
			targets.started_at,
			targets.completed_at,
			targets.created_at,
			targets.updated_at`).
		Order("targets.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&activity).Error; err != nil {
		return nil, 0, err
	}
	return activity, int(count), nil
}

func (s *GormWebhookStore) GetWebhookTarget(ctx context.Context, targetID uuid.UUID) (*GitHubWebhookTarget, error) {
	var target GitHubWebhookTarget
	if err := s.db.WithContext(ctx).Where("id = ?", targetID).First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWebhookTargetNotFound
		}
		return nil, err
	}
	return &target, nil
}

func (s *GormWebhookStore) FindPullRequestByHeadSHA(ctx context.Context, selectedRepositoryID uuid.UUID, headSHA string) (*RepositoryPullRequest, error) {
	var pullRequest RepositoryPullRequest
	if err := s.db.WithContext(ctx).
		Where("selected_repository_id = ? AND head_sha = ?", selectedRepositoryID, headSHA).
		Order("updated_at DESC").
		First(&pullRequest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWebhookPullRequestNotFound
		}
		return nil, err
	}
	return &pullRequest, nil
}

func (s *GormWebhookStore) ListWebhookTargetsForReconciliation(ctx context.Context, now time.Time, limit int) ([]GitHubWebhookTarget, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var targets []GitHubWebhookTarget
	err := s.db.WithContext(ctx).
		Where("status IN ? OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?))",
			[]string{WebhookTargetStatusReceived, WebhookTargetStatusEnqueued, WebhookTargetStatusProcessing}, WebhookTargetStatusDispatching, now).
		Order("updated_at ASC").
		Limit(limit).
		Find(&targets).Error
	if err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *GormWebhookStore) ClaimWebhookTarget(ctx context.Context, targetID uuid.UUID, owner string, now time.Time, leaseFor time.Duration) (*GitHubWebhookTarget, bool, error) {
	var claimed GitHubWebhookTarget
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND (status = ? OR (status = ? AND (lease_expires_at IS NULL OR lease_expires_at <= ?)))",
				targetID, WebhookTargetStatusReceived, WebhookTargetStatusDispatching, now).
			First(&claimed).Error; err != nil {
			return err
		}
		leaseUntil := now.Add(leaseFor)
		if err := tx.Model(&GitHubWebhookTarget{}).Where("id = ?", targetID).Updates(map[string]any{
			"status":           WebhookTargetStatusDispatching,
			"attempts":         gorm.Expr("attempts + 1"),
			"lease_owner":      owner,
			"lease_expires_at": leaseUntil,
			"updated_at":       now,
		}).Error; err != nil {
			return err
		}
		claimed.Status = WebhookTargetStatusDispatching
		claimed.Attempts++
		claimed.LeaseOwner = owner
		claimed.LeaseExpiresAt = &leaseUntil
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &claimed, true, nil
}

func (s *GormWebhookStore) MarkWebhookTargetEnqueued(ctx context.Context, targetID, jobID uuid.UUID, owner string, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND status = ? AND lease_owner = ?", targetID, WebhookTargetStatusDispatching, owner).
		Updates(map[string]any{
			"status":           WebhookTargetStatusEnqueued,
			"job_id":           jobID,
			"enqueued_at":      now,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"error_category":   "",
			"error_reference":  "",
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) MarkWebhookTargetProcessing(ctx context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND job_id = ? AND status IN ?", targetID, jobID, []string{WebhookTargetStatusEnqueued, WebhookTargetStatusProcessing}).
		Updates(map[string]any{
			"status":           WebhookTargetStatusProcessing,
			"started_at":       gorm.Expr("COALESCE(started_at, ?)", now),
			"lease_owner":      "",
			"lease_expires_at": nil,
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) MarkWebhookTargetSucceeded(ctx context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND job_id = ? AND status IN ?", targetID, jobID, []string{WebhookTargetStatusEnqueued, WebhookTargetStatusProcessing, WebhookTargetStatusSucceeded}).
		Updates(map[string]any{
			"status":           WebhookTargetStatusSucceeded,
			"completed_at":     now,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"error_category":   "",
			"error_reference":  "",
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) MarkWebhookTargetFailed(ctx context.Context, targetID, jobID uuid.UUID, category, reference string, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND job_id = ? AND status IN ?", targetID, jobID, []string{WebhookTargetStatusEnqueued, WebhookTargetStatusProcessing, WebhookTargetStatusFailed}).
		Updates(map[string]any{
			"status":           WebhookTargetStatusFailed,
			"completed_at":     now,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"error_category":   category,
			"error_reference":  reference,
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) MarkWebhookTargetDispatchFailed(ctx context.Context, targetID uuid.UUID, owner, category, reference string, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND status = ? AND lease_owner = ?", targetID, WebhookTargetStatusDispatching, owner).
		Updates(map[string]any{
			"status":           WebhookTargetStatusFailed,
			"completed_at":     now,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"error_category":   category,
			"error_reference":  reference,
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) ResetWebhookTarget(ctx context.Context, targetID uuid.UUID, now time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND job_id IS NULL AND status IN ?", targetID, []string{WebhookTargetStatusEnqueued, WebhookTargetStatusProcessing}).
		Updates(map[string]any{
			"status":           WebhookTargetStatusDispatching,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"updated_at":       now,
		})
	return result.RowsAffected == 1, result.Error
}

func (s *GormWebhookStore) ReleaseWebhookTarget(ctx context.Context, targetID uuid.UUID, owner, category, reference string) (bool, error) {
	result := s.db.WithContext(ctx).Model(&GitHubWebhookTarget{}).
		Where("id = ? AND status IN ? AND lease_owner = ?", targetID, []string{WebhookTargetStatusReceived, WebhookTargetStatusDispatching, WebhookTargetStatusProcessing}, owner).
		Updates(map[string]any{
			"status":           WebhookTargetStatusDispatching,
			"job_id":           nil,
			"lease_owner":      "",
			"lease_expires_at": nil,
			"error_category":   category,
			"error_reference":  reference,
			"updated_at":       time.Now(),
		})
	return result.RowsAffected == 1, result.Error
}
