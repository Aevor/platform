package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
)

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
