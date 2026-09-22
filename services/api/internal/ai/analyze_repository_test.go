package ai

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

const (
	validAnalyzeRepoJSON = `{
		"overview": "A small Go demo repository with one package.",
		"components": [
			{"name": "demo", "kind": "package", "path": ".", "description": "the demo package", "responsibilities": ["entry point"]}
		],
		"relationships": [
			{"from": "demo", "to": "fmt", "kind": "uses"}
		],
		"conventions": [
			{"name": "table tests", "description": "tests use tables", "evidence": ["a_test.go"]}
		],
		"engineering_decisions": ["keep it small"],
		"databases": [],
		"external_integrations": ["github"],
		"uncertainty": ["runtime behavior unverified"],
		"status": "complete"
	}`
)

func validRepositoryRequest() *AnalyzeRepositoryRequest {
	return &AnalyzeRepositoryRequest{
		RepositoryID:   "bbbbbbbb-0f00-0000-0000-000000000001",
		RepositoryName: "octocat/hello-world",
		Structure: RepositoryStructure{
			Languages:   []string{"Go"},
			Files:       4,
			Chunks:      7,
			EntryPoints: []string{"cmd/api/main.go(application)"},
			AppScopes:   []string{"cmd/api(app)"},
			ConfigFiles: []string{"config.yaml"},
			TestFiles:   []string{"main_test.go"},
			BuildFiles:  []string{"go.mod"},
		},
	}
}

func TestAnalyzeRepository_SuccessfulRequestShape(t *testing.T) {
	var requestPath string

	server := captureServer(t, 0, validAnalyzeRepoJSON, func(r *http.Request, raw []byte) {
		requestPath = r.URL.Path

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		received := &AnalyzeRepositoryRequest{}
		if err := json.Unmarshal(raw, received); err != nil {
			t.Fatalf("decode request %s: %v", raw, err)
		}
		if received.RepositoryID != "bbbbbbbb-0f00-0000-0000-000000000001" {
			t.Errorf("repository_id = %q", received.RepositoryID)
		}
		if len(received.Structure.Languages) != 1 || received.Structure.Languages[0] != "Go" {
			t.Errorf("languages = %v", received.Structure.Languages)
		}
		if received.Structure.Files != 4 || received.Structure.Chunks != 7 {
			t.Errorf("files/chunks = %d/%d", received.Structure.Files, received.Structure.Chunks)
		}
	})
	t.Cleanup(server.Close)

	client := NewClient(nil, WithBaseURL(server.URL))
	response, err := client.AnalyzeRepository(t.Context(), validRepositoryRequest())
	if err != nil {
		t.Fatalf("AnalyzeRepository: %v", err)
	}

	if requestPath != defaultAnalyzeRepository {
		t.Errorf("path = %q, want %q", requestPath, defaultAnalyzeRepository)
	}

	if response.Overview == "" {
		t.Errorf("overview empty")
	}
	if len(response.Components) != 1 || response.Components[0].Name != "demo" {
		t.Errorf("components = %+v", response.Components)
	}
	if len(response.Relationships) != 1 || response.Relationships[0].From != "demo" {
		t.Errorf("relationships = %+v", response.Relationships)
	}
	if len(response.Conventions) != 1 || response.Conventions[0].Name != "table tests" {
		t.Errorf("conventions = %+v", response.Conventions)
	}
	if response.Status != "complete" {
		t.Errorf("status = %q", response.Status)
	}
}

func TestAnalyzeRepository_SecretAPIKeyNeverInBody(t *testing.T) {
	var rawBody []byte

	server := captureServer(t, 0, validAnalyzeRepoJSON, func(_ *http.Request, raw []byte) {
		rawBody = raw
	})
	t.Cleanup(server.Close)

	client := NewClient(nil, WithBaseURL(server.URL), WithAPIKey("super-secret-repo-key"))
	if _, err := client.AnalyzeRepository(t.Context(), validRepositoryRequest()); err != nil {
		t.Fatalf("AnalyzeRepository: %v", err)
	}

	if len(rawBody) == 0 {
		t.Fatal("request body empty")
	}
	var captured map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &captured); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if _, ok := captured["api_key"]; ok {
		t.Errorf("api key leaked into request body")
	}
}

func TestAnalyzeRepository_RequestValidation(t *testing.T) {
	client := NewClient(nil, WithBaseURL("http://127.0.0.1:1"))

	request := validRepositoryRequest()
	request.RepositoryID = ""

	_, err := client.AnalyzeRepository(t.Context(), request)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}

	request = validRepositoryRequest()
	request.RepositoryName = ""

	_, err = client.AnalyzeRepository(t.Context(), request)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

func TestAnalyzeRepository_ResponseValidation(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{
			name: "missing overview",
			json: `{"status":"complete"}`,
		},
		{
			name: "missing status",
			json: `{"overview":"x"}`,
		},
		{
			name: "missing component name",
			json: `{"overview":"x","status":"complete","components":[{"kind":"package"}]}`,
		},
		{
			name: "missing relationship end",
			json: `{"overview":"x","status":"complete","relationships":[{"from":"a","kind":"uses"}]}`,
		},
		{
			name: "too many components",
			json: `{"overview":"x","status":"complete","components":` + manyRepositoryComponents(maxRepositoryEntries+1) + `}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := captureServer(t, 0, tc.json, nil)
			t.Cleanup(server.Close)

			client := NewClient(nil, WithBaseURL(server.URL))
			_, err := client.AnalyzeRepository(t.Context(), validRepositoryRequest())
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestAnalyzeRepository_ErrorTaxonomy(t *testing.T) {
	tests := []struct {
		status   int
		body     string
		checkTax func(error) bool
	}{
		{status: http.StatusBadRequest, body: `{"error":"bad"}`, checkTax: func(err error) bool { return errors.Is(err, ErrAPIError) }},
		{status: http.StatusUnauthorized, body: `{"error":"nope"}`, checkTax: func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{status: http.StatusGatewayTimeout, body: "timeout", checkTax: func(err error) bool { return errors.Is(err, ErrUnavailable) }},
	}

	for _, tc := range tests {
		server := captureServer(t, tc.status, tc.body, nil)
		t.Cleanup(server.Close)

		client := NewClient(nil, WithBaseURL(server.URL))
		_, err := client.AnalyzeRepository(t.Context(), validRepositoryRequest())
		if err == nil {
			t.Fatalf("status %d: no error", tc.status)
		}
		if !tc.checkTax(err) {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
	}
}

func TestAnalyzeRepository_NetworkFailureAndTimeout(t *testing.T) {
	client := NewClient(nil, WithBaseURL("http://127.0.0.1:1"))
	if _, err := client.AnalyzeRepository(t.Context(), validRepositoryRequest()); err == nil {
		t.Fatalf("network failure returned nil error")
	}
}

func TestSanitizeRepositoryContext(t *testing.T) {
	sanitized := sanitizeRepositoryContext(nil)
	if sanitized != nil {
		t.Errorf("nil context sanitized to %v", sanitized)
	}

	oversized := stringsRepeat("x", maxRepositoryContextText+50)
	context := &RepositoryContext{
		DeterministicSummary: oversized,
		Languages:            stringsA(maxRepositoryContextStrings + 5),
		EntryPoints:          stringsA(3),
	}

	sanitized = sanitizeRepositoryContext(context)
	if len(sanitized.DeterministicSummary) != maxRepositoryContextText {
		t.Errorf("summary length = %d, want %d", len(sanitized.DeterministicSummary), maxRepositoryContextText)
	}
	if len(sanitized.Languages) != maxRepositoryContextStrings {
		t.Errorf("languages length = %d, want %d", len(sanitized.Languages), maxRepositoryContextStrings)
	}
	if len(sanitized.EntryPoints) != 3 {
		t.Errorf("entry points length = %d, want 3", len(sanitized.EntryPoints))
	}

	blank := &RepositoryContext{Languages: []string{"  ", "Go", ""}}
	sanitized = sanitizeRepositoryContext(blank)
	if len(sanitized.Languages) != 1 || sanitized.Languages[0] != "Go" {
		t.Errorf("blank items not trimmed: %v", sanitized.Languages)
	}
}

func manyRepositoryComponents(count int) string {
	body := "["
	for i := 0; i < count; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"name":"c","kind":"package","description":"d"}`
	}
	return body + "]"
}

func stringsRepeat(value string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += value
	}
	return out
}

func stringsA(count int) []string {
	items := make([]string, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, "item")
	}
	return items
}
