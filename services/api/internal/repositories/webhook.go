package repositories

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	WebhookEventPing              = "ping"
	WebhookEventRepository        = "repository"
	WebhookEventIssues            = "issues"
	WebhookEventIssueComment      = "issue_comment"
	WebhookEventPullRequest       = "pull_request"
	WebhookEventPullReview        = "pull_request_review"
	WebhookEventPullReviewComment = "pull_request_review_comment"
	WebhookEventPullReviewThread  = "pull_request_review_thread"
	WebhookEventCheckRun          = "check_run"
	WebhookEventCheckSuite        = "check_suite"
	WebhookEventStatus            = "status"
	WebhookEventPush              = "push"
)

const (
	WebhookOperationIssues       = "issues"
	WebhookOperationPullRequests = "pull_requests"
	WebhookOperationFeedback     = "pr_feedback"
	WebhookOperationCommits      = "commits"
	WebhookOperationAll          = "all"
)

const (
	WebhookTargetStatusReceived    = "received"
	WebhookTargetStatusDispatching = "dispatching"
	WebhookTargetStatusEnqueued    = "enqueued"
	WebhookTargetStatusProcessing  = "processing"
	WebhookTargetStatusSucceeded   = "succeeded"
	WebhookTargetStatusFailed      = "failed"
)

const defaultWebhookMaxBodyBytes int64 = 1 << 20

var errInvalidWebhookPayload = errors.New("invalid webhook payload")

type WebhookSyncPayload struct {
	TargetID uuid.UUID `json:"target_id"`
}

type WebhookSyncResult struct {
	TargetID    uuid.UUID `json:"target_id"`
	Operation   string    `json:"operation"`
	Synced      int       `json:"synced"`
	PullRequest int       `json:"pull_request,omitempty"`
}

type GitHubWebhookHandler struct {
	store        WebhookStore
	secret       []byte
	maxBodyBytes int64
	now          func() time.Time
}

func NewGitHubWebhookHandler(store WebhookStore, secret string, maxBodyBytes int64) *GitHubWebhookHandler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultWebhookMaxBodyBytes
	}
	return &GitHubWebhookHandler{
		store:        store,
		secret:       []byte(secret),
		maxBodyBytes: maxBodyBytes,
		now:          time.Now,
	}
}

func (h *GitHubWebhookHandler) GitHub(c *gin.Context) {
	if h == nil || h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook_unavailable"})
		return
	}
	if len(h.secret) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook_unavailable"})
		return
	}

	body, err := readWebhookBody(c, h.maxBodyBytes)
	if err != nil {
		if errors.Is(err, errWebhookBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	if !verifyWebhookSignature(c.GetHeader("X-Hub-Signature-256"), body, h.secret) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_signature"})
		return
	}
	if !isJSONContentType(c.GetHeader("Content-Type")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	deliveryUUID, err := uuid.Parse(strings.TrimSpace(c.GetHeader("X-GitHub-Delivery")))
	if err != nil || deliveryUUID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	event := strings.ToLower(strings.TrimSpace(c.GetHeader("X-GitHub-Event")))
	if !validWebhookEventName(event) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	envelope, err := parseGitHubWebhookPayload(event, body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	now := h.now()
	delivery := &GitHubWebhookDelivery{
		ID:           uuid.New(),
		DeliveryID:   deliveryUUID.String(),
		Event:        event,
		Action:       envelope.Action,
		RepositoryID: envelope.RepositoryID,
		Ignored:      envelope.Ignored,
		ReceivedAt:   now,
		ProcessedAt:  &now,
	}
	if envelope.Ignored {
		if _, _, err := h.store.CreateWebhookDelivery(c.Request.Context(), delivery, nil); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "delivery_id": deliveryUUID.String()})
		return
	}

	selected, err := h.store.ListSelectedRepositoriesByGithubRepositoryID(c.Request.Context(), *envelope.RepositoryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	targets := make([]GitHubWebhookTarget, 0, len(selected))
	for _, repository := range selected {
		targets = append(targets, GitHubWebhookTarget{
			DeliveryID:           delivery.ID,
			SelectedRepositoryID: repository.ID,
			UserID:               repository.UserID,
			GithubRepositoryID:   repository.GithubRepositoryID,
			Operation:            envelope.Operation,
			IssueNumber:          envelope.IssueNumber,
			PullRequestNumber:    envelope.PullRequestNumber,
			HeadSHA:              envelope.HeadSHA,
			Status:               WebhookTargetStatusReceived,
		})
	}

	created, duplicate, err := h.store.CreateWebhookDelivery(c.Request.Context(), delivery, targets)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	if duplicate {
		c.JSON(http.StatusAccepted, gin.H{"status": "duplicate", "delivery_id": created.DeliveryID})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "delivery_id": created.DeliveryID, "targets": len(targets)})
}

var errWebhookBodyTooLarge = errors.New("webhook body too large")

func readWebhookBody(c *gin.Context, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultWebhookMaxBodyBytes
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errWebhookBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}

func verifyWebhookSignature(header string, body, secret []byte) bool {
	provided := strings.TrimSpace(strings.TrimPrefix(header, "sha256="))
	if !strings.HasPrefix(header, "sha256=") {
		provided = ""
	}
	decoded, err := hex.DecodeString(provided)
	expected := hmac.New(sha256.New, secret)
	_, _ = expected.Write(body)
	if err != nil {
		decoded = nil
	}
	return subtle.ConstantTimeCompare(decoded, expected.Sum(nil)) == 1
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func validWebhookEventName(event string) bool {
	if event == "" || len(event) > 64 {
		return false
	}
	for _, r := range event {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

type webhookRepository struct {
	ID int64 `json:"id"`
}

type webhookIssue struct {
	Number      int             `json:"number"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type webhookPullRequest struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type webhookCheckRun struct {
	HeadSHA      string `json:"head_sha"`
	Status       string `json:"status"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

type webhookCheckSuite struct {
	Status       string `json:"status"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

type webhookStatus struct {
	SHA   string `json:"sha"`
	State string `json:"state"`
}

type webhookPush struct {
	After string `json:"after"`
	Head  string `json:"head"`
}

type githubWebhookPayload struct {
	Action      string              `json:"action"`
	Repository  *webhookRepository  `json:"repository"`
	Issue       *webhookIssue       `json:"issue"`
	PullRequest *webhookPullRequest `json:"pull_request"`
	CheckRun    *webhookCheckRun    `json:"check_run"`
	CheckSuite  *webhookCheckSuite  `json:"check_suite"`
	Status      *webhookStatus      `json:"status"`
	Push        *webhookPush        `json:"push"`
}

type webhookEnvelope struct {
	Action            string
	RepositoryID      *int64
	Operation         string
	IssueNumber       *int
	PullRequestNumber *int
	HeadSHA           string
	Ignored           bool
}

func parseGitHubWebhookPayload(event string, body []byte) (webhookEnvelope, error) {
	var payload githubWebhookPayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return webhookEnvelope{}, errInvalidWebhookPayload
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return webhookEnvelope{}, errInvalidWebhookPayload
	}
	if len(payload.Action) > 64 {
		return webhookEnvelope{}, errInvalidWebhookPayload
	}
	if event == WebhookEventPing {
		return webhookEnvelope{Action: boundedWebhookAction(payload.Action), Ignored: true}, nil
	}
	if !supportedWebhookEvent(event) {
		return webhookEnvelope{Action: boundedWebhookAction(payload.Action), Ignored: true}, nil
	}
	if payload.Repository == nil || payload.Repository.ID <= 0 {
		return webhookEnvelope{}, errInvalidWebhookPayload
	}
	repositoryID := payload.Repository.ID
	envelope := webhookEnvelope{Action: boundedWebhookAction(payload.Action), RepositoryID: &repositoryID}

	switch event {
	case WebhookEventIssues:
		envelope.Operation = WebhookOperationIssues
		if payload.Issue == nil || payload.Issue.Number <= 0 {
			return webhookEnvelope{}, errInvalidWebhookPayload
		}
		envelope.IssueNumber = intPointer(payload.Issue.Number)
	case WebhookEventIssueComment:
		if payload.Issue == nil || payload.Issue.Number <= 0 {
			return webhookEnvelope{}, errInvalidWebhookPayload
		}
		envelope.IssueNumber = intPointer(payload.Issue.Number)
		if len(payload.Issue.PullRequest) > 0 && string(payload.Issue.PullRequest) != "null" {
			envelope.Operation = WebhookOperationFeedback
			envelope.PullRequestNumber = intPointer(payload.Issue.Number)
		} else {
			envelope.Operation = WebhookOperationIssues
		}
	case WebhookEventPullRequest:
		envelope.Operation = WebhookOperationPullRequests
		if payload.PullRequest == nil || payload.PullRequest.Number <= 0 {
			return webhookEnvelope{}, errInvalidWebhookPayload
		}
		envelope.PullRequestNumber = intPointer(payload.PullRequest.Number)
		if payload.PullRequest.Head.SHA != "" {
			sha, err := normalizeWebhookSHA(payload.PullRequest.Head.SHA)
			if err != nil {
				return webhookEnvelope{}, err
			}
			envelope.HeadSHA = sha
		}
	case WebhookEventPullReview, WebhookEventPullReviewComment, WebhookEventPullReviewThread:
		envelope.Operation = WebhookOperationFeedback
		if payload.PullRequest == nil || payload.PullRequest.Number <= 0 {
			return webhookEnvelope{}, errInvalidWebhookPayload
		}
		envelope.PullRequestNumber = intPointer(payload.PullRequest.Number)
	case WebhookEventCheckRun:
		envelope.Operation = WebhookOperationFeedback
		if payload.CheckRun == nil || payload.CheckRun.Status != "completed" {
			envelope.Ignored = true
			break
		}
		if len(payload.CheckRun.PullRequests) > 0 {
			if payload.CheckRun.PullRequests[0].Number <= 0 {
				return webhookEnvelope{}, errInvalidWebhookPayload
			}
			envelope.PullRequestNumber = intPointer(payload.CheckRun.PullRequests[0].Number)
		}
		if payload.CheckRun.HeadSHA == "" {
			return webhookEnvelope{}, errInvalidWebhookPayload
		}
		sha, err := normalizeWebhookSHA(payload.CheckRun.HeadSHA)
		if err != nil {
			return webhookEnvelope{}, err
		}
		envelope.HeadSHA = sha
	case WebhookEventCheckSuite:
		envelope.Operation = WebhookOperationFeedback
		if payload.CheckSuite == nil || payload.CheckSuite.Status != "completed" {
			envelope.Ignored = true
			break
		}
		if len(payload.CheckSuite.PullRequests) > 0 {
			if payload.CheckSuite.PullRequests[0].Number <= 0 {
				return webhookEnvelope{}, errInvalidWebhookPayload
			}
			envelope.PullRequestNumber = intPointer(payload.CheckSuite.PullRequests[0].Number)
		}
	case WebhookEventStatus:
		envelope.Operation = WebhookOperationFeedback
		if payload.Status == nil || !terminalWebhookState(payload.Status.State) || payload.Status.SHA == "" {
			envelope.Ignored = true
			break
		}
		sha, err := normalizeWebhookSHA(payload.Status.SHA)
		if err != nil {
			return webhookEnvelope{}, err
		}
		envelope.HeadSHA = sha
	case WebhookEventPush:
		envelope.Operation = WebhookOperationCommits
		if payload.Push != nil {
			shaValue := payload.Push.After
			if shaValue == "" || shaValue == "0000000000000000000000000000000000000000" {
				shaValue = payload.Push.Head
			}
			if shaValue != "" {
				sha, err := normalizeWebhookSHA(shaValue)
				if err != nil {
					return webhookEnvelope{}, err
				}
				envelope.HeadSHA = sha
			}
		}
	case WebhookEventRepository:
		envelope.Operation = WebhookOperationAll
	}
	return envelope, nil
}

func supportedWebhookEvent(event string) bool {
	switch event {
	case WebhookEventRepository, WebhookEventIssues, WebhookEventIssueComment,
		WebhookEventPullRequest, WebhookEventPullReview, WebhookEventPullReviewComment,
		WebhookEventPullReviewThread, WebhookEventCheckRun, WebhookEventCheckSuite,
		WebhookEventStatus, WebhookEventPush:
		return true
	default:
		return false
	}
}

func boundedWebhookAction(action string) string {
	if len(action) > 64 {
		return action[:64]
	}
	return action
}

func intPointer(value int) *int {
	return &value
}

func terminalWebhookState(state string) bool {
	switch state {
	case "success", "failure", "error":
		return true
	default:
		return false
	}
}

func normalizeWebhookSHA(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 {
		return "", errInvalidWebhookPayload
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", errInvalidWebhookPayload
	}
	return value, nil
}
