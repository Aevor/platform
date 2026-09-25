package repositories

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/jobs"
)

type memoryWebhookStore struct {
	mu           sync.Mutex
	deliveries   map[string]GitHubWebhookDelivery
	targets      map[uuid.UUID]GitHubWebhookTarget
	selected     []SelectedRepository
	pullRequests map[string]RepositoryPullRequest
}

func newMemoryWebhookStore(selected []SelectedRepository) *memoryWebhookStore {
	return &memoryWebhookStore{
		deliveries:   make(map[string]GitHubWebhookDelivery),
		targets:      make(map[uuid.UUID]GitHubWebhookTarget),
		selected:     selected,
		pullRequests: make(map[string]RepositoryPullRequest),
	}
}

func (s *memoryWebhookStore) CreateWebhookDelivery(_ context.Context, delivery *GitHubWebhookDelivery, targets []GitHubWebhookTarget) (*GitHubWebhookDelivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.deliveries[delivery.DeliveryID]; ok {
		copy := existing
		return &copy, true, nil
	}
	if delivery.ID == uuid.Nil {
		delivery.ID = uuid.New()
	}
	copy := *delivery
	s.deliveries[delivery.DeliveryID] = copy
	for i := range targets {
		target := targets[i]
		if target.ID == uuid.Nil {
			target.ID = uuid.New()
		}
		target.DeliveryID = delivery.ID
		if target.Status == "" {
			target.Status = WebhookTargetStatusReceived
		}
		if target.CreatedAt.IsZero() {
			target.CreatedAt = time.Now()
		}
		target.UpdatedAt = target.CreatedAt
		s.targets[target.ID] = target
	}
	copy = *delivery
	return &copy, false, nil
}

func (s *memoryWebhookStore) FindWebhookDelivery(_ context.Context, deliveryID string) (*GitHubWebhookDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delivery, ok := s.deliveries[deliveryID]
	if !ok {
		return nil, ErrWebhookDeliveryNotFound
	}
	return &delivery, nil
}

func (s *memoryWebhookStore) ListSelectedRepositoriesByGithubRepositoryID(_ context.Context, repositoryID int64) ([]SelectedRepository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]SelectedRepository, 0)
	for _, selected := range s.selected {
		if selected.GithubRepositoryID == repositoryID {
			result = append(result, selected)
		}
	}
	return result, nil
}

func (s *memoryWebhookStore) ListWebhookActivity(_ context.Context, userID, selectedRepositoryID uuid.UUID, limit, offset int) ([]WebhookActivity, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]WebhookActivity, 0)
	for _, target := range s.targets {
		if target.UserID != userID || target.SelectedRepositoryID != selectedRepositoryID {
			continue
		}
		var delivery GitHubWebhookDelivery
		for _, candidate := range s.deliveries {
			if candidate.ID == target.DeliveryID {
				delivery = candidate
				break
			}
		}
		items = append(items, WebhookActivity{
			RepositoryID:      target.SelectedRepositoryID,
			TargetID:          target.ID,
			DeliveryID:        target.DeliveryID,
			GitHubDeliveryID:  delivery.DeliveryID,
			Event:             delivery.Event,
			Action:            delivery.Action,
			Operation:         target.Operation,
			Status:            target.Status,
			IssueNumber:       target.IssueNumber,
			PullRequestNumber: target.PullRequestNumber,
			JobID:             target.JobID,
			Attempts:          target.Attempts,
			ErrorCategory:     target.ErrorCategory,
			ErrorReference:    target.ErrorReference,
			ReceivedAt:        delivery.ReceivedAt,
			EnqueuedAt:        target.EnqueuedAt,
			StartedAt:         target.StartedAt,
			CompletedAt:       target.CompletedAt,
			CreatedAt:         target.CreatedAt,
			UpdatedAt:         target.UpdatedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	total := len(items)
	if offset >= total {
		return []WebhookActivity{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return items[offset:end], total, nil
}

func (s *memoryWebhookStore) GetWebhookTarget(_ context.Context, targetID uuid.UUID) (*GitHubWebhookTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok {
		return nil, ErrWebhookTargetNotFound
	}
	return &target, nil
}

func (s *memoryWebhookStore) FindPullRequestByHeadSHA(_ context.Context, selectedRepositoryID uuid.UUID, headSHA string) (*RepositoryPullRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := selectedRepositoryID.String() + ":" + headSHA
	pullRequest, ok := s.pullRequests[key]
	if !ok {
		return nil, ErrWebhookPullRequestNotFound
	}
	return &pullRequest, nil
}

func (s *memoryWebhookStore) ListWebhookTargetsForReconciliation(_ context.Context, now time.Time, limit int) ([]GitHubWebhookTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]GitHubWebhookTarget, 0)
	for _, target := range s.targets {
		if target.Status == WebhookTargetStatusReceived || target.Status == WebhookTargetStatusEnqueued || target.Status == WebhookTargetStatusProcessing || (target.Status == WebhookTargetStatusDispatching && (target.LeaseExpiresAt == nil || !target.LeaseExpiresAt.After(now))) {
			result = append(result, target)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryWebhookStore) ClaimWebhookTarget(_ context.Context, targetID uuid.UUID, owner string, now time.Time, leaseFor time.Duration) (*GitHubWebhookTarget, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || (target.Status != WebhookTargetStatusReceived && target.Status != WebhookTargetStatusDispatching) {
		return nil, false, nil
	}
	if target.Status == WebhookTargetStatusDispatching && target.LeaseExpiresAt != nil && target.LeaseExpiresAt.After(now) {
		return nil, false, nil
	}
	lease := now.Add(leaseFor)
	target.Status = WebhookTargetStatusDispatching
	target.Attempts++
	target.LeaseOwner = owner
	target.LeaseExpiresAt = &lease
	target.UpdatedAt = now
	s.targets[targetID] = target
	return &target, true, nil
}

func (s *memoryWebhookStore) MarkWebhookTargetEnqueued(_ context.Context, targetID, jobID uuid.UUID, owner string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.Status != WebhookTargetStatusDispatching || target.LeaseOwner != owner {
		return false, nil
	}
	target.Status = WebhookTargetStatusEnqueued
	target.JobID = &jobID
	target.EnqueuedAt = &now
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) MarkWebhookTargetProcessing(_ context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.JobID == nil || *target.JobID != jobID || (target.Status != WebhookTargetStatusEnqueued && target.Status != WebhookTargetStatusProcessing) {
		return false, nil
	}
	target.Status = WebhookTargetStatusProcessing
	if target.StartedAt == nil {
		target.StartedAt = &now
	}
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) MarkWebhookTargetSucceeded(_ context.Context, targetID, jobID uuid.UUID, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.JobID == nil || *target.JobID != jobID {
		return false, nil
	}
	target.Status = WebhookTargetStatusSucceeded
	target.CompletedAt = &now
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) MarkWebhookTargetFailed(_ context.Context, targetID, jobID uuid.UUID, category, reference string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.JobID == nil || *target.JobID != jobID {
		return false, nil
	}
	target.Status = WebhookTargetStatusFailed
	target.ErrorCategory = category
	target.ErrorReference = reference
	target.CompletedAt = &now
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) MarkWebhookTargetDispatchFailed(_ context.Context, targetID uuid.UUID, owner, category, reference string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.Status != WebhookTargetStatusDispatching || target.LeaseOwner != owner {
		return false, nil
	}
	target.Status = WebhookTargetStatusFailed
	target.ErrorCategory = category
	target.ErrorReference = reference
	target.CompletedAt = &now
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) ResetWebhookTarget(_ context.Context, targetID uuid.UUID, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.JobID != nil {
		return false, nil
	}
	target.Status = WebhookTargetStatusDispatching
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.UpdatedAt = now
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) ReleaseWebhookTarget(_ context.Context, targetID uuid.UUID, owner, category, reference string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.targets[targetID]
	if !ok || target.LeaseOwner != owner {
		return false, nil
	}
	target.Status = WebhookTargetStatusDispatching
	target.JobID = nil
	target.LeaseOwner = ""
	target.LeaseExpiresAt = nil
	target.ErrorCategory = category
	target.ErrorReference = reference
	target.UpdatedAt = time.Now()
	s.targets[targetID] = target
	return true, nil
}

func (s *memoryWebhookStore) snapshotTargets() []GitHubWebhookTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]GitHubWebhookTarget, 0, len(s.targets))
	for _, target := range s.targets {
		result = append(result, target)
	}
	return result
}

func newWebhookTestHandler(store WebhookStore, secret string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGitHubWebhookHandler(store, secret, 1<<20)
	router.POST("/webhooks/github", handler.GitHub)
	return router
}

func signedWebhookRequest(t *testing.T, event, deliveryID string, payload []byte, secret string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return request
}

func performWebhook(router http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestGitHubWebhookHandlerRejectsUnconfiguredAndInvalidRequests(t *testing.T) {
	store := newMemoryWebhookStore(nil)
	payload := []byte(`{"zen":"test"}`)
	validRequest := signedWebhookRequest(t, WebhookEventPing, uuid.NewString(), payload, "webhook-secret")

	unconfigured := performWebhook(newWebhookTestHandler(store, ""), validRequest.Clone(context.Background()))
	if unconfigured.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want %d", unconfigured.Code, http.StatusServiceUnavailable)
	}

	invalidRequest := signedWebhookRequest(t, WebhookEventPing, uuid.NewString(), payload, "other-secret")
	invalid := performWebhook(newWebhookTestHandler(store, "webhook-secret"), invalidRequest)
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d, want %d", invalid.Code, http.StatusUnauthorized)
	}

	missingDelivery := signedWebhookRequest(t, WebhookEventPing, "not-a-uuid", payload, "webhook-secret")
	missing := performWebhook(newWebhookTestHandler(store, "webhook-secret"), missingDelivery)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("invalid delivery status = %d, want %d", missing.Code, http.StatusBadRequest)
	}

	wrongType := signedWebhookRequest(t, WebhookEventPing, uuid.NewString(), payload, "webhook-secret")
	wrongType.Header.Set("Content-Type", "text/plain")
	wrongTypeResponse := performWebhook(newWebhookTestHandler(store, "webhook-secret"), wrongType)
	if wrongTypeResponse.Code != http.StatusBadRequest {
		t.Fatalf("wrong content type status = %d, want %d", wrongTypeResponse.Code, http.StatusBadRequest)
	}

	tooLarge := signedWebhookRequest(t, WebhookEventPing, uuid.NewString(), payload, "webhook-secret")
	handler := NewGitHubWebhookHandler(store, "webhook-secret", 1)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/webhooks/github", handler.GitHub)
	if response := performWebhook(router, tooLarge); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestGitHubWebhookHandlerCreatesOwnerFanoutAndDeduplicatesDelivery(t *testing.T) {
	userA := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	userB := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000002")
	selectedA := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000001")
	selectedB := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002")
	store := newMemoryWebhookStore([]SelectedRepository{
		{ID: selectedA, UserID: userA, GithubRepositoryID: 77},
		{ID: selectedB, UserID: userB, GithubRepositoryID: 77},
	})
	payload := []byte(`{"action":"opened","repository":{"id":77},"issue":{"number":9,"body":"untrusted-body-marker"},"raw":"must-not-be-stored"}`)
	deliveryID := uuid.NewString()
	router := newWebhookTestHandler(store, "webhook-secret")

	first := performWebhook(router, signedWebhookRequest(t, WebhookEventIssues, deliveryID, payload, "webhook-secret"))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	targets := store.snapshotTargets()
	if len(targets) != 2 {
		t.Fatalf("target count = %d, want 2", len(targets))
	}
	for _, target := range targets {
		if target.Operation != WebhookOperationIssues || target.IssueNumber == nil || *target.IssueNumber != 9 {
			t.Fatalf("unexpected target: %+v", target)
		}
	}
	encoded, _ := json.Marshal(store.deliveries)
	if strings.Contains(string(encoded), "untrusted-body-marker") || strings.Contains(string(encoded), "must-not-be-stored") {
		t.Fatal("webhook delivery persisted raw payload content")
	}

	second := performWebhook(router, signedWebhookRequest(t, WebhookEventIssues, deliveryID, payload, "webhook-secret"))
	if second.Code != http.StatusAccepted || !strings.Contains(second.Body.String(), "duplicate") {
		t.Fatalf("duplicate response = %d %s", second.Code, second.Body.String())
	}
	if len(store.snapshotTargets()) != 2 || len(store.deliveries) != 1 {
		t.Fatal("duplicate delivery created another delivery or target")
	}
}

func TestParseGitHubWebhookPayloadMapsSupportedEvents(t *testing.T) {
	validSHA := strings.Repeat("a", 40)
	cases := []struct {
		name      string
		event     string
		body      string
		operation string
		ignored   bool
	}{
		{
			name:      "ordinary issue comment",
			event:     WebhookEventIssueComment,
			body:      `{"repository":{"id":77},"issue":{"number":9}}`,
			operation: WebhookOperationIssues,
		},
		{
			name:      "pull request issue comment",
			event:     WebhookEventIssueComment,
			body:      `{"repository":{"id":77},"issue":{"number":9,"pull_request":{"url":"x"}}}`,
			operation: WebhookOperationFeedback,
		},
		{
			name:      "completed check run",
			event:     WebhookEventCheckRun,
			body:      `{"repository":{"id":77},"check_run":{"status":"completed","head_sha":"` + validSHA + `","pull_requests":[{"number":12}]}}`,
			operation: WebhookOperationFeedback,
		},
		{
			name:      "pending status",
			event:     WebhookEventStatus,
			body:      `{"repository":{"id":77},"status":{"state":"pending","sha":"` + validSHA + `"}}`,
			operation: WebhookOperationFeedback,
			ignored:   true,
		},
		{
			name:      "successful status",
			event:     WebhookEventStatus,
			body:      `{"repository":{"id":77},"status":{"state":"success","sha":"` + validSHA + `"}}`,
			operation: WebhookOperationFeedback,
		},
		{
			name:      "push",
			event:     WebhookEventPush,
			body:      `{"repository":{"id":77},"push":{"after":"` + validSHA + `"}}`,
			operation: WebhookOperationCommits,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := parseGitHubWebhookPayload(test.event, []byte(test.body))
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if envelope.Operation != test.operation || envelope.Ignored != test.ignored {
				t.Fatalf("envelope = %+v, want operation %q ignored %t", envelope, test.operation, test.ignored)
			}
		})
	}
}

func TestWebhookActivityEndpointIsOwnerScoped(t *testing.T) {
	userID := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	foreignUserID := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000002")
	selectedID := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000001")
	selectedStore := &fakeStore{rows: map[uuid.UUID]SelectedRepository{
		selectedID: {ID: selectedID, UserID: userID, GithubRepositoryID: 77},
	}}
	webhookStore := newMemoryWebhookStore(nil)
	_, _, err := webhookStore.CreateWebhookDelivery(context.Background(), &GitHubWebhookDelivery{
		ID:         uuid.New(),
		DeliveryID: uuid.NewString(),
		Event:      WebhookEventIssues,
		Action:     "opened",
		ReceivedAt: time.Now(),
	}, []GitHubWebhookTarget{{
		ID:                   uuid.New(),
		SelectedRepositoryID: selectedID,
		UserID:               userID,
		Operation:            WebhookOperationIssues,
		IssueNumber:          intPointer(9),
		Status:               WebhookTargetStatusSucceeded,
	}})
	if err != nil {
		t.Fatalf("create activity: %v", err)
	}
	handler := &Handler{service: &Service{store: selectedStore, webhookStore: webhookStore}}
	manager := auth.NewJWTManager([]byte(testJWTSecret))
	router := gin.New()
	router.GET("/repositories/:id/webhooks", auth.RequireAuth(manager), handler.ListWebhookActivity)

	issueToken := func(id uuid.UUID) string {
		token, issueErr := manager.Issue(id, time.Hour)
		if issueErr != nil {
			t.Fatalf("issue token: %v", issueErr)
		}
		return token
	}
	request := func(user uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/repositories/"+selectedID.String()+"/webhooks?page=1&per_page=5", nil)
		req.Header.Set("Authorization", "Bearer "+issueToken(user))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}

	owned := request(userID)
	if owned.Code != http.StatusOK || !strings.Contains(owned.Body.String(), `"count":1`) || !strings.Contains(owned.Body.String(), `"event":"issues"`) {
		t.Fatalf("owned activity response = %d %s", owned.Code, owned.Body.String())
	}
	foreign := request(foreignUserID)
	if foreign.Code != http.StatusNotFound || !strings.Contains(foreign.Body.String(), "repository_not_found") {
		t.Fatalf("foreign activity response = %d %s", foreign.Code, foreign.Body.String())
	}
}

func TestProcessWebhookTargetRunsExistingPullRequestSync(t *testing.T) {
	fixture := newPRFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(prPageJSON(1)))
	})
	encryptedToken, _ := encryptFixtureToken(t)
	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)
	webhookStore := newMemoryWebhookStore(nil)
	fixture.service.ConfigureWebhookStore(webhookStore)
	targetID := uuid.New()
	_, _, err := webhookStore.CreateWebhookDelivery(context.Background(), &GitHubWebhookDelivery{
		ID:         uuid.New(),
		DeliveryID: uuid.NewString(),
		Event:      WebhookEventPullRequest,
		ReceivedAt: time.Now(),
	}, []GitHubWebhookTarget{{
		ID:                   targetID,
		SelectedRepositoryID: prSelected,
		UserID:               prUserID,
		Operation:            WebhookOperationPullRequests,
		Status:               WebhookTargetStatusReceived,
	}})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	result, err := fixture.service.ProcessWebhookTarget(context.Background(), targetID)

	if err != nil {
		t.Fatalf("process webhook target: %v", err)
	}
	if result.Synced != 1 {
		t.Fatalf("sync result = %+v, want one pull request", result)
	}
	if len(fixture.store.pullRequests) != 1 {
		t.Fatalf("stored pull requests = %d, want 1", len(fixture.store.pullRequests))
	}
}

func TestRepositorySyncJobMarksWebhookTargetSucceeded(t *testing.T) {
	fixture := newPRFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(prPageJSON(1)))
	})
	encryptedToken, _ := encryptFixtureToken(t)
	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)
	webhookStore := newMemoryWebhookStore(nil)
	fixture.service.ConfigureWebhookStore(webhookStore)
	targetID := uuid.New()
	_, _, err := webhookStore.CreateWebhookDelivery(context.Background(), &GitHubWebhookDelivery{
		ID:         uuid.New(),
		DeliveryID: uuid.NewString(),
		Event:      WebhookEventPullRequest,
		ReceivedAt: time.Now(),
	}, []GitHubWebhookTarget{{
		ID:                   targetID,
		SelectedRepositoryID: prSelected,
		UserID:               prUserID,
		Operation:            WebhookOperationPullRequests,
		Status:               WebhookTargetStatusDispatching,
		LeaseOwner:           "webhook-test-dispatcher",
	}})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	jobStore := jobs.NewMemoryStore()
	registry := jobs.NewRegistry()
	RegisterJobDefinitions(registry, fixture.service)
	jobService, err := jobs.NewService(jobStore, jobs.Options{Registry: registry})
	if err != nil {
		t.Fatalf("new jobs service: %v", err)
	}
	job, err := jobService.Enqueue(context.Background(), jobs.EnqueueOptions{
		UserID:               prUserID,
		SelectedRepositoryID: prSelected,
		Type:                 JobTypeRepositorySync,
		Payload:              WebhookSyncPayload{TargetID: targetID},
		IdempotencyKey:       "webhook-target:" + targetID.String(),
	})
	if err != nil {
		t.Fatalf("enqueue repository sync: %v", err)
	}
	marked, err := webhookStore.MarkWebhookTargetEnqueued(context.Background(), targetID, job.ID, "webhook-test-dispatcher", time.Now())
	if err != nil || !marked {
		t.Fatalf("mark target enqueued = %t, err %v", marked, err)
	}
	target, err := webhookStore.GetWebhookTarget(context.Background(), targetID)
	if err != nil {
		t.Fatalf("get enqueued target: %v", err)
	}
	if target.JobID == nil || *target.JobID != job.ID {
		t.Fatalf("target job = %v, want %s", target.JobID, job.ID)
	}
	worker := jobs.NewWorker(jobService, "webhook-test-worker", jobs.WorkerOptions{})
	processed, err := worker.ProcessNow(context.Background(), 1)
	if err != nil {
		t.Fatalf("process repository sync: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed jobs = %d, want 1", processed)
	}
	job, err = jobService.Get(context.Background(), prUserID, prSelected, job.ID)
	if err != nil || job.Status != jobs.StatusSucceeded {
		t.Fatalf("job = %+v, err %v, want succeeded", job, err)
	}
	target, err = webhookStore.GetWebhookTarget(context.Background(), targetID)
	if err != nil || target.Status != WebhookTargetStatusSucceeded {
		t.Fatalf("target = %+v, err %v, want succeeded", target, err)
	}
}

func TestWebhookDispatcherEnqueuesOneDurableSyncJob(t *testing.T) {
	userID := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	selectedID := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000001")
	targetID := uuid.New()
	store := newMemoryWebhookStore(nil)
	deliveryID := uuid.NewString()
	if _, _, err := store.CreateWebhookDelivery(context.Background(), &GitHubWebhookDelivery{
		ID:         uuid.New(),
		DeliveryID: deliveryID,
		Event:      WebhookEventIssues,
		ReceivedAt: time.Now(),
	}, []GitHubWebhookTarget{{
		ID:                   targetID,
		SelectedRepositoryID: selectedID,
		UserID:               userID,
		GithubRepositoryID:   77,
		Operation:            WebhookOperationIssues,
		Status:               WebhookTargetStatusReceived,
	}}); err != nil {
		t.Fatalf("create delivery: %v", err)
	}

	jobStore := jobs.NewMemoryStore()
	registry := jobs.NewRegistry()
	RegisterJobDefinitions(registry, &Service{})
	jobService, err := jobs.NewService(jobStore, jobs.Options{Registry: registry})
	if err != nil {
		t.Fatalf("new jobs service: %v", err)
	}
	dispatcher := NewWebhookDispatcher(store, jobService, "dispatcher-test")
	if err := dispatcher.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("dispatch once: %v", err)
	}
	if err := dispatcher.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("dispatch twice: %v", err)
	}
	jobsList, count, err := jobService.List(context.Background(), userID, selectedID, jobs.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if count != 1 || len(jobsList) != 1 || jobsList[0].Type != JobTypeRepositorySync {
		t.Fatalf("jobs = %+v, count = %d", jobsList, count)
	}
	if !strings.Contains(string(jobsList[0].Payload), targetID.String()) {
		t.Fatalf("job payload = %s, want target reference", jobsList[0].Payload)
	}
}

func TestWebhookDispatcherRecoversExistingJobAfterEnqueueCrashWindow(t *testing.T) {
	userID := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	selectedID := uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000001")
	targetID := uuid.New()
	store := newMemoryWebhookStore(nil)
	deliveryID := uuid.NewString()
	_, _, err := store.CreateWebhookDelivery(context.Background(), &GitHubWebhookDelivery{
		ID:         uuid.New(),
		DeliveryID: deliveryID,
		Event:      WebhookEventIssues,
		ReceivedAt: time.Now(),
	}, []GitHubWebhookTarget{{
		ID:                   targetID,
		SelectedRepositoryID: selectedID,
		UserID:               userID,
		Operation:            WebhookOperationIssues,
		Status:               WebhookTargetStatusReceived,
	}})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	jobStore := jobs.NewMemoryStore()
	registry := jobs.NewRegistry()
	RegisterJobDefinitions(registry, &Service{})
	jobService, err := jobs.NewService(jobStore, jobs.Options{Registry: registry})
	if err != nil {
		t.Fatalf("new jobs service: %v", err)
	}
	if _, err := jobService.Enqueue(context.Background(), jobs.EnqueueOptions{
		UserID:               userID,
		SelectedRepositoryID: selectedID,
		Type:                 JobTypeRepositorySync,
		Payload:              WebhookSyncPayload{TargetID: targetID},
		IdempotencyKey:       "webhook-target:" + targetID.String(),
	}); err != nil {
		t.Fatalf("pre-enqueue job: %v", err)
	}
	dispatcher := NewWebhookDispatcher(store, jobService, "dispatcher-test")
	if err := dispatcher.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("dispatch recovery: %v", err)
	}
	jobsList, count, err := jobService.List(context.Background(), userID, selectedID, jobs.ListOptions{})
	if err != nil || count != 1 || len(jobsList) != 1 {
		t.Fatalf("recovered jobs = %+v count %d err %v", jobsList, count, err)
	}
}
