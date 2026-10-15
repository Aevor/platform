package repositories

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/impact"
	"github.com/Aevor/platform/services/api/internal/jobs"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/validation"
	"github.com/Aevor/platform/services/api/internal/webhook"
)

// impactAnalysisSetResponse is the SAFE external shape of an impact analysis.
// The deterministic, repository-derived analyses are authoritative and always
// returned; the AI block is explicitly labeled, so a client can never mistake an
// inferred suggestion for a repository fact.
type impactAnalysisSetResponse struct {
	RepositoryID  string             `json:"repository_id"`
	ChangeSetID   *uuid.UUID         `json:"change_set_id,omitempty"`
	Analyses      []*impact.Analysis `json:"analyses"`
	AIUnavailable bool               `json:"ai_unavailable"`
	AINote        string             `json:"ai_note,omitempty"`
	AISummary     string             `json:"ai_summary,omitempty"`
	AIRisks       []string           `json:"ai_risks,omitempty"`
	AIUncertainty string             `json:"ai_uncertainty,omitempty"`
	AISemantic    []AISemanticImpact `json:"ai_possible_impacts,omitempty"`
	AIDropped     int                `json:"ai_dropped,omitempty"`
}

// AnalyzeImpact handles POST /repositories/:id/impact-analysis for the
// authenticated user: deterministic, repository-derived impact analysis for
// one explicit target OR one owned change set, with optional grounded AI
// enrichment that is always labeled as inferred.
//
// The two target modes are mutually exclusive: a change set derives its own
// targets from the persisted, ownership-scoped change-set rows, so a client can
// never mix its own paths into a change-set analysis.
func (h *Handler) AnalyzeImpact(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	selectedRepositoryID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	var body struct {
		Target *struct {
			FilePath string `json:"file_path"`
			Symbol   string `json:"symbol"`
		} `json:"target"`
		ChangeSetID *string `json:"change_set_id"`
		IncludeAI   bool    `json:"include_ai"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	request := ImpactAnalysisRequest{IncludeAI: body.IncludeAI}

	switch {
	case body.ChangeSetID != nil:
		changeSetID, parseErr := uuid.Parse(*body.ChangeSetID)
		if parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
		if body.Target != nil {
			// A change set is the single source of its targets; mixing in a
			// client-supplied target would let a caller analyze a path the
			// change set never touched.
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
			return
		}
		request.ChangeSetID = &changeSetID
	case body.Target != nil:
		request.FilePath = body.Target.FilePath
		request.Symbol = body.Target.Symbol
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	result, err := h.service.AnalyzeImpact(c.Request.Context(), userID, selectedRepositoryID, request)
	if err != nil {
		impactErrorResponse(c, err)
		return
	}

	response := impactAnalysisSetResponse{
		RepositoryID:  result.RepositoryID,
		Analyses:      result.Analyses,
		AIUnavailable: result.AIUnavailable,
		AINote:        result.AINote,
		AISummary:     result.AISummary,
		AIRisks:       result.AIRisks,
		AIUncertainty: result.AIUncertainty,
		AISemantic:    result.AISemantic,
		AIDropped:     result.AIDropped,
	}

	if result.ChangeSetID != nil {
		response.ChangeSetID = result.ChangeSetID
	}

	c.JSON(http.StatusOK, response)
}

// impactErrorResponse maps impact-analysis failures onto the same opaque,
// ownership-first vocabulary as the rest of the repository surface. A repository
// the caller does not own is indistinguishable from one that does not exist.
func impactErrorResponse(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrSelectedNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
	case errors.Is(err, ErrChangeSetNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "change_set_not_found"})
	case errors.Is(err, ErrInvalidImpactRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
	case errors.Is(err, ErrInvalidChangeSet):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_change_set"})
	case errors.Is(err, ErrWorkspaceNotReady):
		c.JSON(http.StatusConflict, gin.H{"error": "workspace_not_ready"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// PublishChangeSet handles POST
// /repositories/:id/issues/:issueID/changes/:changeSetID/publish for the
// authenticated user: explicit, user-initiated delivery of a validated change
// set to GitHub as a branch, a commit, and a pull request.
//
// This is the only endpoint that ever writes to a user's remote, and it only
// runs after the user approved and validated the exact reviewed change set. It
// is idempotent: an in-flight or already-delivered change set re-reports the
// same branch, commit, and pull request instead of duplicating them, and a
// conflicting remote branch is refused rather than force-pushed.
func (h *Handler) PublishChangeSet(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, issueID, changeSetID, ok := parseChangeSetParams(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	state, err := h.service.PublishChangeSet(c.Request.Context(), userID, id, issueID, changeSetID)
	if err != nil {
		issuePipelineErrorResponse(c, err)
		return
	}

	c.JSON(http.StatusOK, changeSetStateResponseFromState(state))
}

// GetPullRequestDetails handles GET /repositories/:id/pull-requests/:number for
// the authenticated user: the live GitHub view of one pull request on a
// repository the caller owns. Nothing is cached, so Aevor never reports a stale
// CI verdict or an outdated review.
func (h *Handler) GetPullRequestDetails(c *gin.Context) {
	userID, selectedRepositoryID, number, ok := parsePullRequestParams(c)
	if !ok {
		return
	}

	details, err := h.service.PullRequestDetails(c.Request.Context(), userID, selectedRepositoryID, number)
	if err != nil {
		pullRequestErrorResponse(c, err)
		return
	}

	c.JSON(http.StatusOK, pullRequestDetailsResponseFromDetails(details))
}

// AnalyzePullRequestFeedback handles POST
// /repositories/:id/pull-requests/:number/analyze-feedback for the
// authenticated user: a grounded, structured analysis of the review feedback
// on one pull request the caller owns. Ownership is verified BEFORE the AI
// service is contacted, so an inaccessible repository never causes an outbound
// call, and the request carries only GitHub facts plus bounded context chunks.
func (h *Handler) AnalyzePullRequestFeedback(c *gin.Context) {
	userID, selectedRepositoryID, number, ok := parsePullRequestParams(c)
	if !ok {
		return
	}

	result, err := h.service.AnalyzePullRequestFeedback(c.Request.Context(), userID, selectedRepositoryID, number)
	if err != nil {
		pullRequestErrorResponse(c, err)
		return
	}

	if result == nil || result.Response == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}

	c.JSON(http.StatusOK, pullRequestFeedbackResponseFromResult(number, result.Response))
}

// parsePullRequestParams resolves the caller's Aevor identity and validates the
// :id and :number path params shared by the pull request detail and feedback
// endpoints.
func parsePullRequestParams(c *gin.Context) (uuid.UUID, uuid.UUID, int, bool) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return uuid.Nil, uuid.Nil, 0, false
	}

	selectedRepositoryID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return uuid.Nil, uuid.Nil, 0, false
	}

	number, err := strconv.Atoi(c.Param("number"))
	if err != nil || number <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return uuid.Nil, uuid.Nil, 0, false
	}

	return userID, selectedRepositoryID, number, true
}

// pullRequestErrorResponse maps pull request failures onto the opaque,
// ownership-first error vocabulary. A repository the caller does not own is
// indistinguishable from one that does not exist, and GitHub failures are
// reported by category without echoing upstream bodies.
func pullRequestErrorResponse(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrSelectedNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "repository_not_found"})
	case errors.Is(err, github.ErrPullRequestNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "pull_request_not_found"})
	case errors.Is(err, ErrWorkspaceNotReady):
		c.JSON(http.StatusConflict, gin.H{"error": "workspace_not_ready"})
	case errors.Is(err, users.ErrNotFound), errors.Is(err, users.ErrGitHubTokenMissing):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "github_token_missing"})
	case errors.Is(err, github.ErrUnauthorized):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "github_token_invalid"})
	case errors.Is(err, github.ErrRateLimited):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "github_rate_limited"})
	case errors.Is(err, github.ErrUnavailable),
		errors.Is(err, github.ErrInvalidResponse),
		errors.Is(err, github.ErrAPIError):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "github_unavailable"})
	case errors.Is(err, ai.ErrUnavailable), errors.Is(err, ai.ErrTimeout):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai_service_unavailable"})
	case errors.Is(err, ai.ErrRateLimited):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "ai_rate_limited"})
	case errors.Is(err, ai.ErrUnauthorized):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "ai_unauthorized"})
	case errors.Is(err, ai.ErrInvalidResponse):
		// A response the AI service contract does not describe is never
		// partially trusted: the analysis fails closed as an opaque server
		// error rather than surfacing an unvalidated payload.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	case errors.Is(err, ai.ErrRejected):
		c.JSON(http.StatusBadRequest, gin.H{"error": "ai_request_rejected"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// validationPlanResponse is the SAFE external shape of a change set's
// validation plan: which toolchains Aevor detected, exactly which controlled
// checks it would run, and the notes that bound the plan. It is a read-only
// preview — planning never mutates the change set or the workspace.
type validationPlanResponse struct {
	ChangeSetID  uuid.UUID                 `json:"change_set_id"`
	RepositoryID string                    `json:"repository_id"`
	IssueID      string                    `json:"issue_id"`
	Status       string                    `json:"status"`
	ChangedFiles []string                  `json:"changed_files"`
	Toolchains   []validation.Toolchain    `json:"toolchains"`
	Checks       []validation.PlannedCheck `json:"checks"`
	Notes        []string                  `json:"notes"`
}

// ValidationPlan handles GET
// /repositories/:id/issues/:issueID/changes/:changeSetID/validation-plan for the
// authenticated user: the trusted plan for validating one change set, returned
// before anything runs so a user can see exactly which controlled checks would
// execute against their workspace.
func (h *Handler) ValidationPlan(c *gin.Context) {
	userID, ok := auth.GetAuthenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	id, issueID, changeSetID, ok := parseChangeSetParams(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	plan, err := h.service.ValidationPlan(c.Request.Context(), userID, id, issueID, changeSetID)
	if err != nil {
		issuePipelineErrorResponse(c, err)
		return
	}

	c.JSON(http.StatusOK, validationPlanResponse{
		ChangeSetID:  plan.ChangeSetID,
		RepositoryID: plan.RepositoryID.String(),
		IssueID:      plan.IssueID.String(),
		Status:       plan.Status,
		ChangedFiles: plan.ChangedFiles,
		Toolchains:   plan.Toolchains,
		Checks:       plan.Checks,
		Notes:        plan.Notes,
	})
}

// HandleGitHubWebhook receives GitHub webhook events for owned repositories.
// It verifies the HMAC signature, persists the event, and enqueues a background
// job to process it. Only events for repositories the caller owns are accepted;
// foreign repositories are silently dropped (opaque 200) to avoid enumeration.
func (h *Handler) HandleGitHubWebhook(c *gin.Context) {
	// Read the raw body for signature verification.
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("webhook: failed to read body: %v", err)
		c.Status(http.StatusBadRequest)
		return
	}

	// Verify GitHub HMAC signature.
	signature := c.GetHeader("X-Hub-Signature-256")
	if signature == "" {
		log.Printf("webhook: missing X-Hub-Signature-256 header")
		c.Status(http.StatusBadRequest)
		return
	}

	secret := h.service.GetWebhookSecret()
	if secret == "" {
		log.Printf("webhook: webhook secret not configured")
		c.Status(http.StatusInternalServerError)
		return
	}

	expectedMAC := hmac.New(sha256.New, []byte(secret))
	expectedMAC.Write(body)
	expectedSig := "sha256=" + hex.EncodeToString(expectedMAC.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		log.Printf("webhook: invalid signature")
		c.Status(http.StatusUnauthorized)
		return
	}

	eventType := c.GetHeader("X-GitHub-Event")
	if eventType == "" {
		log.Printf("webhook: missing X-GitHub-Event header")
		c.Status(http.StatusBadRequest)
		return
	}

	deliveryID := c.GetHeader("X-GitHub-Delivery")

	// Parse the payload to determine the repository.
	var payload struct {
		Repository struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("webhook: failed to parse payload: %v", err)
		c.Status(http.StatusBadRequest)
		return
	}

	repoFullName := payload.Repository.FullName
	if repoFullName == "" {
		log.Printf("webhook: repository full_name missing in payload")
		c.Status(http.StatusBadRequest)
		return
	}

	ownerLogin, repoName, ok := parseFullName(repoFullName)
	if !ok {
		log.Printf("webhook: could not parse repo full_name: %s", repoFullName)
		c.Status(http.StatusBadRequest)
		return
	}

	// Create webhook target and enqueue job.
	// The id is assigned HERE, before the job payload is built, because the
	// payload must carry the same id the row is persisted under. Letting the
	// store assign it on insert would enqueue a job pointing at uuid.Nil, and
	// the worker would never find its target.
	target := &webhook.WebhookTarget{
		ID:           uuid.New(),
		RepositoryID: uuid.Nil,
		EventType:    eventType,
		Payload:      body,
		Status:       "enqueued",
		Attempts:     0,
		MaxAttempts:  3,
		ScheduledAt:  time.Now(),
	}

	// The row keeps the RAW signed body plus the routing facts the worker needs
	// to resolve which selected repository this event belongs to.
	storedPayload := map[string]any{
		"owner":       ownerLogin,
		"repo":        repoName,
		"event_type":  eventType,
		"delivery_id": deliveryID,
		"body":        json.RawMessage(body),
	}

	payloadBytes, err := json.Marshal(storedPayload)
	if err != nil {
		log.Printf("webhook: failed to encode target payload: %v", err)
		c.Status(http.StatusInternalServerError)
		return
	}

	target.Payload = payloadBytes

	if err := h.service.GetWebhookStore().CreateWebhookTarget(c.Request.Context(), target); err != nil {
		log.Printf("webhook: failed to create target: %v", err)
		c.Status(http.StatusInternalServerError)
		return
	}

	// The job payload is the contract the repository.sync definition validates:
	// a single target_id. The routing facts live on the target row, not here.
	// The STRUCT is passed, not pre-marshalled bytes — the job service encodes
	// the payload itself, and handing it bytes would double-encode into a JSON
	// string that fails validation.
	if _, err := h.service.GetJobsService().Enqueue(c.Request.Context(), jobs.EnqueueOptions{
		Type:     JobTypeRepositorySync,
		Payload:  webhookSyncPayload{TargetID: target.ID},
		Priority: 10,
	}); err != nil {
		log.Printf("webhook: failed to enqueue job: %v", err)
		c.Status(http.StatusInternalServerError)
		return
	}

	c.Status(http.StatusAccepted)
}

// parseFullName splits "owner/repo" into owner and repo.
func parseFullName(fullName string) (string, string, bool) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
