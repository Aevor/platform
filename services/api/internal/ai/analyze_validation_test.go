package ai

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

const validAnalyzeValidationJSON = `{
	"summary": "The gofmt formatting check found unformatted files.",
	"root_cause": "The new main.go was not formatted with gofmt.",
	"suggestions": ["Run gofmt -w on the changed files."],
	"status": "analyzed"
}`

func validValidationRequest() *AnalyzeValidationFailureRequest {
	return &AnalyzeValidationFailureRequest{
		RepositoryID:   "bbbbbbbb-0f00-0000-0000-000000000001",
		RepositoryName: "octocat/hello-world",
		Language:       "Go",
		Toolchains:     []string{"go"},
		FailureClass:   "code",
		Summary:        "4 controlled check(s) failed.",
		Checks: []ValidationFailureCheck{
			{
				Name:         "go build ./...",
				Status:       "FAIL",
				Stage:        "build",
				FailureClass: "code",
				Message:      "exit status 1: undefined: Foo",
			},
		},
	}
}

func TestAnalyzeValidationFailure_SuccessfulRequestShape(t *testing.T) {
	var requestPath string

	server := captureServer(t, 0, validAnalyzeValidationJSON, func(r *http.Request, raw []byte) {
		requestPath = r.URL.Path

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		received := &AnalyzeValidationFailureRequest{}
		if err := json.Unmarshal(raw, received); err != nil {
			t.Fatalf("decode request %s: %v", raw, err)
		}
		if received.RepositoryID != "bbbbbbbb-0f00-0000-0000-000000000001" {
			t.Errorf("repository_id = %q", received.RepositoryID)
		}
		if received.RepositoryName != "octocat/hello-world" {
			t.Errorf("repository_name = %q", received.RepositoryName)
		}
		if len(received.Toolchains) != 1 || received.Toolchains[0] != "go" {
			t.Errorf("toolchains = %v", received.Toolchains)
		}
		if len(received.Checks) != 1 || received.Checks[0].Name != "go build ./..." {
			t.Errorf("checks = %+v", received.Checks)
		}
		if received.Checks[0].Status != "FAIL" {
			t.Errorf("check status = %q", received.Checks[0].Status)
		}
	})
	t.Cleanup(server.Close)

	client := NewClient(nil, WithBaseURL(server.URL))
	response, err := client.AnalyzeValidationFailure(t.Context(), validValidationRequest())
	if err != nil {
		t.Fatalf("AnalyzeValidationFailure: %v", err)
	}

	if requestPath != defaultAnalyzeValidation {
		t.Errorf("path = %q, want %q", requestPath, defaultAnalyzeValidation)
	}

	if response.Summary == "" {
		t.Errorf("summary empty")
	}
	if response.RootCause == "" {
		t.Errorf("root_cause empty")
	}
	if len(response.Suggestions) != 1 || response.Suggestions[0] == "" {
		t.Errorf("suggestions = %v", response.Suggestions)
	}
	if response.Status != "analyzed" {
		t.Errorf("status = %q", response.Status)
	}
}

func TestAnalyzeValidationFailure_RequestValidationBoundary(t *testing.T) {
	tooMany := make([]ValidationFailureCheck, maxValidationChecks+1)
	for i := range tooMany {
		tooMany[i] = ValidationFailureCheck{Name: "check", Status: "FAIL"}
	}

	base := validValidationRequest()
	for _, tc := range []struct {
		name    string
		mutate  func(r *AnalyzeValidationFailureRequest)
		wantErr bool
	}{
		{"valid", func(r *AnalyzeValidationFailureRequest) {}, false},
		{"missing repository id", func(r *AnalyzeValidationFailureRequest) { r.RepositoryID = "" }, true},
		{"missing repository name", func(r *AnalyzeValidationFailureRequest) { r.RepositoryName = " " }, true},
		{"missing check name", func(r *AnalyzeValidationFailureRequest) { r.Checks[0].Name = "" }, true},
		{"missing check status", func(r *AnalyzeValidationFailureRequest) { r.Checks[0].Status = "" }, true},
		{"too many checks", func(r *AnalyzeValidationFailureRequest) { r.Checks = tooMany }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := captureServer(t, 0, validAnalyzeValidationJSON, nil)
			t.Cleanup(server.Close)

			client := NewClient(nil, WithBaseURL(server.URL))
			request := *base
			tc.mutate(&request)

			_, err := client.AnalyzeValidationFailure(t.Context(), &request)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAnalyzeValidationFailure_ResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid", validAnalyzeValidationJSON, false},
		{"missing summary", `{"root_cause":"x","suggestions":[],"status":"analyzed"}`, true},
		{"missing status", `{"summary":"x","root_cause":"x","suggestions":[]}`, true},
		{"malformed", `not json`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := captureServer(t, 0, tc.body, nil)
			t.Cleanup(server.Close)

			client := NewClient(nil, WithBaseURL(server.URL))
			_, err := client.AnalyzeValidationFailure(t.Context(), validValidationRequest())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAnalyzeValidationFailure_ErrorTaxonomy(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		wantErr error
	}{
		{http.StatusUnauthorized, `{"error":"missing"}`, ErrUnauthorized},
		{http.StatusTooManyRequests, `{"error":"slow down"}`, ErrRateLimited},
		{http.StatusServiceUnavailable, `{"error":"down"}`, ErrUnavailable},
		{http.StatusBadGateway, `{"error":"upstream"}`, ErrUnavailable},
	} {
		server := captureServer(t, tc.status, tc.body, nil)
		t.Cleanup(server.Close)

		client := NewClient(nil, WithBaseURL(server.URL))
		_, err := client.AnalyzeValidationFailure(t.Context(), validValidationRequest())
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("status %d: got %v, want %v", tc.status, err, tc.wantErr)
		}
	}
}

func TestAnalyzeValidationFailure_RequestTooLargeIsRejected(t *testing.T) {
	server := captureServer(t, http.StatusRequestEntityTooLarge, `{}`, nil)
	t.Cleanup(server.Close)

	client := NewClient(nil, WithBaseURL(server.URL))
	_, err := client.AnalyzeValidationFailure(t.Context(), validValidationRequest())
	if err == nil {
		t.Fatal("expected error for 413")
	}
	if !errors.Is(err, ErrRejected) {
		t.Errorf("got %v, want %v", err, ErrRejected)
	}
}
