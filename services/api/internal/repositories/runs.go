package repositories

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// ErrRunNotFound is returned when an engineering run either does not exist or
// does not belong to the requesting user's repository context. It stays opaque
// so foreign run existence cannot be probed.
var ErrRunNotFound = errors.New("engineering run not found")

// The engineering-run lifecycle. A run is the traceable lifecycle of ONE
// issue attempt: created at the first analysis, advanced through every stage
// of the pipeline (analysis, planning, generation, review/approval, apply,
// validation, delivery), and recorded as complete when the pull request is
// delivered and accepted. Failure statuses preserve the stage that failed so
// the run history pinpoints exactly where an attempt broke.
const (
	RunStatusCreated    = "CREATED"
	RunStatusAnalyzing  = "ANALYZING"
	RunStatusPlanning   = "PLANNING"
	RunStatusGenerating = "GENERATING"
	RunStatusReviewing  = "REVIEWING"
	RunStatusApproved   = "APPROVED"
	RunStatusApplying   = "APPLYING"
	RunStatusValidating = "VALIDATING"
	RunStatusDelivering = "DELIVERING"
	RunStatusPrOpen     = "PR_OPEN"
	RunStatusIterating  = "ITERATING"
	RunStatusCompleted  = "COMPLETED"

	RunStatusFailedAnalysis   = "FAILED_ANALYSIS"
	RunStatusFailedGeneration = "FAILED_GENERATION"
	RunStatusFailedValidation = "FAILED_VALIDATION"
	RunStatusFailedDelivery   = "FAILED_DELIVERY"
)

// Failure statuses, listed by the stage they belong to. Every pipeline stage
// maps to exactly one failure status so the run's failure_stage stays honest.
var runFailureStatusByStage = map[string]string{
	"ANALYZING":  RunStatusFailedAnalysis,
	"PLANNING":   RunStatusFailedGeneration,
	"GENERATING": RunStatusFailedGeneration,
	"VALIDATING": RunStatusFailedValidation,
	"DELIVERING": RunStatusFailedDelivery,
}

// The engineering-event classifications in the audit trail. Types are the
// meaningful, non-trivial transitions of the pipeline (never internal helper
// calls); Status is started/success/failure; Stage is the pipeline stage the
// event belongs to.
const (
	EventRunCreated             = "RUN_CREATED"
	EventIssueAnalysisStarted   = "ISSUE_ANALYSIS_STARTED"
	EventIssueAnalysisCompleted = "ISSUE_ANALYSIS_COMPLETED"

	EventSolutionGenerationStarted   = "SOLUTION_GENERATION_STARTED"
	EventSolutionGenerationCompleted = "SOLUTION_GENERATION_COMPLETED"

	EventCodeGenerationStarted   = "CODE_GENERATION_STARTED"
	EventCodeGenerationCompleted = "CODE_GENERATION_COMPLETED"
	EventImpactAnalysisCompleted = "IMPACT_ANALYSIS_COMPLETED"

	EventChangesApproved = "CHANGES_APPROVED"
	EventChangesApplied  = "CHANGES_APPLIED"

	EventValidationStarted   = "VALIDATION_STARTED"
	EventValidationCompleted = "VALIDATION_COMPLETED"

	EventBranchCreated      = "BRANCH_CREATED"
	EventCommitCreated      = "COMMIT_CREATED"
	EventPushCompleted      = "PUSH_COMPLETED"
	EventPullRequestCreated = "PR_CREATED"

	EventPullRequestFeedbackReceived = "PR_FEEDBACK_RECEIVED"
	EventIterationStarted            = "ITERATION_STARTED"

	EventRunCompleted = "RUN_COMPLETED"
	EventRunFailed    = "RUN_FAILED"
)

// Event statuses. Started events open a stage, success/failure close it with
// the meaningful outcome.
const (
	EventStatusStarted = "started"
	EventStatusSuccess = "success"
	EventStatusFailure = "failure"
)

// Event error categories. They are the SAFE, coarse classification of where an
// attempt failed; raw error text (which may embed tokens, paths, or URLs) is
// never stored as an event detail.
const (
	ErrCategoryAuthorization = "AUTHORIZATION"
	ErrCategoryValidation    = "VALIDATION"
	ErrCategoryAIService     = "AI_SERVICE"
	ErrCategoryGitHub        = "GITHUB"
	ErrCategoryGit           = "GIT"
	ErrCategoryWorkspace     = "WORKSPACE"
	ErrCategoryDatabase      = "DATABASE"
	ErrCategoryTimeout       = "TIMEOUT"
	ErrCategoryDependency    = "DEPENDENCY"
	ErrCategoryUnknown       = "UNKNOWN"
)

// EngineeringRun is the durable trace of one engineering attempt on one issue
// for one selected-repository context. Only the event stream carries per-step
// detail; the run row holds the current lifecycle position, the failure stage
// (if any), the linked change set and pull request identities, and a bounded
// metadata envelope. Ownership is enforced through the selected repository the
// same way as every other pipeline entity.
type EngineeringRun struct {
	ID                   uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID               uuid.UUID  `gorm:"type:uuid;index:idx_engineering_runs_user;not null" json:"-"`
	SelectedRepositoryID uuid.UUID  `gorm:"type:uuid;index:idx_engineering_runs_repo;not null" json:"-"`
	RepositoryIssueID    uuid.UUID  `gorm:"type:uuid;index:idx_engineering_runs_issue;not null" json:"-"`
	ChangeSetID          *uuid.UUID `gorm:"type:uuid" json:"change_set_id"`
	PullRequestNumber    *int       `gorm:"type:integer" json:"pull_request_number"`

	Status       string `gorm:"size:32;index:idx_engineering_runs_status;not null" json:"status"`
	CurrentStage string `gorm:"size:32;not null" json:"current_stage"`
	FailureStage string `gorm:"size:32" json:"failure_stage,omitempty"`

	Metadata string `gorm:"type:text" json:"-"`

	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EngineeringRun) TableName() string {
	return "engineering_runs"
}

func (r *EngineeringRun) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	return nil
}

// EngineeringEvent is one meaningful transition in an engineering run's audit
// trail: WHO owned the repository, WHEN the event happened, WHAT the transition
// was, WHERE (stage/status) the run sat, and the RESULT (duration plus the
// coarse failure category when the transition failed). Metadata is a bounded,
// whitelisted JSON envelope — never raw AI output, source content, or secrets.
type EngineeringEvent struct {
	ID                   uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	RunID                uuid.UUID `gorm:"type:uuid;index:idx_engineering_events_run;not null" json:"-"`
	UserID               uuid.UUID `gorm:"type:uuid;index:idx_engineering_events_user;not null" json:"-"`
	SelectedRepositoryID uuid.UUID `gorm:"type:uuid;index:idx_engineering_events_repo;not null" json:"-"`
	RepositoryIssueID    uuid.UUID `gorm:"type:uuid;index:idx_engineering_events_issue;not null" json:"-"`

	Type   string `gorm:"size:64;index:idx_engineering_events_type;not null" json:"type"`
	Stage  string `gorm:"size:32" json:"stage,omitempty"`
	Status string `gorm:"size:16;index:idx_engineering_events_status;not null" json:"status"`

	Timestamp time.Time `json:"timestamp"`
	Duration  int64     `json:"duration_ms,omitempty"`

	Metadata string `gorm:"type:text" json:"-"`

	ErrorCategory  string `gorm:"size:32" json:"error_category,omitempty"`
	ErrorReference string `gorm:"size:255" json:"error_reference,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (EngineeringEvent) TableName() string {
	return "engineering_events"
}

func (e *EngineeringEvent) BeforeCreate(tx *gorm.DB) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return nil
}

// Event metadata envelope. Keys are whitelisted at construction; any attacker-
// controlled free text is deliberately excluded. Values are bounded at write.
type runMetadata struct {
	InitiatedBy string `json:"initiated_by,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
}

type aiOperationMetadata struct {
	Operation     string `json:"operation,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
	Success       bool   `json:"success"`
	ContextChunks int    `json:"context_chunks,omitempty"`
	ResponseValid bool   `json:"response_valid"`
}

// marshalRunMetadata serializes a bounded metadata envelope, returning an
// empty string when there is nothing to store.
func marshalRunMetadata(value any) string {
	if value == nil {
		return ""
	}
	raw, err := json.Marshal(value)
	if err != nil || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// parseRunMetadata parses a stored metadata envelope back into a generic map.
// It is used to surface ONLY the whitelisted bounded fields on the API. On any
// parse failure it returns nil so a corrupted envelope never breaks a read.
func parseRunMetadata(stored string) map[string]any {
	if strings.TrimSpace(stored) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return nil
	}
	return out
}

// stageErrorCategory classifies a stage failure into the SAFE coarse category
// plus whether that specific failure may be retried. It is the ONLY place
// where an arbitrary pipeline error is reduced to an audit-field-safe summary;
// the full error is never stored on the event.
func stageErrorCategory(err error) (category string, retryable bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ai.ErrUnavailable):
		return ErrCategoryAIService, true
	case errors.Is(err, ai.ErrTimeout):
		return ErrCategoryTimeout, true
	case errors.Is(err, ai.ErrRateLimited):
		return ErrCategoryAIService, true
	case errors.Is(err, ai.ErrUnauthorized):
		return ErrCategoryAuthorization, false
	case errors.Is(err, ai.ErrInvalidResponse):
		return ErrCategoryAIService, false
	case errors.Is(err, ai.ErrRejected):
		return ErrCategoryValidation, false
	case errors.Is(err, github.ErrUnauthorized):
		return ErrCategoryAuthorization, false
	case errors.Is(err, github.ErrRateLimited):
		return ErrCategoryGitHub, true
	case errors.Is(err, github.ErrUnavailable):
		return ErrCategoryGitHub, true
	case errors.Is(err, github.ErrInvalidResponse):
		return ErrCategoryGitHub, false
	case errors.Is(err, github.ErrAPIError):
		return ErrCategoryGitHub, true
	case errors.Is(err, workspace.ErrCloneFailed):
		return ErrCategoryGit, true
	case errors.Is(err, workspace.ErrTimeout):
		return ErrCategoryTimeout, true
	case errors.Is(err, workspace.ErrInvalidCloneURL):
		return ErrCategoryValidation, false
	case errors.Is(err, workspace.ErrAuthRejected):
		return ErrCategoryAuthorization, false
	case errors.Is(err, ErrWorkspaceNotReady),
		errors.Is(err, ErrWorkspaceChanged):
		return ErrCategoryWorkspace, false
	case errors.Is(err, ErrBranchConflict):
		return ErrCategoryGit, false
	case errors.Is(err, ErrChangeSetSecretsDetected),
		errors.Is(err, ErrInvalidChangeSet):
		return ErrCategoryValidation, false
	default:
		return ErrCategoryUnknown, false
	}
}
