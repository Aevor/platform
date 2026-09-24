package repositories

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/repoinfo"
)

// intelligenceHTTPResponse mirrors the handler DTO so endpoint tests decode the
// exact external shape (including the labeled profile).
type intelligenceHTTPResponse struct {
	RepositoryID uuid.UUID        `json:"repository_id"`
	Revision     string           `json:"revision"`
	Status       string           `json:"status"`
	Stale        bool             `json:"stale"`
	GeneratedAt  time.Time        `json:"generated_at"`
	AINote       string           `json:"ai_note"`
	Profile      repoinfo.Profile `json:"profile"`
}

// newIntelligenceFixture wires the clone fixture (real local Git repo) with the
// intelligence routes. The AI client is the issue-pipeline recording server so
// context injection can be asserted end to end.
func newIntelligenceFixture(t *testing.T) *issuePipelineFixture {
	t.Helper()

	f := newIssuePipelineFileFixture(t)
	f.service.publisher = &fakePublisher{}

	handler := NewHandlerWithJobs(f.service, f.jobService)
	f.router.GET(
		"/repositories/:id/intelligence",
		auth.RequireAuth(f.jwtManager),
		handler.GetIntelligence,
	)
	f.router.POST(
		"/repositories/:id/intelligence/refresh",
		auth.RequireAuth(f.jwtManager),
		handler.RefreshIntelligence,
	)

	return f
}

func intelligenceToken(t *testing.T, f *issuePipelineFixture) string {
	t.Helper()

	token, err := f.jwtManager.Issue(cloneUserID, time.Hour)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	return token
}

func intelligenceRequest(
	t *testing.T,
	f *issuePipelineFixture,
	token string,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, path, reader)
	recorder := httptest.NewRecorder()

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	f.router.ServeHTTP(recorder, request)

	return recorder
}

func decodeIntelligence(t *testing.T, recorder *httptest.ResponseRecorder) intelligenceHTTPResponse {
	t.Helper()

	var response intelligenceHTTPResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode intelligence response %q: %v", recorder.Body.String(), err)
	}

	return response
}

// refreshIntelligenceCall posts /repositories/:id/intelligence/refresh. The
// endpoint enqueues a repository.intelligence_refresh job and ACKs with HTTP
// 202; the helper drains the worker and returns the refreshed profile through
// GET — exactly how a client observes the async refresh — so every call site
// keeps asserting the profile contract in the same shape. Rejection responses
// (401/400/404/409) are returned untouched; no job is enqueued for them.
func refreshIntelligenceCall(t *testing.T, f *issuePipelineFixture, token, refreshPath, body string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := intelligenceRequest(t, f, token, http.MethodPost, refreshPath, body)
	if recorder.Code != http.StatusAccepted {
		return recorder
	}

	f.drainJobs(t)

	getPath := "/repositories/" + cloneSelected.String() + "/intelligence"
	return intelligenceRequest(t, f, token, http.MethodGet, getPath, "")
}

// TestRefreshIntelligence_EnqueuesJobAndAcks pins the async contract: the POST
// ACKs with the queued job reference and a drained worker leaves the refreshed
// profile readable through GET.
func TestRefreshIntelligence_EnqueuesJobAndAcks(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	recorder := intelligenceRequest(t, f, token, http.MethodPost, refreshPath, "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("refresh status = %d, want 202 body=%s", recorder.Code, recorder.Body.String())
	}

	var ack struct {
		RepositoryID string `json:"repository_id"`
		Status       string `json:"status"`
		Job          struct {
			ID     uuid.UUID `json:"id"`
			Type   string    `json:"type"`
			Status string    `json:"status"`
		} `json:"job"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &ack); err != nil {
		t.Fatalf("decode refresh ack: %v", err)
	}

	if ack.Status != "queued" || ack.Job.Status != "queued" {
		t.Errorf("ack status = %q job=%q, want queued", ack.Status, ack.Job.Status)
	}
	if ack.RepositoryID != cloneSelected.String() {
		t.Errorf("ack repository = %q", ack.RepositoryID)
	}
	if ack.Job.Type != JobTypeIntelligenceRefresh {
		t.Errorf("ack job type = %q, want %q", ack.Job.Type, JobTypeIntelligenceRefresh)
	}
	if ack.Job.ID == uuid.Nil {
		t.Error("ack job id missing")
	}

	f.drainJobs(t)

	getPath := "/repositories/" + cloneSelected.String() + "/intelligence"
	got := decodeIntelligence(t, intelligenceRequest(t, f, token, http.MethodGet, getPath, ""))
	if got.Status != IntelligenceStatusCurrent {
		t.Errorf("status = %q, want current after drain", got.Status)
	}
	if got.Revision != strings.Repeat("a", 40) {
		t.Errorf("revision = %q, want pinned HEAD", got.Revision)
	}
}

// TestRefreshIntelligenceDeterministicThenGet is the primary path: refresh
// builds a revision-bound deterministic profile, persists it, and GET returns
// it as current.
func TestRefreshIntelligenceDeterministicThenGet(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	recorder := refreshIntelligenceCall(t, f, token, refreshPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	refreshed := decodeIntelligence(t, recorder)
	if refreshed.Status != IntelligenceStatusCurrent {
		t.Errorf("status = %q, want %q", refreshed.Status, IntelligenceStatusCurrent)
	}
	if refreshed.Revision != strings.Repeat("a", 40) {
		t.Errorf("revision = %q, want pinned HEAD", refreshed.Revision)
	}
	if refreshed.Stale {
		t.Errorf("fresh profile reported stale")
	}
	if refreshed.Profile.Structure.Files < 2 {
		t.Errorf("files = %d, want >= 2 (main.go + main_test.go)", refreshed.Profile.Structure.Files)
	}
	if refreshed.Profile.Structure.LanguageCounts["Go"] < 2 {
		t.Errorf("go language count = %d, want >= 2", refreshed.Profile.Structure.LanguageCounts["Go"])
	}
	if len(refreshed.Profile.Architecture.Components) != 0 {
		t.Errorf("deterministic refresh must not fabricate architecture: %+v",
			refreshed.Profile.Architecture.Components)
	}

	getPath := "/repositories/" + cloneSelected.String() + "/intelligence"

	getRecorder := intelligenceRequest(t, f, token, http.MethodGet, getPath, "")
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}

	got := decodeIntelligence(t, getRecorder)
	if got.Revision != refreshed.Revision || got.Status != refreshed.Status {
		t.Errorf("GET mismatch: %+v vs %+v", got, refreshed)
	}

	stored, err := f.store.GetIntelligence(cloneSelected)
	if err != nil || stored == nil {
		t.Fatalf("profile not persisted: %v %v", stored, err)
	}
	if stored.ProfileJSON == "" {
		t.Errorf("persisted profile JSON empty")
	}
}

// TestIntelligenceMissingThenRefresh verifies GET before any refresh returns an
// opaque not-found that the owner remediates via refresh.
func TestIntelligenceMissingThenRefresh(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	getPath := "/repositories/" + cloneSelected.String() + "/intelligence"

	recorder := intelligenceRequest(t, f, token, http.MethodGet, getPath, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "intelligence_not_found") {
		t.Errorf("body = %s, want intelligence_not_found", recorder.Body.String())
	}
}

// TestIntelligenceOwnershipOpaque guards against cross-user probing: a foreign
// or nonexistent repository is an opaque 404 with no ownership signal.
func TestIntelligenceOwnershipOpaque(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	foreignToken, err := f.jwtManager.Issue(cloneForeignID, time.Hour)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/repositories/" + cloneSelected.String() + "/intelligence"},
		{http.MethodPost, "/repositories/" + cloneSelected.String() + "/intelligence/refresh"},
		{http.MethodGet, "/repositories/" + uuid.New().String() + "/intelligence"},
		{http.MethodPost, "/repositories/" + uuid.New().String() + "/intelligence/refresh"},
	}

	for _, item := range paths {
		recorder := intelligenceRequest(t, f, foreignToken, item.method, item.path, "")
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404 body=%s", item.method, item.path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "repository_not_found") {
			t.Errorf("%s %s body = %s, want repository_not_found", item.method, item.path, recorder.Body.String())
		}
	}
}

// TestIntelligenceStalenessIsDerived verifies staleness is a live comparison of
// the pinned revision against current HEAD, never a stored value.
func TestIntelligenceStalenessIsDerived(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"
	getPath := "/repositories/" + cloneSelected.String() + "/intelligence"

	if recorder := refreshIntelligenceCall(t, f, token, refreshPath, ""); recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	// HEAD advances: the persisted profile must now be reported stale.
	f.service.publisher = &fakePublisher{headSHA: strings.Repeat("b", 40)}

	recorder := intelligenceRequest(t, f, token, http.MethodGet, getPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	got := decodeIntelligence(t, recorder)
	if !got.Stale {
		t.Errorf("expected stale after HEAD advance; revision=%q", got.Revision)
	}

	// Refreshing re-pins to the new HEAD and clears staleness.
	if refresh := refreshIntelligenceCall(t, f, token, refreshPath, ""); refresh.Code != http.StatusOK {
		t.Fatalf("re-refresh status = %d body=%s", refresh.Code, refresh.Body.String())
	}

	reloaded := decodeIntelligence(t, intelligenceRequest(t, f, token, http.MethodGet, getPath, ""))
	if reloaded.Stale {
		t.Errorf("expected fresh after re-pin; revision=%q", reloaded.Revision)
	}
	if reloaded.Revision != strings.Repeat("b", 40) {
		t.Errorf("revision = %q, want new HEAD", reloaded.Revision)
	}
}

// TestRefreshIntelligenceIncludeAI verifies the AI architecture layer is folded
// in and labeled inferred, while deterministic facts stay authoritative.
func TestRefreshIntelligenceIncludeAI(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	var received *ai.AnalyzeRepositoryRequest

	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/analyze-repository" {
			http.NotFound(w, r)
			return
		}

		request := &ai.AnalyzeRepositoryRequest{}
		if err := json.NewDecoder(r.Body).Decode(request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		received = request

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"overview": "A small Go demo repository.",
			"components": [{"name": "demo", "kind": "package", "path": ".", "description": "demo package"}],
			"relationships": [{"from": "demo", "to": "fmt", "kind": "uses"}],
			"conventions": [{"name": "table tests", "description": "tests use tables"}],
			"engineering_decisions": ["keep it small"],
			"databases": [],
			"external_integrations": ["github"],
			"uncertainty": ["runtime behavior unverified"],
			"status": "complete"
		}`))
	}))
	t.Cleanup(aiServer.Close)

	f.service.aiClient = ai.NewClient(nil, ai.WithBaseURL(aiServer.URL))

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	recorder := refreshIntelligenceCall(t, f, token, refreshPath, `{"include_ai":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	got := decodeIntelligence(t, recorder)
	if got.Status != IntelligenceStatusCurrent {
		t.Errorf("status = %q, want current", got.Status)
	}
	if got.AINote != "" {
		t.Errorf("ai_note = %q, want empty on success", got.AINote)
	}
	if got.Profile.Architecture.Overview == "" {
		t.Errorf("architecture overview missing after AI enrichment")
	}
	if len(got.Profile.Structure.LanguageCounts) == 0 {
		t.Errorf("deterministic structure lost during AI enrichment")
	}
	for _, component := range got.Profile.Architecture.Components {
		if component.Level != repoinfo.LevelInferred {
			t.Errorf("component level = %q, want inferred", component.Level)
		}
	}
	for _, statement := range got.Profile.Architecture.ExternalIntegrations {
		if statement.Level != repoinfo.LevelInferred {
			t.Errorf("statement level = %q, want inferred", statement.Level)
		}
	}

	if received == nil {
		t.Fatal("AI service never received the structure request")
	}
	if received.RepositoryID != cloneSelected.String() {
		t.Errorf("repository_id = %q", received.RepositoryID)
	}
	if received.Structure.Languages[0] != "Go" {
		t.Errorf("languages = %v, want Go first", received.Structure.Languages)
	}
	if received.Structure.Files < 2 {
		t.Errorf("files = %d, want >= 2", received.Structure.Files)
	}
}

// TestRefreshIntelligenceAIUnavailableDegrades verifies deterministic
// intelligence is never blocked behind a broken AI dependency.
func TestRefreshIntelligenceAIUnavailableDegrades(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(aiServer.Close)

	f.service.aiClient = ai.NewClient(nil, ai.WithBaseURL(aiServer.URL))

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	recorder := refreshIntelligenceCall(t, f, token, refreshPath, `{"include_ai":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	got := decodeIntelligence(t, recorder)
	if got.Status != IntelligenceStatusAIUnavailable {
		t.Errorf("status = %q, want %q", got.Status, IntelligenceStatusAIUnavailable)
	}
	if got.AINote == "" {
		t.Errorf("ai_note empty, want degradation note")
	}
	if got.Profile.Structure.Files < 2 {
		t.Errorf("deterministic structure lost on AI failure: %+v", got.Profile.Structure)
	}
	if len(got.Profile.Architecture.Components) != 0 {
		t.Errorf("failed AI must not fabricate architecture")
	}
}

// TestRepositoryContextInjectedIntoIssueAnalysis verifies the persisted
// deterministic profile reaches the AI request as bounded context.
func TestRepositoryContextInjectedIntoIssueAnalysis(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	if recorder := refreshIntelligenceCall(t, f, token, refreshPath, ""); recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	issueID := f.seedIssue(t, "Alpha mishandles X")
	analyzePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"

	if recorder := issuePipelineCall(t, f.cloneFixture, token, analyzePath); recorder.Code != http.StatusOK {
		t.Fatalf("analyze status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	var analyzeCall *recordedIssueCall
	for _, call := range f.calls() {
		if call.Analyze != nil {
			analyzeCall = call
		}
	}
	if analyzeCall == nil {
		t.Fatal("no analyze issue call recorded")
	}
	if analyzeCall.Analyze.RepositoryContext == nil {
		t.Fatal("repository context missing from issue analysis request")
	}
	if analyzeCall.Analyze.RepositoryContext.DeterministicSummary == "" {
		t.Errorf("deterministic summary empty")
	}
	if len(analyzeCall.Analyze.RepositoryContext.Languages) == 0 ||
		analyzeCall.Analyze.RepositoryContext.Languages[0] != "Go" {
		t.Errorf("languages = %v, want Go", analyzeCall.Analyze.RepositoryContext.Languages)
	}
}

// TestRepositoryContextOmittedWhenStale verifies stale context is never
// silently attached to an AI request.
func TestRepositoryContextOmittedWhenStale(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	refreshPath := "/repositories/" + cloneSelected.String() + "/intelligence/refresh"

	if recorder := refreshIntelligenceCall(t, f, token, refreshPath, ""); recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	// Advance HEAD without refreshing the profile.
	f.service.publisher = &fakePublisher{headSHA: strings.Repeat("c", 40)}

	issueID := f.seedIssue(t, "Alpha mishandles X")
	analyzePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"

	if recorder := issuePipelineCall(t, f.cloneFixture, token, analyzePath); recorder.Code != http.StatusOK {
		t.Fatalf("analyze status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	for _, call := range f.calls() {
		if call.Analyze != nil && call.Analyze.RepositoryContext != nil {
			t.Errorf("stale repository context was attached: %+v", call.Analyze.RepositoryContext)
		}
	}
}

// TestRepositoryContextOmittedWhenAbsent verifies no context is sent when no
// profile exists yet.
func TestRepositoryContextOmittedWhenAbsent(t *testing.T) {
	f := newIntelligenceFixture(t)
	f.seedWorkspace(t)

	token := intelligenceToken(t, f)
	issueID := f.seedIssue(t, "Alpha mishandles X")
	analyzePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"

	if recorder := issuePipelineCall(t, f.cloneFixture, token, analyzePath); recorder.Code != http.StatusOK {
		t.Fatalf("analyze status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	for _, call := range f.calls() {
		if call.Analyze != nil && call.Analyze.RepositoryContext != nil {
			t.Errorf("context attached without a persisted profile: %+v", call.Analyze.RepositoryContext)
		}
	}
}
