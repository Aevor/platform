package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/jobs"
)

// Background job type identifiers. They are stable, owner-facing tokens:
// clients enqueue and filter by them, and the frontend surfaces them directly.
const (
	JobTypeRepositoryIndex     = "repository.index"
	JobTypeIntelligenceRefresh = "repository.intelligence_refresh"
	JobTypeIssueAnalysis       = "issue.analysis"
	JobTypeSolutionProposal    = "issue.solution_proposal"
	JobTypeCodeGeneration      = "issue.code_generation"
	JobTypeValidation          = "issue.validation"
	JobTypePullRequestFeedback = "pull_request.feedback"
)

// Job payloads carry REFERENCES only (compound IDs / booleans) — never issue
// content, source text, AI output, or secrets.
type issueReferencePayload struct {
	IssueID uuid.UUID `json:"issue_id"`
}

// changeSetReferencePayload additionally names the change set for validation.
type changeSetReferencePayload struct {
	IssueID     uuid.UUID `json:"issue_id"`
	ChangeSetID uuid.UUID `json:"change_set_id"`
}

// intelligenceRefreshPayload controls whether the AI architecture layer is
// regenerated alongside the deterministic profile.
type intelligenceRefreshPayload struct {
	IncludeAI bool `json:"include_ai"`
}

// pullRequestReferencePayload names a pull request by its GitHub number.
type pullRequestReferencePayload struct {
	Number int `json:"number"`
}

// indexJobResult is the reference-only result envelope attached to a
// repository.index job on success.
type indexJobResult struct {
	RepositoryID uuid.UUID `json:"repository_id"`
	Files        int       `json:"files"`
	Chunks       int       `json:"chunks"`
}

// unmarshalJobPayload decodes a stored JSON envelope, tolerating an empty
// store for optional payloads.
func unmarshalJobPayload(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return json.Unmarshal(raw, out)
}

func validateIssuePayload(raw json.RawMessage) (any, error) {
	var payload issueReferencePayload
	if err := unmarshalJobPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.IssueID == uuid.Nil {
		return nil, errors.New("issue_id is required")
	}
	return payload, nil
}

func validateChangeSetPayload(raw json.RawMessage) (any, error) {
	var payload changeSetReferencePayload
	if err := unmarshalJobPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.IssueID == uuid.Nil || payload.ChangeSetID == uuid.Nil {
		return nil, errors.New("issue_id and change_set_id are required")
	}
	return payload, nil
}

func validatePullRequestPayload(raw json.RawMessage) (any, error) {
	var payload pullRequestReferencePayload
	if err := unmarshalJobPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Number <= 0 {
		return nil, errors.New("number must be positive")
	}
	return payload, nil
}

// RegisterJobDefinitions registers every background job type the domain layer
// can execute. Definitions close over the Service; registration happens once
// at wiring time (main.go) before any worker starts.
func RegisterJobDefinitions(reg *jobs.Registry, s *Service) {
	reg.MustRegister(jobs.Definition{
		Type:        JobTypeRepositoryIndex,
		Description: "Indexes the owned repository's current workspace contents",
		ValidatePayload: func(raw json.RawMessage) (any, error) {
			return nil, nil
		},
		Timeout:     30 * time.Minute,
		MaxAttempts: 2,
		Classify:    s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			result, err := s.IndexRepositoryContent(ctx, job.UserID, job.SelectedRepositoryID)
			if err == nil {
				exec.SetResult(indexJobResult{
					RepositoryID: job.SelectedRepositoryID,
					Files:        result.Files,
					Chunks:       result.Chunks,
				})
			}
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:        JobTypeIntelligenceRefresh,
		Description: "Regenerates the revision-bound repository intelligence profile",
		ValidatePayload: func(raw json.RawMessage) (any, error) {
			var payload intelligenceRefreshPayload
			if err := unmarshalJobPayload(raw, &payload); err != nil {
				return nil, err
			}
			return payload, nil
		},
		Timeout:     30 * time.Minute,
		MaxAttempts: 2,
		Classify:    s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload intelligenceRefreshPayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.RefreshIntelligence(ctx, job.UserID, job.SelectedRepositoryID, payload.IncludeAI)
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:            JobTypeIssueAnalysis,
		Description:     "Structured AI analysis of an owned issue",
		ValidatePayload: validateIssuePayload,
		Timeout:         20 * time.Minute,
		MaxAttempts:     3,
		Classify:        s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload issueReferencePayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.AnalyzeIssue(ctx, job.UserID, job.SelectedRepositoryID, payload.IssueID)
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:            JobTypeSolutionProposal,
		Description:     "AI solution proposal for an owned issue",
		ValidatePayload: validateIssuePayload,
		Timeout:         20 * time.Minute,
		MaxAttempts:     3,
		Classify:        s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload issueReferencePayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.ProposeSolution(ctx, job.UserID, job.SelectedRepositoryID, payload.IssueID)
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:            JobTypeCodeGeneration,
		Description:     "Generate-changes for an owned issue",
		ValidatePayload: validateIssuePayload,
		Timeout:         30 * time.Minute,
		MaxAttempts:     3,
		Classify:        s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload issueReferencePayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.GenerateChanges(ctx, job.UserID, job.SelectedRepositoryID, payload.IssueID)
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:            JobTypeValidation,
		Description:     "Validation of an applied change set",
		ValidatePayload: validateChangeSetPayload,
		Timeout:         30 * time.Minute,
		MaxAttempts:     3,
		Classify:        s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload changeSetReferencePayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.ValidateChangeSet(ctx, job.UserID, job.SelectedRepositoryID, payload.IssueID, payload.ChangeSetID)
			return err
		},
	})

	reg.MustRegister(jobs.Definition{
		Type:            JobTypePullRequestFeedback,
		Description:     "Grounded AI analysis of an owned pull request's feedback",
		ValidatePayload: validatePullRequestPayload,
		Timeout:         20 * time.Minute,
		MaxAttempts:     3,
		Classify:        s.classifyJobError,
		Run: func(ctx context.Context, job *jobs.Job, exec *jobs.Executor) error {
			var payload pullRequestReferencePayload
			if err := unmarshalJobPayload(job.Payload, &payload); err != nil {
				return err
			}
			_, err := s.AnalyzePullRequestFeedback(ctx, job.UserID, job.SelectedRepositoryID, payload.Number)
			return err
		},
	})
}

// classifyJobError reduces an arbitrary handler error to the stable, safe job
// category. It reuses the run taxonomy so a stage that fails in a background
// job classifies identically to the same stage failing synchronously. When the
// run taxonomy marks a failure as non-retryable, the job category is degraded
// to a non-retryable one so the engine will not auto-retry what the pipeline
// decided must stop.
func (s *Service) classifyJobError(err error) jobs.ErrorCategory {
	category, retryable := stageErrorCategory(err)
	if err == nil {
		category, retryable = ErrCategoryValidation, false
	}
	if category == "" {
		category, retryable = ErrCategoryUnknown, false
	}

	jobCategory := jobCategoryFrom(category)
	if !retryable && jobCategory.Retryable() {
		return jobs.CategoryUserError
	}
	return jobCategory
}

// jobCategoryFrom maps a run-event category to the corresponding job engine
// category. Every run category has a home; unknown values fail closed.
func jobCategoryFrom(category string) jobs.ErrorCategory {
	switch category {
	case ErrCategoryAIService:
		return jobs.CategoryAIServiceFailure
	case ErrCategoryTimeout:
		return jobs.CategoryTimeout
	case ErrCategoryAuthorization:
		return jobs.CategoryAuthorizationError
	case ErrCategoryValidation:
		return jobs.CategoryValidationError
	case ErrCategoryGitHub, ErrCategoryGit:
		return jobs.CategoryTransientExternalError
	case ErrCategoryDatabase:
		return jobs.CategoryDatabaseFailure
	case ErrCategoryWorkspace, ErrCategoryDependency:
		return jobs.CategoryUserError
	default:
		return jobs.CategoryInternalFailure
	}
}

// jobEventRunMetadata is the bounded metadata envelope stamped onto a run
// audit event by the job engine. References only — never payloads, results, or
// error text.
type jobEventRunMetadata struct {
	JobID   uuid.UUID `json:"job_id,omitempty"`
	JobType string    `json:"job_type,omitempty"`
}

// RecordJobEvent implements jobs.EventRecorder: it persists one bounded job
// lifecycle event onto the job's linked engineering run. Resources shared with
// a run surface deterministically (index/intelligence jobs) have no linked run
// and surface progress through the job itself, so nothing is recorded here.
// Recording is best-effort and can never fail the job: a missing run is a
// no-op, and a storage failure surfaces as a log line only.
func (s *Service) RecordJobEvent(ctx context.Context, event *jobs.JobEvent) error {
	if event == nil || event.RunID == nil {
		return nil
	}

	run, err := s.store.GetRunByIDUnscoped(*event.RunID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			return nil
		}
		return err
	}
	if run == nil {
		return nil
	}

	return s.recordEvent(ctx, run, event.Type, event.Status, event.DurationMs,
		jobEventRunMetadata{JobID: event.JobID, JobType: event.JobType},
		string(event.ErrorCategory), event.ErrorReference)
}
