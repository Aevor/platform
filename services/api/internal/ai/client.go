// Package ai defines the boundary types and HTTP client for communicating
// with the external AI analysis service. The AI service lives in a SEPARATE
// repository and handles all model inference, prompt construction, embeddings,
// and vector operations.
//
// This package contains ZERO AI logic. It is the controlled interface through
// which the API sends bounded repository context and receives structured
// analysis results.
//
// Security invariants:
//
//   - Requests carry ONLY: repository identity (UUID, name), language, user
//     query, and bounded metadata chunks. NEVER GitHub tokens, JWT secrets,
//     encryption keys, credentials, or environment variables.
//   - Raw AI output is parsed and validated; callers never receive unstructured
//     text that could leak source code or sensitive information.
//   - The API key is configured server-side only and is never exposed to
//     clients or logged.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrUnavailable     = errors.New("ai_service_unavailable")
	ErrTimeout         = errors.New("ai_service_timeout")
	ErrInvalidResponse = errors.New("ai_invalid_response")
	ErrRateLimited     = errors.New("ai_rate_limited")
	ErrUnauthorized    = errors.New("ai_unauthorized")
	ErrAPIError        = errors.New("ai_api_error")
	ErrRejected        = errors.New("ai_request_rejected")
)

const (
	defaultBaseURL    = "http://localhost:11434"
	defaultUserAgent  = "Aevor/0.1 (https://github.com/Aevor/platform)"
	defaultEndpoint   = "/v1/analyze"
	clientTimeout     = 30 * time.Second
	maxResponseSize   = 4 << 20
	maxContextChunks  = 128
	maxQueryLength    = 4096
	maxChunkContent   = 8192
)

// ContextChunk is the bounded metadata unit sent to the AI service. It
// represents one indexed representation enriched with its source content. The
// API assembles these from index lookups + upstream content retrieval; the AI
// service never accesses the filesystem directly.
type ContextChunk struct {
	ID           string  `json:"id"`
	FilePath     string  `json:"file_path"`
	Language     string  `json:"language"`
	FileRole     string  `json:"file_role"`
	ChunkIndex   int     `json:"chunk_index"`
	StartLine    int     `json:"start_line"`
	EndLine      int     `json:"end_line"`
	Content      string  `json:"content"`
	SymbolName   *string `json:"symbol_name,omitempty"`
	SymbolType   string  `json:"symbol_type"`
	ParentSymbol *string `json:"parent_symbol,omitempty"`
}

// AnalyzeRequest is the controlled payload sent to the AI service. It carries
// repository identity, the user's query, and bounded context chunks. It
// NEVER carries: GitHub tokens, JWT secrets, encryption keys, credentials,
// or environment variables.
type AnalyzeRequest struct {
	RepositoryID   string         `json:"repository_id"`
	RepositoryName string         `json:"repository_name"`
	Language       string         `json:"language,omitempty"`
	Query          string         `json:"query"`
	ContextChunks  []ContextChunk `json:"context_chunks"`
}

// Insight is one structured analysis result. Location fields reference the
// repository-relative file and line range. Confidence is a 0.0–1.0 score
// indicating the AI service's self-assessed certainty.
type Insight struct {
	Type       string  `json:"type"`
	FilePath   string  `json:"file_path"`
	StartLine  int     `json:"start_line"`
	EndLine    int     `json:"end_line"`
	Message    string  `json:"message"`
	Confidence float64 `json:"confidence"`
}

// AnalyzeResponse is the structured result returned by the AI service. The
// API parses and validates this; raw AI output is never exposed directly.
type AnalyzeResponse struct {
	Summary string   `json:"summary"`
	Insights []Insight `json:"insights"`
	Status  string   `json:"status"`
}

// Client communicates with the external AI analysis service over HTTP.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string
}

// ClientOption configures the Client.
type ClientOption func(*Client)

// WithBaseURL overrides the default AI service URL.
func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

// WithAPIKey sets the server-side API key sent as a Bearer token.
func WithAPIKey(apiKey string) ClientOption {
	return func(c *Client) {
		c.apiKey = apiKey
	}
}

// NewClient creates a new AI service client. If httpClient is nil, a default
// client with the standard timeout is used.
func NewClient(httpClient *http.Client, opts ...ClientOption) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	httpClient = &http.Client{
		Transport: httpClient.Transport,
		Jar:       httpClient.Jar,
		Timeout:   httpClient.Timeout,
	}

	if httpClient.Timeout == 0 {
		httpClient.Timeout = clientTimeout
	}

	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	client := &Client{
		baseURL:    defaultBaseURL,
		httpClient: httpClient,
		userAgent:  defaultUserAgent,
	}

	for _, opt := range opts {
		opt(client)
	}

	return client
}

// Analyze sends a bounded analysis request to the AI service and returns the
// structured response. Context chunks are validated and truncated to safe
// limits before transmission. The method never sends source code beyond what
// is explicitly provided in the bounded context chunks.
func (c *Client) Analyze(ctx context.Context, request *AnalyzeRequest) (*AnalyzeResponse, error) {
	if err := validateRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	sanitized := sanitizeRequest(request)

	body, err := json.Marshal(sanitized)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}

	endpoint := c.baseURL + defaultEndpoint

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", c.userAgent)

	if c.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpRequest)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}

		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return nil, fmt.Errorf("%w: request too large", ErrRejected)
	}

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrAPIError, resp.StatusCode)
	}

	var response AnalyzeResponse

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&response); err != nil {
		return nil, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidResponse
	}

	if err := validateResponse(&response); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}

	return &response, nil
}

// validateRequest rejects obviously invalid payloads before they leave the
// API boundary. This catches programming errors, not adversarial input — the
// API is the trust boundary, not the AI service.
func validateRequest(request *AnalyzeRequest) error {
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}

	if strings.TrimSpace(request.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}

	if strings.TrimSpace(request.Query) == "" {
		return fmt.Errorf("query required")
	}

	if len(request.Query) > maxQueryLength {
		return fmt.Errorf("query exceeds %d characters", maxQueryLength)
	}

	if len(request.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}

	for i, chunk := range request.ContextChunks {
		if strings.TrimSpace(chunk.ID) == "" {
			return fmt.Errorf("context_chunks[%d].id required", i)
		}

		if strings.TrimSpace(chunk.FilePath) == "" {
			return fmt.Errorf("context_chunks[%d].file_path required", i)
		}
	}

	return nil
}

// sanitizeRequest applies safety limits to the request before transmission.
// Content is truncated to maxChunkContent bytes; oversized chunks are not
// rejected outright because their metadata may still be useful to the model.
func sanitizeRequest(request *AnalyzeRequest) *AnalyzeRequest {
	sanitized := *request
	sanitized.ContextChunks = make([]ContextChunk, len(request.ContextChunks))

	for i, chunk := range request.ContextChunks {
		c := chunk

		if len(c.Content) > maxChunkContent {
			c.Content = c.Content[:maxChunkContent]
		}

		sanitized.ContextChunks[i] = c
	}

	return &sanitized
}

// validateResponse ensures the response from the AI service has the expected
// shape before it reaches business logic.
func validateResponse(response *AnalyzeResponse) error {
	if strings.TrimSpace(response.Summary) == "" {
		return fmt.Errorf("summary required")
	}

	if strings.TrimSpace(response.Status) == "" {
		return fmt.Errorf("status required")
	}

	for i, insight := range response.Insights {
		if strings.TrimSpace(insight.Type) == "" {
			return fmt.Errorf("insights[%d].type required", i)
		}

		if strings.TrimSpace(insight.Message) == "" {
			return fmt.Errorf("insights[%d].message required", i)
		}

		if insight.Confidence < 0 || insight.Confidence > 1 {
			return fmt.Errorf("insights[%d].confidence must be 0.0–1.0", i)
		}
	}

	return nil
}
