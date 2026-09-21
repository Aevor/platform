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
	defaultBaseURL              = "http://localhost:11434"
	defaultUserAgent            = "Aevor/0.1 (https://github.com/Aevor/platform)"
	defaultAnalyzeEndpoint      = "/v1/analyze"
	defaultAnalyzeIssueEndpoint = "/v1/analyze-issue"
	defaultProposeEndpoint      = "/v1/propose-solution"
	defaultGenerateEndpoint     = "/v1/generate-changes"
	defaultPRFeedbackEndpoint   = "/v1/analyze-pr-feedback"
	defaultImpactEndpoint       = "/v1/analyze-impact"
	clientTimeout               = 30 * time.Second
	maxResponseSize             = 4 << 20
	maxContextChunks            = 128
	maxQueryLength              = 4096
	maxChunkContent             = 8192
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
	Summary  string    `json:"summary"`
	Insights []Insight `json:"insights"`
	Status   string    `json:"status"`
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

	endpoint := c.baseURL + defaultAnalyzeEndpoint

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

// IssueInfo carries the bounded metadata of a synced issue for AI requests.
// The issue body is intentionally excluded — it is not persisted.
type IssueInfo struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	AuthorLogin string `json:"author_login"`
}

// AnalyzeIssueRequest is the controlled payload sent to the AI service for
// issue analysis. It carries repository identity, issue metadata, and bounded
// context chunks.
type AnalyzeIssueRequest struct {
	RepositoryID   string         `json:"repository_id"`
	RepositoryName string         `json:"repository_name"`
	Language       string         `json:"language,omitempty"`
	Issue          IssueInfo      `json:"issue"`
	ContextChunks  []ContextChunk `json:"context_chunks"`
}

// AnalyzeIssueResponse is the structured result from issue analysis.
type AnalyzeIssueResponse struct {
	Summary       string   `json:"summary"`
	RootCause     string   `json:"root_cause"`
	AffectedFiles []string `json:"affected_files"`
	Status        string   `json:"status"`
}

// ProposeSolutionRequest is the controlled payload sent to the AI service for
// solution proposal generation.
type ProposeSolutionRequest struct {
	RepositoryID   string               `json:"repository_id"`
	RepositoryName string               `json:"repository_name"`
	Language       string               `json:"language,omitempty"`
	Issue          IssueInfo            `json:"issue"`
	IssueAnalysis  AnalyzeIssueResponse `json:"issue_analysis"`
	ContextChunks  []ContextChunk       `json:"context_chunks"`
}

// ProposeSolutionResponse is the structured result from solution proposal.
type ProposeSolutionResponse struct {
	Summary  string   `json:"summary"`
	Approach string   `json:"approach"`
	Risks    []string `json:"risks"`
	Status   string   `json:"status"`
}

// FileChange represents one structured file operation proposed by the AI.
type FileChange struct {
	FilePath        string `json:"file_path"`
	Operation       string `json:"operation"`
	Symbol          string `json:"symbol"`
	Rationale       string `json:"rationale"`
	OriginalContext string `json:"original_context"`
	ProposedContent string `json:"proposed_content"`
}

// TestChange represents a proposed test file change.
type TestChange struct {
	FilePath        string `json:"file_path"`
	Operation       string `json:"operation"`
	ProposedContent string `json:"proposed_content"`
	Rationale       string `json:"rationale"`
}

// GenerateChangesRequest is the controlled payload sent to the AI service for
// code change generation.
type GenerateChangesRequest struct {
	RepositoryID     string                  `json:"repository_id"`
	RepositoryName   string                  `json:"repository_name"`
	Language         string                  `json:"language,omitempty"`
	Issue            IssueInfo               `json:"issue"`
	IssueAnalysis    AnalyzeIssueResponse    `json:"issue_analysis"`
	SolutionProposal ProposeSolutionResponse `json:"solution_proposal"`
	ContextChunks    []ContextChunk          `json:"context_chunks"`
}

// GenerateChangesResponse is the structured result from code change generation.
type GenerateChangesResponse struct {
	Summary     string       `json:"summary"`
	Changes     []FileChange `json:"changes"`
	Tests       []TestChange `json:"tests"`
	Assumptions []string     `json:"assumptions"`
	Uncertainty string       `json:"uncertainty"`
	Status      string       `json:"status"`
}

// AnalyzeIssue sends a bounded issue analysis request to the AI service.
func (c *Client) AnalyzeIssue(ctx context.Context, request *AnalyzeIssueRequest) (*AnalyzeIssueResponse, error) {
	if err := validateIssueRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID   string         `json:"repository_id"`
		RepositoryName string         `json:"repository_name"`
		Language       string         `json:"language,omitempty"`
		Issue          IssueInfo      `json:"issue"`
		ContextChunks  []ContextChunk `json:"context_chunks"`
	}{
		RepositoryID:   request.RepositoryID,
		RepositoryName: request.RepositoryName,
		Language:       request.Language,
		Issue:          request.Issue,
		ContextChunks:  sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}
	return postAndParse[*AnalyzeIssueResponse](ctx, c, defaultAnalyzeIssueEndpoint, body)
}

// ProposeSolution sends a bounded solution proposal request to the AI service.
func (c *Client) ProposeSolution(ctx context.Context, request *ProposeSolutionRequest) (*ProposeSolutionResponse, error) {
	if err := validateSolutionRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID   string               `json:"repository_id"`
		RepositoryName string               `json:"repository_name"`
		Language       string               `json:"language,omitempty"`
		Issue          IssueInfo            `json:"issue"`
		IssueAnalysis  AnalyzeIssueResponse `json:"issue_analysis"`
		ContextChunks  []ContextChunk       `json:"context_chunks"`
	}{
		RepositoryID:   request.RepositoryID,
		RepositoryName: request.RepositoryName,
		Language:       request.Language,
		Issue:          request.Issue,
		IssueAnalysis:  request.IssueAnalysis,
		ContextChunks:  sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}
	return postAndParse[*ProposeSolutionResponse](ctx, c, defaultProposeEndpoint, body)
}

// GenerateChanges sends a bounded code change generation request to the AI service.
func (c *Client) GenerateChanges(ctx context.Context, request *GenerateChangesRequest) (*GenerateChangesResponse, error) {
	if err := validateGenerateRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID     string                  `json:"repository_id"`
		RepositoryName   string                  `json:"repository_name"`
		Language         string                  `json:"language,omitempty"`
		Issue            IssueInfo               `json:"issue"`
		IssueAnalysis    AnalyzeIssueResponse    `json:"issue_analysis"`
		SolutionProposal ProposeSolutionResponse `json:"solution_proposal"`
		ContextChunks    []ContextChunk          `json:"context_chunks"`
	}{
		RepositoryID:     request.RepositoryID,
		RepositoryName:   request.RepositoryName,
		Language:         request.Language,
		Issue:            request.Issue,
		IssueAnalysis:    request.IssueAnalysis,
		SolutionProposal: request.SolutionProposal,
		ContextChunks:    sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}
	result, err := postAndParse[*GenerateChangesResponse](ctx, c, defaultGenerateEndpoint, body)
	if err != nil {
		return nil, err
	}
	if err := validateGenerateChangesResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

// validateGenerateChangesResponse rejects AI responses whose structured change
// contract is incomplete. Raw AI output is never trusted further than JSON
// shape; the API boundary is responsible for enforcing the contract.
func validateGenerateChangesResponse(r *GenerateChangesResponse) error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summary required")
	}
	if strings.TrimSpace(r.Status) == "" {
		return fmt.Errorf("status required")
	}
	for i, change := range r.Changes {
		if strings.TrimSpace(change.FilePath) == "" {
			return fmt.Errorf("changes[%d].file_path required", i)
		}
		if strings.TrimSpace(change.Operation) == "" {
			return fmt.Errorf("changes[%d].operation required", i)
		}
	}
	return nil
}

// ImpactTargetRef identifies the repository element an impact analysis starts
// from.
type ImpactTargetRef struct {
	FilePath string `json:"file_path"`
	Symbol   string `json:"symbol,omitempty"`
}

// ImpactRelationship is one deterministic relationship the API already
// derived from the repository. It is sent to the AI service as grounding: the
// AI may explain it or surface additional semantic relationships, but it can
// never contradict or replace it.
type ImpactRelationship struct {
	FromFile string `json:"from_file"`
	ToFile   string `json:"to_file"`
	Kind     string `json:"kind"`
	Evidence string `json:"evidence,omitempty"`
}

// AnalyzeImpactRequest is the controlled payload for semantic impact
// discovery. It carries repository identity, the target, the deterministic
// direct relationships already found, and bounded context chunks. It NEVER
// carries tokens, secrets, or credentials.
type AnalyzeImpactRequest struct {
	RepositoryID   string               `json:"repository_id"`
	RepositoryName string               `json:"repository_name"`
	Language       string               `json:"language,omitempty"`
	Target         ImpactTargetRef      `json:"target"`
	DirectImpacts  []ImpactRelationship `json:"direct_impacts"`
	ContextChunks  []ContextChunk       `json:"context_chunks"`
}

// SemanticImpact is one additional POSSIBLE impact proposed by the AI service.
// The API labels every item of this kind as inferred; it never overrides a
// repository-derived relationship.
type SemanticImpact struct {
	FilePath   string  `json:"file_path"`
	Symbol     string  `json:"symbol,omitempty"`
	Kind       string  `json:"kind"`
	Rationale  string  `json:"rationale"`
	Confidence float64 `json:"confidence"`
}

// AnalyzeImpactResponse is the structured result from semantic impact
// discovery.
type AnalyzeImpactResponse struct {
	Summary     string           `json:"summary"`
	Impacts     []SemanticImpact `json:"impacts"`
	Risks       []string         `json:"risks"`
	Uncertainty string           `json:"uncertainty"`
	Status      string           `json:"status"`
}

// maxSemanticImpacts bounds how many AI-proposed impacts are accepted so a
// misbehaving service cannot flood the response.
const maxSemanticImpacts = 128

// AnalyzeImpact sends a bounded semantic impact request to the AI service and
// returns the structured, validated response. Deterministic relationships are
// forwarded as grounding; the response is validated but its truth is decided
// by the caller (which filters to represented files and labels it inferred).
func (c *Client) AnalyzeImpact(ctx context.Context, request *AnalyzeImpactRequest) (*AnalyzeImpactResponse, error) {
	if err := validateImpactRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID   string               `json:"repository_id"`
		RepositoryName string               `json:"repository_name"`
		Language       string               `json:"language,omitempty"`
		Target         ImpactTargetRef      `json:"target"`
		DirectImpacts  []ImpactRelationship `json:"direct_impacts"`
		ContextChunks  []ContextChunk       `json:"context_chunks"`
	}{
		RepositoryID:   request.RepositoryID,
		RepositoryName: request.RepositoryName,
		Language:       request.Language,
		Target:         request.Target,
		DirectImpacts:  request.DirectImpacts,
		ContextChunks:  sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}
	result, err := postAndParse[*AnalyzeImpactResponse](ctx, c, defaultImpactEndpoint, body)
	if err != nil {
		return nil, err
	}
	if err := validateImpactResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

func validateImpactRequest(r *AnalyzeImpactRequest) error {
	if strings.TrimSpace(r.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}
	if strings.TrimSpace(r.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}
	if strings.TrimSpace(r.Target.FilePath) == "" {
		return fmt.Errorf("target.file_path required")
	}
	if len(r.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}
	return nil
}

func validateImpactResponse(r *AnalyzeImpactResponse) error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summary required")
	}
	if strings.TrimSpace(r.Status) == "" {
		return fmt.Errorf("status required")
	}
	if len(r.Impacts) > maxSemanticImpacts {
		return fmt.Errorf("impacts exceeds %d entries", maxSemanticImpacts)
	}
	for i, item := range r.Impacts {
		if strings.TrimSpace(item.FilePath) == "" {
			return fmt.Errorf("impacts[%d].file_path required", i)
		}
		if strings.TrimSpace(item.Kind) == "" {
			return fmt.Errorf("impacts[%d].kind required", i)
		}
		if item.Confidence < 0 || item.Confidence > 1 {
			return fmt.Errorf("impacts[%d].confidence must be 0.0–1.0", i)
		}
	}
	return nil
}

func postAndParse[T any](ctx context.Context, c *Client, endpoint string, body []byte) (T, error) {
	var zero T
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrUnavailable, err)
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
			return zero, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return zero, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return zero, ErrRateLimited
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return zero, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return zero, fmt.Errorf("%w: request too large", ErrRejected)
	}
	if resp.StatusCode >= 500 {
		return zero, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("%w: status %d", ErrAPIError, resp.StatusCode)
	}
	var result T
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))
	if err := decoder.Decode(&result); err != nil {
		return zero, ErrInvalidResponse
	}
	if _, err := decoder.Token(); err != io.EOF {
		return zero, ErrInvalidResponse
	}
	return result, nil
}

func sanitizeContextChunks(chunks []ContextChunk) []ContextChunk {
	out := make([]ContextChunk, len(chunks))
	for i, chunk := range chunks {
		c := chunk
		if len(c.Content) > maxChunkContent {
			c.Content = c.Content[:maxChunkContent]
		}
		out[i] = c
	}
	return out
}

func validateIssueRequest(r *AnalyzeIssueRequest) error {
	if strings.TrimSpace(r.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}
	if strings.TrimSpace(r.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}
	if strings.TrimSpace(r.Issue.Title) == "" {
		return fmt.Errorf("issue.title required")
	}
	if len(r.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}
	return nil
}

func validateSolutionRequest(r *ProposeSolutionRequest) error {
	if strings.TrimSpace(r.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}
	if strings.TrimSpace(r.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}
	if strings.TrimSpace(r.Issue.Title) == "" {
		return fmt.Errorf("issue.title required")
	}
	if strings.TrimSpace(r.IssueAnalysis.Summary) == "" {
		return fmt.Errorf("issue_analysis.summary required")
	}
	if len(r.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}
	return nil
}

func validateGenerateRequest(r *GenerateChangesRequest) error {
	if strings.TrimSpace(r.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}
	if strings.TrimSpace(r.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}
	if strings.TrimSpace(r.Issue.Title) == "" {
		return fmt.Errorf("issue.title required")
	}
	if strings.TrimSpace(r.IssueAnalysis.Summary) == "" {
		return fmt.Errorf("issue_analysis.summary required")
	}
	if strings.TrimSpace(r.SolutionProposal.Summary) == "" {
		return fmt.Errorf("solution_proposal.summary required")
	}
	if len(r.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}
	return nil
}

// PRCheckInfo carries one CI result exactly as GitHub reported it. Status
// preserves GitHub's own effective state; Aevor never infers a pass.
type PRCheckInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Source      string `json:"source"`
}

// PRReview carries one pull request review with GitHub's own verdict.
type PRReview struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	State       string `json:"state"`
	Body        string `json:"body"`
}

// PRReviewComment carries one diff-anchored inline review comment.
type PRReviewComment struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	Body        string `json:"body"`
	Path        string `json:"path"`
	Line        *int   `json:"line,omitempty"`
	DiffHunk    string `json:"diff_hunk,omitempty"`
}

// PRIssueComment carries one comment on the pull request's issue thread.
type PRIssueComment struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	Body        string `json:"body"`
}

// PRFileInfo carries one changed file's metadata only.
type PRFileInfo struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// PullRequestInfo carries bounded pull request metadata for AI analysis.
// The body may be empty (GitHub does not require one).
type PullRequestInfo struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	AuthorLogin string `json:"author_login"`
	State       string `json:"state"`
	HeadSHA     string `json:"head_sha"`
}

// AnalyzePRFeedbackRequest is the controlled payload sent to the AI service
// for pull request feedback analysis. It carries repository identity, GitHub's
// own pull request facts (checks, reviews, comments, files), and bounded
// context chunks. It NEVER carries: GitHub tokens, JWT secrets, encryption
// keys, credentials, or environment variables. All free-form text is data,
// never instructions.
type AnalyzePRFeedbackRequest struct {
	RepositoryID   string            `json:"repository_id"`
	RepositoryName string            `json:"repository_name"`
	Language       string            `json:"language,omitempty"`
	PullRequest    PullRequestInfo   `json:"pull_request"`
	Checks         []PRCheckInfo     `json:"checks"`
	Reviews        []PRReview        `json:"reviews"`
	ReviewComments []PRReviewComment `json:"review_comments"`
	IssueComments  []PRIssueComment  `json:"issue_comments"`
	Files          []PRFileInfo      `json:"files"`
	ContextChunks  []ContextChunk    `json:"context_chunks"`
}

// ActionableFeedbackItem is one grounded, actionable piece of feedback.
// Source and OriginalText reflect GitHub's own record; Recommendation and
// Reason are the AI's interpretation and are always presented as such.
type ActionableFeedbackItem struct {
	Severity       string `json:"severity"`
	Source         string `json:"source"`
	AuthorLogin    string `json:"author_login"`
	FilePath       string `json:"file_path"`
	Line           *int   `json:"line,omitempty"`
	OriginalText   string `json:"original_text"`
	Recommendation string `json:"recommendation"`
	Reason         string `json:"reason"`
}

// AnalyzePRFeedbackResponse is the structured result from pull request
// feedback analysis. GitHub facts and the interpretation are kept separate.
type AnalyzePRFeedbackResponse struct {
	Summary             string                   `json:"summary"`
	Blocking            []ActionableFeedbackItem `json:"blocking"`
	NonBlocking         []ActionableFeedbackItem `json:"non_blocking"`
	InsufficientContext bool                     `json:"insufficient_context"`
	Status              string                   `json:"status"`
}

// AnalyzePRFeedback sends a bounded pull request feedback analysis request to
// the AI service and returns the structured, validated response.
func (c *Client) AnalyzePRFeedback(ctx context.Context, request *AnalyzePRFeedbackRequest) (*AnalyzePRFeedbackResponse, error) {
	if err := validatePRFeedbackRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID   string            `json:"repository_id"`
		RepositoryName string            `json:"repository_name"`
		Language       string            `json:"language,omitempty"`
		PullRequest    PullRequestInfo   `json:"pull_request"`
		Checks         []PRCheckInfo     `json:"checks"`
		Reviews        []PRReview        `json:"reviews"`
		ReviewComments []PRReviewComment `json:"review_comments"`
		IssueComments  []PRIssueComment  `json:"issue_comments"`
		Files          []PRFileInfo      `json:"files"`
		ContextChunks  []ContextChunk    `json:"context_chunks"`
	}{
		RepositoryID:   request.RepositoryID,
		RepositoryName: request.RepositoryName,
		Language:       request.Language,
		PullRequest:    request.PullRequest,
		Checks:         request.Checks,
		Reviews:        request.Reviews,
		ReviewComments: request.ReviewComments,
		IssueComments:  request.IssueComments,
		Files:          request.Files,
		ContextChunks:  sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}
	result, err := postAndParse[*AnalyzePRFeedbackResponse](ctx, c, defaultPRFeedbackEndpoint, body)
	if err != nil {
		return nil, err
	}
	if err := validatePRFeedbackResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

func validatePRFeedbackRequest(r *AnalyzePRFeedbackRequest) error {
	if strings.TrimSpace(r.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}
	if strings.TrimSpace(r.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}
	if r.PullRequest.Number <= 0 {
		return fmt.Errorf("pull_request.number must be positive")
	}
	if strings.TrimSpace(r.PullRequest.Title) == "" {
		return fmt.Errorf("pull_request.title required")
	}
	if strings.TrimSpace(r.PullRequest.HeadSHA) == "" {
		return fmt.Errorf("pull_request.head_sha required")
	}
	if len(r.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}
	for i, check := range r.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("checks[%d].name required", i)
		}
		if strings.TrimSpace(check.Status) == "" {
			return fmt.Errorf("checks[%d].status required", i)
		}
	}
	for i, review := range r.Reviews {
		if strings.TrimSpace(review.AuthorLogin) == "" {
			return fmt.Errorf("reviews[%d].author_login required", i)
		}
		if strings.TrimSpace(review.State) == "" {
			return fmt.Errorf("reviews[%d].state required", i)
		}
	}
	for i, comment := range r.ReviewComments {
		if strings.TrimSpace(comment.AuthorLogin) == "" {
			return fmt.Errorf("review_comments[%d].author_login required", i)
		}
		if strings.TrimSpace(comment.Body) == "" {
			return fmt.Errorf("review_comments[%d].body required", i)
		}
	}
	for i, comment := range r.IssueComments {
		if strings.TrimSpace(comment.AuthorLogin) == "" {
			return fmt.Errorf("issue_comments[%d].author_login required", i)
		}
		if strings.TrimSpace(comment.Body) == "" {
			return fmt.Errorf("issue_comments[%d].body required", i)
		}
	}
	for i, file := range r.Files {
		if strings.TrimSpace(file.Filename) == "" {
			return fmt.Errorf("files[%d].filename required", i)
		}
	}
	return nil
}

func validatePRFeedbackResponse(r *AnalyzePRFeedbackResponse) error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summary required")
	}
	if strings.TrimSpace(r.Status) == "" {
		return fmt.Errorf("status required")
	}
	validSeverities := map[string]bool{"critical": true, "warning": true, "info": true}
	validSources := map[string]bool{"review": true, "review_comment": true, "issue_comment": true, "check": true, "general": true}
	items := append(append([]ActionableFeedbackItem{}, r.Blocking...), r.NonBlocking...)
	for i, item := range items {
		if !validSeverities[item.Severity] {
			return fmt.Errorf("items[%d].severity invalid", i)
		}
		if !validSources[item.Source] {
			return fmt.Errorf("items[%d].source invalid", i)
		}
		if strings.TrimSpace(item.Recommendation) == "" {
			return fmt.Errorf("items[%d].recommendation required", i)
		}
		if strings.TrimSpace(item.Reason) == "" {
			return fmt.Errorf("items[%d].reason required", i)
		}
	}
	return nil
}
