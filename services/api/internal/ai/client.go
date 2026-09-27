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
	defaultAnalyzeRepository    = "/v1/analyze-repository"
	clientTimeout               = 30 * time.Second
	maxResponseSize             = 4 << 20
	maxContextChunks            = 128
	maxQueryLength              = 4096
	maxChunkContent             = 8192
	maxRepositoryContextText    = 4096
	maxRepositoryContextStrings = 64
	maxRepositoryEntries        = 32
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

// untrustedDataMarker fences prompt-injection-sensitive text (the user query
// and repository source content) so the model can always tell Aevor's
// instructions apart from data it was handed.
const untrustedDataMarker = "[[UNTRUSTED_DATA]]"

// sanitizeRequest applies safety limits to the request before transmission.
// Content is truncated to maxChunkContent bytes; oversized chunks are not
// rejected outright because their metadata may still be useful to the model.
// Untrusted data (query, chunk content) is wrapped with prompt injection
// markers to reduce the risk of instruction override.
func sanitizeRequest(request *AnalyzeRequest) *AnalyzeRequest {
	sanitized := *request
	sanitized.ContextChunks = make([]ContextChunk, len(request.ContextChunks))

	// Wrap the query with prompt injection markers
	if strings.TrimSpace(sanitized.Query) != "" {
		sanitized.Query = untrustedDataMarker + " " + strings.ToLower(strings.TrimSpace(sanitized.Query)) + " " + untrustedDataMarker
	}

	for i, chunk := range request.ContextChunks {
		c := chunk

		if len(c.Content) > maxChunkContent {
			c.Content = c.Content[:maxChunkContent]
		}

		// Chunk content is repository-controlled text, so it is fenced with the
		// same prompt-injection markers as the query before it can reach the
		// model. The content itself is forwarded VERBATIM: source code is
		// case-sensitive, and rewriting it would corrupt the very evidence the
		// model is asked to reason about.
		if strings.TrimSpace(c.Content) != "" {
			c.Content = untrustedDataMarker + " " + c.Content + " " + untrustedDataMarker
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

// validateRepositoryResponse validates the AnalyzeRepositoryResponse.
func validateRepositoryResponse(response *AnalyzeRepositoryResponse) error {
	if strings.TrimSpace(response.Overview) == "" {
		return fmt.Errorf("overview required")
	}

	if strings.TrimSpace(response.Status) == "" {
		return fmt.Errorf("status required")
	}

	if len(response.Components) > maxRepositoryEntries {
		return fmt.Errorf("components exceeds %d entries", maxRepositoryEntries)
	}

	for i, component := range response.Components {
		if strings.TrimSpace(component.Name) == "" {
			return fmt.Errorf("components[%d].name required", i)
		}
		if strings.TrimSpace(component.Kind) == "" {
			return fmt.Errorf("components[%d].kind required", i)
		}
	}

	if len(response.Relationships) > maxRepositoryEntries {
		return fmt.Errorf("relationships exceeds %d entries", maxRepositoryEntries)
	}

	for i, rel := range response.Relationships {
		if strings.TrimSpace(rel.From) == "" {
			return fmt.Errorf("relationships[%d].from required", i)
		}
		if strings.TrimSpace(rel.To) == "" {
			return fmt.Errorf("relationships[%d].to required", i)
		}
		if strings.TrimSpace(rel.Kind) == "" {
			return fmt.Errorf("relationships[%d].kind required", i)
		}
	}

	if len(response.Conventions) > maxRepositoryEntries {
		return fmt.Errorf("conventions exceeds %d entries", maxRepositoryEntries)
	}

	for i, conv := range response.Conventions {
		if strings.TrimSpace(conv.Name) == "" {
			return fmt.Errorf("conventions[%d].name required", i)
		}
	}

	return nil
}

// validateValidationResponse validates the AnalyzeValidationFailureResponse.
func validateValidationResponse(response *AnalyzeValidationFailureResponse) error {
	if strings.TrimSpace(response.Summary) == "" {
		return fmt.Errorf("summary required")
	}

	if strings.TrimSpace(response.Status) == "" {
		return fmt.Errorf("status required")
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

	// RepositoryContext carries the caller's persisted, revision-bound
	// deterministic repository profile. It is omitted entirely when no fresh
	// profile exists, so the AI service is never told about structure Aevor
	// cannot currently attest.
	RepositoryContext *RepositoryContext `json:"repository_context,omitempty"`
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

// RepositoryStructure is the compact deterministic structure sent to the AI
// service for repository-level analysis. It contains only bounded,
// repository-derived facts.
type RepositoryStructure struct {
	Languages   []string `json:"languages"`
	Files       int      `json:"files"`
	Chunks      int      `json:"chunks"`
	EntryPoints []string `json:"entry_points"`
	AppScopes   []string `json:"app_scopes"`
	ConfigFiles []string `json:"config_files"`
	TestFiles   []string `json:"test_files"`
	BuildFiles  []string `json:"build_files"`
}

// AnalyzeRepositoryRequest is the controlled payload sent to the AI service for
// repository architecture analysis. It carries repository identity and the
// deterministic structure. It NEVER carries: GitHub tokens, JWT secrets,
// encryption keys, credentials, or environment variables.
type AnalyzeRepositoryRequest struct {
	RepositoryID   string              `json:"repository_id"`
	RepositoryName string              `json:"repository_name"`
	Language       string              `json:"language,omitempty"`
	Structure      RepositoryStructure `json:"structure"`
}

// AnalyzeRepositoryComponent is one architecture component returned by the AI.
type AnalyzeRepositoryComponent struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Path             string   `json:"path"`
	Description      string   `json:"description"`
	Responsibilities []string `json:"responsibilities"`
}

// AnalyzeRepositoryRelationship is one architecture relationship returned by the AI.
type AnalyzeRepositoryRelationship struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// AnalyzeRepositoryConvention is one convention returned by the AI.
type AnalyzeRepositoryConvention struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Evidence    []string `json:"evidence"`
}

// AnalyzeRepositoryResponse is the structured result returned by the AI service
// for repository analysis. The API parses and validates this; raw AI output is
// never exposed directly.
type AnalyzeRepositoryResponse struct {
	Overview             string                          `json:"overview"`
	Components           []AnalyzeRepositoryComponent    `json:"components"`
	Relationships        []AnalyzeRepositoryRelationship `json:"relationships"`
	Conventions          []AnalyzeRepositoryConvention   `json:"conventions"`
	EngineeringDecisions []string                        `json:"engineering_decisions"`
	Databases            []string                        `json:"databases"`
	ExternalIntegrations []string                        `json:"external_integrations"`
	Uncertainty          []string                        `json:"uncertainty"`
	Status               string                          `json:"status"`
}

// RepositoryContext is the compact repository intelligence context sent to the
// AI service for issue-level analysis. It carries only bounded,
// repository-derived facts — never raw source code or secrets.
type RepositoryContext struct {
	DeterministicSummary string   `json:"deterministic_summary"`
	Languages            []string `json:"languages"`
	EntryPoints          []string `json:"entry_points"`
	Components           []string `json:"components"`
	Conventions          []string `json:"conventions"`
	ArchitectureOverview string   `json:"architecture_overview"`
	Relationships        []string `json:"relationships"`
	Uncertainty          []string `json:"uncertainty"`
}

// AnalyzeRepository sends a bounded repository analysis request to the AI
// service and returns the structured architecture response.
func (c *Client) AnalyzeRepository(ctx context.Context, request *AnalyzeRepositoryRequest) (*AnalyzeRepositoryResponse, error) {
	if err := validateRepositoryRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}

	result, err := postAndParse[*AnalyzeRepositoryResponse](ctx, c, defaultAnalyzeRepository, body)
	if err != nil {
		return nil, err
	}
	if err := validateRepositoryResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

// validateRepositoryRequest rejects obviously invalid payloads before they leave
// the API boundary. This catches programming errors, not adversarial input — the
// API is the trust boundary, not the AI service.
func validateRepositoryRequest(request *AnalyzeRepositoryRequest) error {
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}

	if strings.TrimSpace(request.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}

	if request.Structure.Files < 0 || request.Structure.Chunks < 0 {
		return fmt.Errorf("files and chunks must be non-negative")
	}

	if len(request.Structure.Languages) > maxRepositoryContextStrings {
		return fmt.Errorf("languages exceeds %d entries", maxRepositoryContextStrings)
	}
	if len(request.Structure.EntryPoints) > maxRepositoryContextStrings {
		return fmt.Errorf("entry_points exceeds %d entries", maxRepositoryContextStrings)
	}
	if len(request.Structure.AppScopes) > maxRepositoryContextStrings {
		return fmt.Errorf("app_scopes exceeds %d entries", maxRepositoryContextStrings)
	}
	if len(request.Structure.ConfigFiles) > maxRepositoryContextStrings {
		return fmt.Errorf("config_files exceeds %d entries", maxRepositoryContextStrings)
	}
	if len(request.Structure.TestFiles) > maxRepositoryContextStrings {
		return fmt.Errorf("test_files exceeds %d entries", maxRepositoryContextStrings)
	}
	if len(request.Structure.BuildFiles) > maxRepositoryContextStrings {
		return fmt.Errorf("build_files exceeds %d entries", maxRepositoryContextStrings)
	}

	return nil
}

// sanitizeRepositoryContext applies safety limits to the repository context
// before transmission. String fields are truncated; oversized arrays are capped.
func sanitizeRepositoryContext(ctx *RepositoryContext) *RepositoryContext {
	if ctx == nil {
		return nil
	}

	sanitized := *ctx

	if len(sanitized.DeterministicSummary) > maxRepositoryContextText {
		sanitized.DeterministicSummary = sanitized.DeterministicSummary[:maxRepositoryContextText]
	}

	sanitized.Languages = sanitizeStringSlice(sanitized.Languages, maxRepositoryContextStrings)
	sanitized.EntryPoints = sanitizeStringSlice(sanitized.EntryPoints, maxRepositoryContextStrings)
	sanitized.Components = sanitizeStringSlice(sanitized.Components, maxRepositoryContextStrings)
	sanitized.Conventions = sanitizeStringSlice(sanitized.Conventions, maxRepositoryContextStrings)
	sanitized.Relationships = sanitizeStringSlice(sanitized.Relationships, maxRepositoryContextStrings)
	sanitized.Uncertainty = sanitizeStringSlice(sanitized.Uncertainty, maxRepositoryContextStrings)

	if len(sanitized.ArchitectureOverview) > maxRepositoryContextText {
		sanitized.ArchitectureOverview = sanitized.ArchitectureOverview[:maxRepositoryContextText]
	}

	return &sanitized
}

// sanitizeStringSlice trims each string and caps the slice length.
func sanitizeStringSlice(slice []string, max int) []string {
	if len(slice) == 0 {
		return nil
	}

	out := make([]string, 0, len(slice))
	for _, s := range slice {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}

	if len(out) > max {
		out = out[:max]
	}

	return out
}

// AnalyzeIssue sends a bounded issue analysis request to the AI service.
func (c *Client) AnalyzeIssue(ctx context.Context, request *AnalyzeIssueRequest) (*AnalyzeIssueResponse, error) {
	if err := validateIssueRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	sanitized := sanitizeContextChunks(request.ContextChunks)
	body, err := json.Marshal(struct {
		RepositoryID      string             `json:"repository_id"`
		RepositoryName    string             `json:"repository_name"`
		Language          string             `json:"language,omitempty"`
		Issue             IssueInfo          `json:"issue"`
		ContextChunks     []ContextChunk     `json:"context_chunks"`
		RepositoryContext *RepositoryContext `json:"repository_context,omitempty"`
	}{
		RepositoryID:      request.RepositoryID,
		RepositoryName:    request.RepositoryName,
		Language:          request.Language,
		Issue:             request.Issue,
		ContextChunks:     sanitized,
		RepositoryContext: sanitizeRepositoryContext(request.RepositoryContext),
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

const (
	defaultAnalyzeValidation = "/v1/analyze-validation"
	maxValidationChecks      = 32
)

// ValidationFailureCheck is one controlled validation check that failed.
type ValidationFailureCheck struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Stage        string `json:"stage,omitempty"`
	FailureClass string `json:"failure_class,omitempty"`
	Message      string `json:"message"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	ExitCode     int    `json:"exit_code,omitempty"`
}

// AnalyzeValidationFailureRequest is the controlled payload sent to the AI
// service for validation failure analysis.
type AnalyzeValidationFailureRequest struct {
	RepositoryID   string                   `json:"repository_id"`
	RepositoryName string                   `json:"repository_name"`
	Language       string                   `json:"language,omitempty"`
	Toolchains     []string                 `json:"toolchains"`
	FailureClass   string                   `json:"failure_class"`
	Summary        string                   `json:"summary"`
	Checks         []ValidationFailureCheck `json:"checks"`
}

// AnalyzeValidationFailureResponse is the structured result from validation
// failure analysis.
type AnalyzeValidationFailureResponse struct {
	Summary     string   `json:"summary"`
	RootCause   string   `json:"root_cause"`
	Suggestions []string `json:"suggestions"`
	Status      string   `json:"status"`
}

// AnalyzeValidationFailure sends a bounded validation failure analysis request
// to the AI service.
func (c *Client) AnalyzeValidationFailure(ctx context.Context, request *AnalyzeValidationFailureRequest) (*AnalyzeValidationFailureResponse, error) {
	if err := validateValidationRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}

	result, err := postAndParse[*AnalyzeValidationFailureResponse](ctx, c, defaultAnalyzeValidation, body)
	if err != nil {
		return nil, err
	}
	if err := validateValidationResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

func validateValidationRequest(request *AnalyzeValidationFailureRequest) error {
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}

	if strings.TrimSpace(request.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}

	if len(request.Toolchains) > maxRepositoryContextStrings {
		return fmt.Errorf("toolchains exceeds %d entries", maxRepositoryContextStrings)
	}

	if len(request.Checks) > maxValidationChecks {
		return fmt.Errorf("checks exceeds %d entries", maxValidationChecks)
	}

	for i, check := range request.Checks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("checks[%d].name required", i)
		}
		if strings.TrimSpace(check.Status) == "" {
			return fmt.Errorf("checks[%d].status required", i)
		}
	}

	return nil
}

const (
	defaultAnalyzePRFeedback = "/v1/analyze-pr-feedback"
	maxPRFeedbackChecks      = 64
	maxPRFeedbackFiles       = 128
	maxPRFeedbackComments    = 256
)

// PRCheckInfo is one check from a pull request.
type PRCheckInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Source      string `json:"source,omitempty"`
}

// PRReview is one review on a pull request.
type PRReview struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
}

// PRReviewComment is one review comment on a pull request.
type PRReviewComment struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	Body        string `json:"body"`
	Path        string `json:"path"`

	// Line is GitHub's own 1-based anchor and stays null when GitHub anchors
	// the comment to a file or to an outdated position. It is never faked to a
	// real line number.
	Line *int `json:"line"`

	DiffHunk string `json:"diff_hunk,omitempty"`
}

// PRIssueComment is one issue-thread comment on a pull request.
type PRIssueComment struct {
	ID          int    `json:"id"`
	AuthorLogin string `json:"author_login"`
	Body        string `json:"body"`
}

// PRFileInfo is one changed file in a pull request.
type PRFileInfo struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// PullRequestInfo is the bounded metadata of a pull request for AI requests.
type PullRequestInfo struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	AuthorLogin string `json:"author_login"`
	State       string `json:"state"`
	HeadSHA     string `json:"head_sha"`
}

// AnalyzePRFeedbackRequest is the controlled payload sent to the AI service
// for pull request feedback analysis.
type AnalyzePRFeedbackRequest struct {
	RepositoryID      string             `json:"repository_id"`
	RepositoryName    string             `json:"repository_name"`
	Language          string             `json:"language,omitempty"`
	PullRequest       PullRequestInfo    `json:"pull_request"`
	Checks            []PRCheckInfo      `json:"checks"`
	Reviews           []PRReview         `json:"reviews"`
	ReviewComments    []PRReviewComment  `json:"review_comments"`
	IssueComments     []PRIssueComment   `json:"issue_comments"`
	Files             []PRFileInfo       `json:"files"`
	ContextChunks     []ContextChunk     `json:"context_chunks"`
	RepositoryContext *RepositoryContext `json:"repository_context,omitempty"`
}

// AnalyzePRFeedbackResponse is the structured result from PR feedback analysis.
type AnalyzePRFeedbackResponse struct {
	Summary             string                   `json:"summary"`
	Blocking            []ActionableFeedbackItem `json:"blocking"`
	NonBlocking         []ActionableFeedbackItem `json:"non_blocking"`
	InsufficientContext bool                     `json:"insufficient_context"`
	Status              string                   `json:"status"`
}

// ActionableFeedbackItem is one actionable feedback item from PR analysis.
type ActionableFeedbackItem struct {
	Severity       string `json:"severity"`
	Source         string `json:"source"`
	AuthorLogin    string `json:"author_login,omitempty"`
	FilePath       string `json:"file_path,omitempty"`
	Line           int    `json:"line,omitempty"`
	OriginalText   string `json:"original_text,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// AnalyzePRFeedback sends a bounded PR feedback analysis request to the AI service.
func (c *Client) AnalyzePRFeedback(ctx context.Context, request *AnalyzePRFeedbackRequest) (*AnalyzePRFeedbackResponse, error) {
	if err := validatePRFeedbackRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}

	result, err := postAndParse[*AnalyzePRFeedbackResponse](ctx, c, defaultAnalyzePRFeedback, body)
	if err != nil {
		return nil, err
	}
	if err := validatePRFeedbackResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

func validatePRFeedbackRequest(request *AnalyzePRFeedbackRequest) error {
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}

	if strings.TrimSpace(request.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}

	if strings.TrimSpace(request.PullRequest.Title) == "" {
		return fmt.Errorf("pull_request.title required")
	}

	if len(request.Checks) > maxPRFeedbackChecks {
		return fmt.Errorf("checks exceeds %d entries", maxPRFeedbackChecks)
	}
	if len(request.Reviews) > maxPRFeedbackChecks {
		return fmt.Errorf("reviews exceeds %d entries", maxPRFeedbackChecks)
	}
	if len(request.ReviewComments) > maxPRFeedbackComments {
		return fmt.Errorf("review_comments exceeds %d entries", maxPRFeedbackComments)
	}
	if len(request.IssueComments) > maxPRFeedbackComments {
		return fmt.Errorf("issue_comments exceeds %d entries", maxPRFeedbackComments)
	}
	if len(request.Files) > maxPRFeedbackFiles {
		return fmt.Errorf("files exceeds %d entries", maxPRFeedbackFiles)
	}
	if len(request.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}

	return nil
}

func validatePRFeedbackResponse(response *AnalyzePRFeedbackResponse) error {
	if strings.TrimSpace(response.Summary) == "" {
		return fmt.Errorf("summary required")
	}

	if strings.TrimSpace(response.Status) == "" {
		return fmt.Errorf("status required")
	}

	return nil
}

const (
	defaultAnalyzeImpact   = "/v1/analyze-impact"
	maxImpactTargets       = 64
	maxImpactRelationships = 256
)

// ImpactTargetRef is a reference to a target for impact analysis.
type ImpactTargetRef struct {
	FilePath string `json:"file_path"`
	Symbol   string `json:"symbol,omitempty"`
}

// ImpactRelationship represents a direct relationship between files/symbols.
type ImpactRelationship struct {
	FromFile string `json:"from_file"`
	ToFile   string `json:"to_file"`
	Kind     string `json:"kind"`
	Evidence string `json:"evidence,omitempty"`
}

// AnalyzeImpactRequest is the controlled payload sent to the AI service
// for impact analysis.
type AnalyzeImpactRequest struct {
	RepositoryID      string               `json:"repository_id"`
	RepositoryName    string               `json:"repository_name"`
	Language          string               `json:"language,omitempty"`
	Target            ImpactTargetRef      `json:"target"`
	DirectImpacts     []ImpactRelationship `json:"direct_impacts"`
	ContextChunks     []ContextChunk       `json:"context_chunks"`
	RepositoryContext *RepositoryContext   `json:"repository_context,omitempty"`
}

// AnalyzeImpactResponse is the structured result from impact analysis.
type AnalyzeImpactResponse struct {
	Summary     string           `json:"summary"`
	Impacts     []ImpactAIResult `json:"impacts"`
	Risks       []string         `json:"risks"`
	Uncertainty string           `json:"uncertainty"`
	Status      string           `json:"status"`
}

// ImpactAIResult is one AI-proposed impact.
type ImpactAIResult struct {
	FilePath   string  `json:"file_path"`
	Symbol     string  `json:"symbol,omitempty"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale,omitempty"`
}

// AnalyzeImpact sends a bounded impact analysis request to the AI service.
func (c *Client) AnalyzeImpact(ctx context.Context, request *AnalyzeImpactRequest) (*AnalyzeImpactResponse, error) {
	if err := validateImpactRequest(request); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRejected, err)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAPIError, err)
	}

	result, err := postAndParse[*AnalyzeImpactResponse](ctx, c, defaultAnalyzeImpact, body)
	if err != nil {
		return nil, err
	}
	if err := validateImpactResponse(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return result, nil
}

func validateImpactRequest(request *AnalyzeImpactRequest) error {
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repository_id required")
	}

	if strings.TrimSpace(request.RepositoryName) == "" {
		return fmt.Errorf("repository_name required")
	}

	if strings.TrimSpace(request.Target.FilePath) == "" {
		return fmt.Errorf("target.file_path required")
	}

	if len(request.DirectImpacts) > maxImpactRelationships {
		return fmt.Errorf("direct_impacts exceeds %d entries", maxImpactRelationships)
	}
	if len(request.ContextChunks) > maxContextChunks {
		return fmt.Errorf("context_chunks exceeds %d entries", maxContextChunks)
	}

	return nil
}

func validateImpactResponse(response *AnalyzeImpactResponse) error {
	if strings.TrimSpace(response.Summary) == "" {
		return fmt.Errorf("summary required")
	}

	if strings.TrimSpace(response.Status) == "" {
		return fmt.Errorf("status required")
	}

	for i, impact := range response.Impacts {
		if strings.TrimSpace(impact.FilePath) == "" {
			return fmt.Errorf("impacts[%d].file_path required", i)
		}
		if impact.Confidence < 0 || impact.Confidence > 1 {
			return fmt.Errorf("impacts[%d].confidence must be 0.0–1.0", i)
		}
	}

	return nil
}
