package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// validRequest returns an AnalyzeRequest that passes validateRequest.
func validRequest() *AnalyzeRequest {
	return &AnalyzeRequest{
		RepositoryID:   "bbbbbbbb-0f00-0000-0000-000000000001",
		RepositoryName: "octocat/hello-world",
		Query:          "how does Alpha work?",
		ContextChunks: []ContextChunk{
			{
				ID:         "chunk-1",
				FilePath:   "main.go",
				Language:   "go",
				FileRole:   "source",
				ChunkIndex: 0,
				StartLine:  4,
				EndLine:    6,
				Content:    "func Alpha() {\n\treturn 1\n}",
				SymbolType: "function",
			},
		},
	}
}

// captureServer returns a server that responds with body (status 200 by
// default) and optionally invokes capture with the received request headers
// and raw body.
func captureServer(t *testing.T, status int, body string, capture func(r *http.Request, raw []byte)) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)

		if err != nil {
			b = nil
		}

		if capture != nil {
			capture(r, b)
		}

		if status != 0 {
			w.WriteHeader(status)
		}

		_, _ = w.Write([]byte(body))
	}))
}

func TestNewClientDefaults(t *testing.T) {
	client := NewClient(nil)

	if client.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want default %q", client.baseURL, defaultBaseURL)
	}

	if client.apiKey != "" {
		t.Errorf("apiKey = %q, want empty", client.apiKey)
	}

	if client.httpClient.Timeout != clientTimeout {
		t.Errorf("timeout = %v, want %v", client.httpClient.Timeout, clientTimeout)
	}
}

func TestNewClientOptions(t *testing.T) {
	custom := &http.Client{Timeout: 7 * time.Second}
	client := NewClient(custom, WithBaseURL("http://ai.internal:9000"), WithAPIKey("secret-key-123"))

	if client.baseURL != "http://ai.internal:9000" {
		t.Errorf("baseURL = %q", client.baseURL)
	}

	if client.apiKey != "secret-key-123" {
		t.Errorf("apiKey = %q", client.apiKey)
	}

	if client.httpClient.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s preserved", client.httpClient.Timeout)
	}
}

func TestAnalyze_SuccessfulRequestShape(t *testing.T) {
	var received map[string]interface{}
	var authHeader, userAgent, requestPath string

	server := captureServer(t, 0, `{"summary":"done","insights":[],"status":"analyzed"}`,
		func(r *http.Request, raw []byte) {
			requestPath = r.URL.Path
			authHeader = r.Header.Get("Authorization")
			userAgent = r.Header.Get("User-Agent")
			_ = json.Unmarshal(raw, &received)
		})
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL), WithAPIKey("server-key-xyz"))

	request := validRequest()

	response, err := client.Analyze(context.Background(), request)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if requestPath != defaultEndpoint {
		t.Errorf("path = %q, want %q", requestPath, defaultEndpoint)
	}

	if authHeader != "Bearer server-key-xyz" {
		t.Errorf("Authorization = %q, want Bearer server-key-xyz", authHeader)
	}

	if userAgent != defaultUserAgent {
		t.Errorf("User-Agent = %q, want %q", userAgent, defaultUserAgent)
	}

	if got := received["repository_id"]; got != request.RepositoryID {
		t.Errorf("repository_id = %v, want %s", got, request.RepositoryID)
	}

	if got := received["repository_name"]; got != request.RepositoryName {
		t.Errorf("repository_name = %v, want %s", got, request.RepositoryName)
	}

	if got := received["query"]; got != request.Query {
		t.Errorf("query = %v, want %s", got, request.Query)
	}

	chunks, ok := received["context_chunks"].([]interface{})
	if !ok || len(chunks) != 1 {
		t.Fatalf("context_chunks = %v, want 1 chunk", received["context_chunks"])
	}

	chunk := chunks[0].(map[string]interface{})

	if chunk["id"] != "chunk-1" || chunk["file_path"] != "main.go" || chunk["content"] != request.ContextChunks[0].Content {
		t.Errorf("chunk = %v, want id/file_path/content populated", chunk)
	}

	if response.Summary != "done" || response.Status != "analyzed" {
		t.Errorf("response = %+v, want summary=done status=analyzed", response)
	}
}

func TestAnalyze_NoAuthHeaderWhenNoAPIKey(t *testing.T) {
	var authHeader string

	server := captureServer(t, 0, `{"summary":"done","insights":[],"status":"analyzed"}`,
		func(r *http.Request, raw []byte) {
			authHeader = r.Header.Get("Authorization")
		})
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	if _, err := client.Analyze(context.Background(), validRequest()); err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if authHeader != "" {
		t.Errorf("Authorization = %q, want empty when no API key", authHeader)
	}
}

func TestAnalyze_InsightsDecoded(t *testing.T) {
	const body = `{
		"summary":"two findings",
		"status":"analyzed",
		"insights":[
			{"type":"bug","file_path":"main.go","start_line":4,"end_line":6,"message":"possible nil deref","confidence":0.82},
			{"type":"style","file_path":"helper.go","start_line":1,"end_line":2,"message":"unused var","confidence":0.5}
		]
	}`

	server := captureServer(t, 0, body, nil)
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	response, err := client.Analyze(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(response.Insights) != 2 {
		t.Fatalf("insights = %d, want 2", len(response.Insights))
	}

	first := response.Insights[0]

	if first.Type != "bug" || first.FilePath != "main.go" || first.StartLine != 4 ||
		first.EndLine != 6 || first.Message != "possible nil deref" || first.Confidence != 0.82 {
		t.Errorf("first insight = %+v", first)
	}
}

func TestAnalyze_ErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{}`, wantErr: ErrRateLimited},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`, wantErr: ErrUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{}`, wantErr: ErrUnauthorized},
		{name: "request too large", status: http.StatusRequestEntityTooLarge, body: `{}`, wantErr: ErrRejected},
		{name: "server error 500", status: http.StatusInternalServerError, body: `{}`, wantErr: ErrUnavailable},
		{name: "server error 503", status: http.StatusServiceUnavailable, body: `{}`, wantErr: ErrUnavailable},
		{name: "other 4xx", status: http.StatusBadRequest, body: `{}`, wantErr: ErrAPIError},
		{name: "other 3xx", status: http.StatusFound, body: `{}`, wantErr: ErrAPIError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := captureServer(t, tc.status, tc.body, nil)
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.Analyze(context.Background(), validRequest())

			wantErr := false
			if tc.wantErr == ErrUnavailable {
				wantErr = isError(err, ErrUnavailable)
			} else {
				wantErr = isError(err, tc.wantErr)
			}

			if !wantErr {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestAnalyze_InvalidResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `not-json`},
		{name: "missing summary", body: `{"status":"analyzed"}`},
		{name: "missing status", body: `{"summary":"done"}`},
		{name: "missing insight type", body: `{"summary":"done","status":"analyzed","insights":[{"message":"x"}]}`},
		{name: "missing insight message", body: `{"summary":"done","status":"analyzed","insights":[{"type":"bug"}]}`},
		{name: "out of range confidence", body: `{"summary":"done","status":"analyzed","insights":[{"type":"bug","message":"x","confidence":1.5}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := captureServer(t, 0, tc.body, nil)
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.Analyze(context.Background(), validRequest())

			if !isError(err, ErrInvalidResponse) {
				t.Errorf("err = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestAnalyze_OversizedResponseRejected(t *testing.T) {
	// maxResponseSize is exposed only as a constant; build a body one byte
	// larger to force ErrInvalidResponse.
	body := `{"summary":"` + strings.Repeat("x", maxResponseSize) + `","status":"analyzed"}`

	server := captureServer(t, 0, body, nil)
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.Analyze(context.Background(), validRequest())

	if !isError(err, ErrInvalidResponse) {
		t.Errorf("err = %v, want ErrInvalidResponse for oversized body", err)
	}
}

func TestAnalyze_NetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("force close")
	}))
	server.Close() // close so requests fail to connect

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.Analyze(context.Background(), validRequest())

	if !isError(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable on network failure", err)
	}
}

func TestAnalyze_Timeout(t *testing.T) {
	server := captureServer(t, 0, `{"summary":"done","insights":[],"status":"analyzed"}`,
		func(r *http.Request, raw []byte) {
			time.Sleep(200 * time.Millisecond)
		})
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))
	client.httpClient.Timeout = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := client.Analyze(ctx, validRequest())

	if !isError(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

func TestAnalyze_RejectedRequestValidation(t *testing.T) {
	server := captureServer(t, 0, `{"summary":"done","insights":[],"status":"analyzed"}`,
		func(r *http.Request, raw []byte) {
			t.Error("server should not be contacted for invalid request")
		})
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	t.Run("empty repository id", func(t *testing.T) {
		request := validRequest()
		request.RepositoryID = ""

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("empty repository name", func(t *testing.T) {
		request := validRequest()
		request.RepositoryName = ""

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("empty query", func(t *testing.T) {
		request := validRequest()
		request.Query = "  "

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("query too long", func(t *testing.T) {
		request := validRequest()
		request.Query = strings.Repeat("a", maxQueryLength+1)

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("too many chunks", func(t *testing.T) {
		request := validRequest()
		for i := 0; i < maxContextChunks+1; i++ {
			request.ContextChunks = append(request.ContextChunks, ContextChunk{
				ID:       fmt.Sprintf("c%d", i),
				FilePath: "f.go",
			})
		}

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("chunk missing id", func(t *testing.T) {
		request := validRequest()
		request.ContextChunks[0].ID = ""

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("chunk missing file path", func(t *testing.T) {
		request := validRequest()
		request.ContextChunks[0].FilePath = ""

		_, err := client.Analyze(context.Background(), request)

		if !isError(err, ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})
}

func TestSanitizeRequest_TruncatesContent(t *testing.T) {
	request := validRequest()
	request.ContextChunks[0].Content = strings.Repeat("y", maxChunkContent+100)

	sanitized := sanitizeRequest(request)

	if len(sanitized.ContextChunks[0].Content) != maxChunkContent {
		t.Errorf("content = %d bytes, want %d", len(sanitized.ContextChunks[0].Content), maxChunkContent)
	}

	// The original is untouched and the metadata survives.
	if len(request.ContextChunks[0].Content) != maxChunkContent+100 {
		t.Errorf("original content modified: %d bytes", len(request.ContextChunks[0].Content))
	}

	if sanitized.ContextChunks[0].FilePath != "main.go" {
		t.Errorf("metadata lost during sanitize: %+v", sanitized.ContextChunks[0])
	}
}

func TestSanitizeRequest_NoTruncationWhenWithinLimit(t *testing.T) {
	request := validRequest()
	request.ContextChunks[0].Content = "small"

	sanitized := sanitizeRequest(request)

	if sanitized.ContextChunks[0].Content != "small" {
		t.Errorf("content = %q, want unchanged", sanitized.ContextChunks[0].Content)
	}
}

func TestAnalyze_SecretAPIKeyNeverInBody(t *testing.T) {
	var rawBody, authHeader string

	server := captureServer(t, 0, `{"summary":"done","insights":[],"status":"analyzed"}`,
		func(r *http.Request, raw []byte) {
			rawBody = string(raw)
			authHeader = r.Header.Get("Authorization")
		})
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL), WithAPIKey("api-key-ABCDEF"))

	response, err := client.Analyze(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	_ = response

	// The API key is carried ONLY as the Authorization header, never in the
	// request body or anywhere that could be logged as part of a payload.
	if authHeader != "Bearer api-key-ABCDEF" {
		t.Errorf("Authorization = %q, want Bearer api-key-ABCDEF", authHeader)
	}

	if strings.Contains(rawBody, "api-key-ABCDEF") {
		t.Error("API key leaked into request body")
	}
}

// isError reports whether err wraps target (including direct equality).
func isError(err error, target error) bool {
	return err != nil && (err == target || errors.Is(err, target))
}
