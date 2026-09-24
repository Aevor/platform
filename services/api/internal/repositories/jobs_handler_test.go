package repositories

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/jobs"
)

func enqueueFixtureJob(t *testing.T, fixture *cloneFixture, typ string, payload any, key string) *jobs.Job {
	t.Helper()

	job, err := fixture.jobService.Enqueue(context.Background(), jobs.EnqueueOptions{
		UserID:               cloneUserID,
		SelectedRepositoryID: cloneSelected,
		Type:                 typ,
		Payload:              payload,
		IdempotencyKey:       key,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", typ, err)
	}

	return job
}

// failedIntelligenceJob enqueues an intelligence refresh that cannot possibly
// succeed in the fixture (no AI client and no prepared workspace) and drains
// the worker so the job reaches its terminal failed state.
func failedIntelligenceJob(t *testing.T, fixture *cloneFixture) *jobs.Job {
	t.Helper()

	job := enqueueFixtureJob(t, fixture, JobTypeIntelligenceRefresh, intelligenceRefreshPayload{IncludeAI: true}, "test-fail-refresh")
	fixture.drainJobs(t)

	refreshed, err := fixture.jobService.Get(context.Background(), cloneUserID, cloneSelected, job.ID)

	if err != nil {
		t.Fatalf("reload failed job: %v", err)
	}

	if refreshed.Status != jobs.StatusFailed {
		t.Fatalf("status = %s, want failed (category=%s)", refreshed.Status, refreshed.ErrorCategory)
	}

	return refreshed
}

// jobsCall performs an HTTP request against the job routes.
func jobsCall(t *testing.T, fixture *cloneFixture, jwtToken, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, path, nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestListJobs_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := jobsCall(t, fixture, "", http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs")

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign repository is opaque 404", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelectedFg.String()+"/jobs")

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
			t.Errorf("status=%d body=%s, want opaque repository_not_found", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown repository is the same 404", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/00000000-0000-4000-8000-0000000000ff/jobs")

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
			t.Errorf("status=%d body=%s, want opaque repository_not_found", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed repository id is 400", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/not-a-uuid/jobs")

		if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown status filter is 400", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs?status=garbage")

		if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown type filter is 400", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs?type=garbage")

		if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("lists owned jobs with registry lookup", func(t *testing.T) {
		enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "")
		enqueueFixtureJob(t, fixture, JobTypeIntelligenceRefresh, intelligenceRefreshPayload{IncludeAI: false}, "")

		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", recorder.Code, recorder.Body.String())
		}

		var body struct {
			RepositoryID string `json:"repository_id"`
			Jobs         []jobEntry
			Count        int  `json:"count"`
			HasMore      bool `json:"has_more"`
			Status       string
		}

		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list: %v", err)
		}

		if body.RepositoryID != cloneSelected.String() || len(body.Jobs) != 2 || body.Count != 2 || body.HasMore || body.Status != "ok" {
			t.Errorf("list = %+v, want 2 jobs for this repository", body)
		}

		gotTypes := make(map[string]bool)

		for _, entry := range body.Jobs {
			gotTypes[entry.Type] = true

			if entry.Description == "" {
				t.Errorf("job %s has no registry description", entry.Type)
			}
		}

		if !gotTypes[JobTypeRepositoryIndex] || !gotTypes[JobTypeIntelligenceRefresh] {
			t.Errorf("types = %v, want index+intelligence refresh", gotTypes)
		}

		if strings.Contains(recorder.Body.String(), `"payload"`) {
			t.Errorf("list response leaks a job payload: %s", recorder.Body.String())
		}
	})

	t.Run("status filter narrows results after drain", func(t *testing.T) {
		enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "filter-succeeded")

		if _, err := fixture.jobWorker.ProcessNow(t.Context(), 100); err != nil {
			t.Fatalf("process: %v", err)
		}

		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs?status="+jobs.StatusSucceeded)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}

		var body jobListResponse

		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		for _, entry := range body.Jobs {
			if entry.Status != jobs.StatusSucceeded {
				t.Errorf("filter returned status=%s, want %s", entry.Status, jobs.StatusSucceeded)
			}
		}
	})
}

func TestGetJob_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)
	job := enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "get-me")

	t.Run("owned job returns the safe envelope", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String())

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}

		var body struct {
			Job jobEntry `json:"job"`
		}

		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if body.Job.ID != job.ID || body.Job.Type != JobTypeRepositoryIndex || body.Job.Status != jobs.StatusQueued {
			t.Errorf("job = %+v, want queued index job", body.Job)
		}

		if strings.Contains(recorder.Body.String(), `"payload"`) {
			t.Errorf("job detail leaks a payload: %s", recorder.Body.String())
		}
	})

	t.Run("unknown job on an owned repo is 404 job_not_found", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs/00000000-0000-4000-8000-000000000000")

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"job_not_found"}` {
			t.Errorf("status=%d body=%s, want opaque job_not_found", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign repository is repository_not_found, never job_not_found", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelectedFg.String()+"/jobs/"+job.ID.String())

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
			t.Errorf("status=%d body=%s, want opaque repository_not_found", recorder.Code, recorder.Body.String())
		}
	})
}

func TestListJobEvents_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)
	job := enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "events-empty")

	recorder := jobsCall(t, fixture, token, http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String()+"/events")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		JobID  uuid.UUID         `json:"job_id"`
		Events []json.RawMessage `json:"events"`
		Status string            `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.JobID != job.ID || len(body.Events) != 0 || body.Status != "ok" {
		t.Errorf("body = %+v, want empty timeline for a run-less job", body)
	}
}

func TestCancelJob_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)

	t.Run("cancels an owned queued job", func(t *testing.T) {
		job := enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "cancel-me")

		recorder := jobsCall(t, fixture, token, http.MethodPost, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String()+"/cancel")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}

		var body struct {
			Job jobEntry `json:"job"`
		}

		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if body.Job.Status != jobs.StatusCancelled {
			t.Errorf("status = %s, want %s", body.Job.Status, jobs.StatusCancelled)
		}
	})

	t.Run("terminal jobs are immutably 409 job_not_active", func(t *testing.T) {
		job := enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "cancel-terminal")

		if _, err := fixture.jobWorker.ProcessNow(t.Context(), 100); err != nil {
			t.Fatalf("process: %v", err)
		}

		recorder := jobsCall(t, fixture, token, http.MethodPost, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String()+"/cancel")

		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "job_not_active") {
			t.Errorf("status=%d body=%s, want 409 job_not_active", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown job is 404", func(t *testing.T) {
		recorder := jobsCall(t, fixture, token, http.MethodPost, "/repositories/"+cloneSelected.String()+"/jobs/00000000-0000-4000-8000-000000000000/cancel")

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"job_not_found"}` {
			t.Errorf("status=%d body=%s, want job_not_found", recorder.Code, recorder.Body.String())
		}
	})
}

func TestRetryJob_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)

	t.Run("re-queues an owned failed job", func(t *testing.T) {
		job := failedIntelligenceJob(t, fixture)

		recorder := jobsCall(t, fixture, token, http.MethodPost, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String()+"/retry")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}

		var body struct {
			Job jobEntry `json:"job"`
		}

		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if body.Job.Status != jobs.StatusQueued {
			t.Errorf("status = %s, want %s", body.Job.Status, jobs.StatusQueued)
		}
	})

	t.Run("non-failed jobs cannot be retried", func(t *testing.T) {
		job := enqueueFixtureJob(t, fixture, JobTypeRepositoryIndex, nil, "retry-not-failed")

		recorder := jobsCall(t, fixture, token, http.MethodPost, "/repositories/"+cloneSelected.String()+"/jobs/"+job.ID.String()+"/retry")

		if recorder.Code != http.StatusConflict {
			t.Errorf("status=%d body=%s, want 409", recorder.Code, recorder.Body.String())
		}
	})
}

func TestJobRoutes_NotInitializedIs501(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fixture := newCloneFixture(t, nil)
	token := fixture.tokenFor(t, cloneUserID)

	noJobsHandler := NewHandler(fixture.service)
	router := gin.New()
	router.GET("/repositories/:id/jobs", auth.RequireAuth(fixture.jwtManager), noJobsHandler.ListJobs)

	request := httptest.NewRequest(http.MethodGet, "/repositories/"+cloneSelected.String()+"/jobs", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotImplemented || strings.TrimSpace(recorder.Body.String()) != `{"error":"jobs_not_initialized"}` {
		t.Errorf("status=%d body=%s, want 501 jobs_not_initialized", recorder.Code, recorder.Body.String())
	}
}
