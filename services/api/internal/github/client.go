package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

type RepositoryOwner struct {
	Login string `json:"login"`
}

type Repository struct {
	ID            int64           `json:"id"`
	Name          string          `json:"name"`
	FullName      string          `json:"full_name"`
	Private       bool            `json:"private"`
	Description   string          `json:"description"`
	DefaultBranch string          `json:"default_branch"`
	Owner         RepositoryOwner `json:"owner"`
	HTMLURL       string          `json:"html_url"`
	CloneURL      string          `json:"clone_url"`
	APIURL        string          `json:"url"`
}

var (
	ErrUnauthorized    = errors.New("github_api_unauthorized")
	ErrRateLimited     = errors.New("github_rate_limited")
	ErrUnavailable     = errors.New("github_unavailable")
	ErrInvalidResponse = errors.New("github_invalid_response")
	ErrAPIError        = errors.New("github_api_error")
	//
	ErrRepositoryNotFound = errors.New("github_repository_not_found")
)

// IssueAuthor is the author (`user`) object embedded in GitHub issue payloads.
type IssueAuthor struct {
	Login string `json:"login"`
}

// Issue is the Aevor view of a GitHub issue. The body is deliberately NOT
// decoded: V1 synchronization persists metadata only, and skipping the field
// keeps oversized bodies from consuming the response-size budget.
type Issue struct {
	ID        int64       `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	State     string      `json:"state"`
	User      IssueAuthor `json:"user"`
	HTMLURL   string      `json:"html_url"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	ClosedAt  *time.Time  `json:"closed_at"`

	// PullRequest is non-nil when the entry is actually a pull request —
	// GitHub's issues endpoints include PRs in their responses. Such entries
	// are filtered out before validation.
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

const (
	defaultBaseURL   = "https://api.github.com"
	defaultUserAgent = "Aevor/0.1 (https://github.com/Aevor/platform)"
	clientTimeout    = 10 * time.Second
	maxResponseSize  = 1 << 20
)

type Client struct {
	baseURL    string
	httpClient *http.Client
	userAgent  string
}

type ClientOption func(*Client)

func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

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

func (c *Client) GetCurrentUser(ctx context.Context, accessToken string) (*User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/user", nil)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}

	var user User

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&user); err != nil {
		return nil, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidResponse
	}

	if user.ID <= 0 || strings.TrimSpace(user.Login) == "" {
		return nil, ErrInvalidResponse
	}

	return &user, nil
}

// ListUserRepositories returns one page of the repositories accessible to
// the GitHub account identified by accessToken, via GET /user/repos. page is
// 1-based; perPage is clamped by the caller. The second return value reports
// whether GitHub advertises a next page (Link header rel="next").
func (c *Client) ListUserRepositories(ctx context.Context, accessToken string, page int, perPage int) ([]Repository, bool, error) {
	endpoint := fmt.Sprintf(
		"%s/user/repos?sort=full_name&direction=asc&page=%d&per_page=%d",
		c.baseURL,
		page,
		perPage,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, classifyError(resp)
	}

	var repositories []Repository

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&repositories); err != nil {
		return nil, false, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, false, ErrInvalidResponse
	}

	for _, repository := range repositories {
		if repository.ID <= 0 || strings.TrimSpace(repository.FullName) == "" {
			return nil, false, ErrInvalidResponse
		}
	}

	return repositories, hasNextPage(resp.Header.Get("Link")), nil
}

// GetRepository fetches the authoritative GitHub repository metadata by
// numeric repository ID, using the given user's access token. A 404 means the
// repository does not exist OR is not visible to that account — both collapse
// to ErrRepositoryNotFound so callers can never persist a repository the
// authenticated user cannot access.
func (c *Client) GetRepository(ctx context.Context, accessToken string, githubRepositoryID int64) (*Repository, error) {
	endpoint := fmt.Sprintf("%s/repositories/%d", c.baseURL, githubRepositoryID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrRepositoryNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}

	var repository Repository

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&repository); err != nil {
		return nil, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidResponse
	}

	if repository.ID <= 0 || strings.TrimSpace(repository.FullName) == "" {
		return nil, ErrInvalidResponse
	}

	return &repository, nil
}

// ListRepositoryIssues returns one page of issues (metadata only) for the
// given repository, via GET /repos/{owner}/{repo}/issues with state=all and
// deterministic updated-descending order. Pull-request entries that GitHub
// includes in the response are filtered out. page is 1-based; perPage is
// clamped by the caller. The second return value reports whether GitHub
// advertises a next page (Link header rel="next"). A 404 means the repository
// does not exist, was renamed, or is not visible to this account — it maps to
// ErrRepositoryNotFound so callers can react uniformly.
func (c *Client) ListRepositoryIssues(
	ctx context.Context,
	accessToken string,
	owner string,
	repository string,
	page int,
	perPage int,
) ([]Issue, bool, error) {
	endpoint := fmt.Sprintf(
		"%s/repos/%s/%s/issues?state=all&sort=updated&direction=desc&page=%d&per_page=%d",
		c.baseURL,
		url.PathEscape(owner),
		url.PathEscape(repository),
		page,
		perPage,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, ErrRepositoryNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return nil, false, classifyError(resp)
	}

	var issues []Issue

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&issues); err != nil {
		return nil, false, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, false, ErrInvalidResponse
	}

	valid := make([]Issue, 0, len(issues))

	for _, issue := range issues {
		// Pull requests are not issues for Aevor's purposes.
		if issue.PullRequest != nil {
			continue
		}

		state := strings.TrimSpace(issue.State)

		if issue.ID <= 0 || issue.Number <= 0 ||
			strings.TrimSpace(issue.Title) == "" ||
			strings.TrimSpace(issue.User.Login) == "" ||
			strings.TrimSpace(issue.HTMLURL) == "" ||
			issue.CreatedAt.IsZero() ||
			(state != "open" && state != "closed") {
			return nil, false, ErrInvalidResponse
		}

		valid = append(valid, issue)
	}

	return valid, hasNextPage(resp.Header.Get("Link")), nil
}

// PullRequestRef is the minimal view of a PR head/base object: only the
// branch name is meaningful to Aevor's metadata model.
type PullRequestRef struct {
	Ref string `json:"ref"`
}

// PullRequest is the Aevor view of a GitHub pull request. Like Issue, the
// body is deliberately NOT decoded: V1 synchronization persists metadata
// only, and skipping the field keeps oversized bodies from consuming the
// response-size budget.
type PullRequest struct {
	ID        int64          `json:"id"`
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	State     string         `json:"state"`
	User      IssueAuthor    `json:"user"`
	HTMLURL   string         `json:"html_url"`
	Head      PullRequestRef `json:"head"`
	Base      PullRequestRef `json:"base"`
	Draft     bool           `json:"draft"`
	Merged    bool           `json:"merged"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	ClosedAt  *time.Time     `json:"closed_at"`
	MergedAt  *time.Time     `json:"merged_at"`
}

// ListRepositoryPullRequests returns one page of pull requests (metadata
// only) for the given repository, via GET /repos/{owner}/{repo}/pulls with
// state=all and deterministic updated-descending order. Unlike the issues
// endpoint, this endpoint contains no non-PR entries, so no filtering is
// required. page is 1-based; perPage is clamped by the caller. The second
// return value reports whether GitHub advertises a next page (Link header
// rel="next"). A 404 means the repository does not exist, was renamed, or is
// not visible to this account — it maps to ErrRepositoryNotFound so callers
// can react uniformly.
func (c *Client) ListRepositoryPullRequests(
	ctx context.Context,
	accessToken string,
	owner string,
	repository string,
	page int,
	perPage int,
) ([]PullRequest, bool, error) {
	endpoint := fmt.Sprintf(
		"%s/repos/%s/%s/pulls?state=all&sort=updated&direction=desc&page=%d&per_page=%d",
		c.baseURL,
		url.PathEscape(owner),
		url.PathEscape(repository),
		page,
		perPage,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)

	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, ErrRepositoryNotFound
	}

	if resp.StatusCode != http.StatusOK {
		return nil, false, classifyError(resp)
	}

	var pullRequests []PullRequest

	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize))

	if err := decoder.Decode(&pullRequests); err != nil {
		return nil, false, ErrInvalidResponse
	}

	if _, err := decoder.Token(); err != io.EOF {
		return nil, false, ErrInvalidResponse
	}

	for _, pullRequest := range pullRequests {
		state := strings.TrimSpace(pullRequest.State)

		if pullRequest.ID <= 0 || pullRequest.Number <= 0 ||
			strings.TrimSpace(pullRequest.Title) == "" ||
			strings.TrimSpace(pullRequest.User.Login) == "" ||
			strings.TrimSpace(pullRequest.HTMLURL) == "" ||
			pullRequest.CreatedAt.IsZero() ||
			strings.TrimSpace(pullRequest.Head.Ref) == "" ||
			strings.TrimSpace(pullRequest.Base.Ref) == "" ||
			(state != "open" && state != "closed") {
			return nil, false, ErrInvalidResponse
		}
	}

	return pullRequests, hasNextPage(resp.Header.Get("Link")), nil
}

// hasNextPage reports whether a GitHub Link header contains a rel="next"
// entry, e.g. `<https://api.github.com/user/repos?page=2>; rel="next"`.
func hasNextPage(link string) bool {
	const marker = `rel="next"`

	for _, segment := range strings.Split(link, ",") {
		if strings.Contains(segment, marker) {
			return true
		}
	}

	return false
}

func classifyError(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrRateLimited, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	default:
		return fmt.Errorf("%w: status %d", ErrAPIError, resp.StatusCode)
	}
}
