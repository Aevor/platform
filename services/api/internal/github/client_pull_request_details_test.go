package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pullRequestDetailJSON = `{
	"id": 7001,
	"number": 10,
	"title": "Add feedback analysis",
	"body": "Implements the PR feedback pipeline.",
	"state": "open",
	"user": {"login": "octocat"},
	"html_url": "https://github.com/octocat/hello-world/pull/10",
	"head": {"ref": "feature-branch", "sha": "abcdef0123456789abcdef0123456789abcdef01"},
	"base": {"ref": "main", "sha": "0000000000000000000000000000000000000000"},
	"draft": false,
	"merged": false,
	"created_at": "2026-01-02T03:04:05Z",
	"updated_at": "2026-02-03T04:05:06Z",
	"closed_at": null,
	"merged_at": null
}`

func TestGetPullRequest_Success(t *testing.T) {
	var gotPath, gotAuth, gotVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		w.Write([]byte(pullRequestDetailJSON))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	detail, err := client.GetPullRequest(
		context.Background(), issuesTestAccessToken, "octocat", "hello-world", 10)

	if err != nil {
		t.Fatalf("GetPullRequest() error: %v", err)
	}

	if gotPath != "/repos/octocat/hello-world/pulls/10" {
		t.Errorf("path = %q, want /repos/octocat/hello-world/pulls/10", gotPath)
	}

	if gotAuth != "Bearer "+issuesTestAccessToken {
		t.Errorf("Authorization = %q, want the caller's token as Bearer", gotAuth)
	}

	if gotVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", gotVersion)
	}

	if detail.ID != 7001 || detail.Number != 10 || detail.Title != "Add feedback analysis" ||
		detail.Body != "Implements the PR feedback pipeline." || detail.State != "open" ||
		detail.User.Login != "octocat" ||
		detail.Head.Ref != "feature-branch" ||
		detail.Head.SHA != "abcdef0123456789abcdef0123456789abcdef01" ||
		detail.Base.Ref != "main" ||
		detail.Base.SHA != "0000000000000000000000000000000000000000" ||
		detail.Draft || detail.Merged || detail.ClosedAt != nil || detail.MergedAt != nil {
		t.Errorf("decoded detail = %+v, want full PR view with body and head SHA", *detail)
	}
}

func TestGetPullRequest_EscapesSlashInPaths(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.Write([]byte(pullRequestDetailJSON))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	if _, err := client.GetPullRequest(
		context.Background(), issuesTestAccessToken, "o", "w/orld", 7); err != nil {
		t.Fatalf("GetPullRequest() error: %v", err)
	}

	if !strings.Contains(gotPath, "/repos/o/w%2Forld/pulls/7") {
		t.Errorf("path = %q, want the slash in the repo name escaped", gotPath)
	}
}

func TestGetPullRequest_NotFoundMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetPullRequest(
		context.Background(), issuesTestAccessToken, "o", "r", 404)

	if !errors.Is(err, ErrPullRequestNotFound) {
		t.Fatalf("error = %v, want github_pull_request_not_found", err)
	}
}

func TestGetPullRequest_StatusMapped(t *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		wantCode error
	}{
		{"401 unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrUnavailable},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.status)
				w.Write([]byte(`{"message":"boom"}`))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.GetPullRequest(
				context.Background(), issuesTestAccessToken, "o", "r", 1)

			if !errors.Is(err, sc.wantCode) {
				t.Errorf("error = %v, want errors.Is(%v)", err, sc.wantCode)
			}
		})
	}
}

func TestGetPullRequest_RejectsMalformedResponses(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"malformed json", `{not-json`},
		{"missing head sha", `{
			"id": 1, "number": 1, "title": "t", "state": "open",
			"user": {"login": "octocat"},
			"html_url": "https://github.com/o/r/pull/1",
			"head": {"ref": "feature-branch"},
			"base": {"ref": "main", "sha": "abc"},
			"created_at": "2026-01-02T03:04:05Z"}`},
		{"unknown state", `{
			"id": 1, "number": 1, "title": "t", "state": "merged-ish",
			"user": {"login": "octocat"},
			"html_url": "https://github.com/o/r/pull/1",
			"head": {"ref": "feature-branch", "sha": "abc"},
			"base": {"ref": "main", "sha": "abc"},
			"created_at": "2026-01-02T03:04:05Z"}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.GetPullRequest(
				context.Background(), issuesTestAccessToken, "o", "r", 1)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}

func reviewJSON(id int64, login string, state string, body string) string {
	return `{
		"id": ` + fmt.Sprint(id) + `,
		"user": {"login": "` + login + `"},
		"state": "` + state + `",
		"body": "` + body + `",
		"commit_id": "abcdef0123456789abcdef0123456789abcdef01",
		"html_url": "https://github.com/o/r/pull/1#pullrequestreview-` + fmt.Sprint(id) + `",
		"submitted_at": "2026-03-04T05:06:07Z"
	}`
}

func TestListPullRequestReviews_Success(t *testing.T) {
	var gotPath, gotQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[` +
			reviewJSON(501, "octocat", "approved", "LGTM") + `,` +
			reviewJSON(502, "other", "changes_requested", "see line 12") + `,` +
			reviewJSON(503, "mild", "comment", "") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	reviews, err := client.ListPullRequestReviews(
		context.Background(), issuesTestAccessToken, "o", "r", 1)

	if err != nil {
		t.Fatalf("ListPullRequestReviews() error: %v", err)
	}

	if gotPath != "/repos/o/r/pulls/1/reviews" {
		t.Errorf("path = %q, want /repos/o/r/pulls/1/reviews", gotPath)
	}

	if !strings.Contains(gotQuery, "per_page=100") {
		t.Errorf("query = %q, want per_page=100", gotQuery)
	}

	if len(reviews) != 3 {
		t.Fatalf("reviews = %d, want 3", len(reviews))
	}

	if reviews[0].ID != 501 || reviews[0].User.Login != "octocat" ||
		reviews[0].State != "approved" || reviews[0].Body != "LGTM" ||
		reviews[0].CommitID == "" || reviews[0].SubmittedAt.IsZero() {
		t.Errorf("decoded review = %+v, want the GitHub review words preserved", reviews[0])
	}

	if reviews[2].State != "comment" || reviews[2].Body != "" {
		t.Errorf("comment review = %+v, want empty body accepted", reviews[2])
	}
}

func TestListPullRequestReviews_RejectsUnknownState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[` + reviewJSON(501, "octocat", "not_a_real_state", "") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.ListPullRequestReviews(
		context.Background(), issuesTestAccessToken, "o", "r", 1)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want github_invalid_response for an unknown review state", err)
	}
}

func reviewCommentJSON(id int64, login string, path string, line int, body string) string {
	return `{
		"id": ` + fmt.Sprint(id) + `,
		"user": {"login": "` + login + `"},
		"body": "` + body + `",
		"path": "` + path + `",
		"line": ` + fmt.Sprint(line) + `,
		"diff_hunk": "@@ -1,3 +1,4 @@",
		"commit_id": "abcdef0123456789abcdef0123456789abcdef01",
		"html_url": "https://github.com/o/r/pull/1#discussion_r` + fmt.Sprint(id) + `",
		"created_at": "2026-03-04T05:06:07Z",
		"updated_at": "2026-03-04T05:06:07Z"
	}`
}

func TestListPullRequestReviewComments_Success(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`[` +
			reviewCommentJSON(601, "octocat", "internal/analysis.go", 12, "guard this") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	comments, err := client.ListPullRequestReviewComments(
		context.Background(), issuesTestAccessToken, "o", "r", 1)

	if err != nil {
		t.Fatalf("ListPullRequestReviewComments() error: %v", err)
	}

	if gotPath != "/repos/o/r/pulls/1/comments" {
		t.Errorf("path = %q, want /repos/o/r/pulls/1/comments", gotPath)
	}

	if len(comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(comments))
	}

	c := comments[0]

	if c.ID != 601 || c.User.Login != "octocat" || c.Body != "guard this" ||
		c.Path != "internal/analysis.go" || c.Line == nil || *c.Line != 12 ||
		c.DiffHunk == "" || c.CreatedAt.IsZero() {
		t.Errorf("decoded comment = %+v, want diff-anchored context preserved", c)
	}
}

func issueCommentJSON(id int64, login string, body string) string {
	return `{
		"id": ` + fmt.Sprint(id) + `,
		"user": {"login": "` + login + `"},
		"body": "` + body + `",
		"html_url": "https://github.com/o/r/pull/1#issuecomment-` + fmt.Sprint(id) + `",
		"created_at": "2026-03-04T05:06:07Z",
		"updated_at": "2026-03-04T05:06:07Z"
	}`
}

func TestListPullRequestIssueComments_Success(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`[` + issueCommentJSON(701, "octocat", "what about perf?") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	comments, err := client.ListPullRequestIssueComments(
		context.Background(), issuesTestAccessToken, "o", "r", 1)

	if err != nil {
		t.Fatalf("ListPullRequestIssueComments() error: %v", err)
	}

	if gotPath != "/repos/o/r/issues/1/comments" {
		t.Errorf("path = %q, want /repos/o/r/issues/1/comments", gotPath)
	}

	if len(comments) != 1 || comments[0].ID != 701 || comments[0].Body != "what about perf?" {
		t.Errorf("decoded comments = %+v, want the issue thread comment", comments)
	}
}

func TestListPullRequestFiles_Success(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`[{
			"filename": "internal/analysis.go",
			"status": "modified",
			"additions": 4,
			"deletions": 2,
			"changes": 6
		}]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	files, err := client.ListPullRequestFiles(
		context.Background(), issuesTestAccessToken, "o", "r", 1)

	if err != nil {
		t.Fatalf("ListPullRequestFiles() error: %v", err)
	}

	if gotPath != "/repos/o/r/pulls/1/files" {
		t.Errorf("path = %q, want /repos/o/r/pulls/1/files", gotPath)
	}

	if len(files) != 1 || files[0].Filename != "internal/analysis.go" ||
		files[0].Status != "modified" || files[0].Additions != 4 ||
		files[0].Deletions != 2 || files[0].Changes != 6 {
		t.Errorf("decoded files = %+v, want metadata only", files)
	}
}

func TestPullRequestDetailLists_404Mapped(t *testing.T) {
	endpoints := []struct {
		name string
		call func(*Client) error
	}{
		{
			"reviews",
			func(c *Client) error {
				_, err := c.ListPullRequestReviews(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"review comments",
			func(c *Client) error {
				_, err := c.ListPullRequestReviewComments(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"issue comments",
			func(c *Client) error {
				_, err := c.ListPullRequestIssueComments(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"files",
			func(c *Client) error {
				_, err := c.ListPullRequestFiles(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	for _, e := range endpoints {
		t.Run(e.name, func(t *testing.T) {
			if err := e.call(client); !errors.Is(err, ErrPullRequestNotFound) {
				t.Errorf("error = %v, want github_pull_request_not_found", err)
			}
		})
	}
}

func TestPullRequestDetailLists_TokenNeverInErrors(t *testing.T) {
	scenarios := []struct {
		name string
		call func(*Client) error
	}{
		{
			"reviews",
			func(c *Client) error {
				_, err := c.ListPullRequestReviews(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"review comments",
			func(c *Client) error {
				_, err := c.ListPullRequestReviewComments(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"issue comments",
			func(c *Client) error {
				_, err := c.ListPullRequestIssueComments(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
		{
			"files",
			func(c *Client) error {
				_, err := c.ListPullRequestFiles(context.Background(), issuesTestAccessToken, "o", "r", 1)
				return err
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			err := sc.call(client)

			if err == nil {
				t.Fatal("expected an error")
			}

			if strings.Contains(err.Error(), issuesTestAccessToken) {
				t.Errorf("error contains the access token: %q", err.Error())
			}
		})
	}
}

func TestListPullRequestChecks_Success(t *testing.T) {
	var gotPaths []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)

		switch r.URL.Path {
		case "/repos/o/r/commits/abcdef0123456789abcdef0123456789abcdef01/check-runs":
			w.Write([]byte(`{
				"total_count": 2,
				"check_runs": [
					{
						"name": "build",
						"status": "in_progress",
						"conclusion": null,
						"started_at": "2026-03-04T05:06:07Z",
						"completed_at": null,
						"html_url": "https://github.com/o/r/actions/runs/1"
					},
					{
						"name": "unit",
						"status": "completed",
						"conclusion": "failure",
						"started_at": "2026-03-04T05:06:07Z",
						"completed_at": "2026-03-04T05:06:08Z",
						"html_url": "https://github.com/o/r/actions/runs/2"
					}
				]
			}`))
		case "/repos/o/r/commits/abcdef0123456789abcdef0123456789abcdef01/status":
			w.Write([]byte(`{
				"state": "failure",
				"total_count": 1,
				"statuses": [{
					"context": "ci/check",
					"state": "failure",
					"description": "formatting failed",
					"target_url": "https://ci.example/job/12"
				}]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	checks, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abcdef0123456789abcdef0123456789abcdef01")

	if err != nil {
		t.Fatalf("ListPullRequestChecks() error: %v", err)
	}

	if len(gotPaths) != 2 ||
		gotPaths[0] != "/repos/o/r/commits/abcdef0123456789abcdef0123456789abcdef01/check-runs" ||
		gotPaths[1] != "/repos/o/r/commits/abcdef0123456789abcdef0123456789abcdef01/status" {
		t.Errorf("paths = %v, want check-runs then status", gotPaths)
	}

	if len(checks) != 3 {
		t.Fatalf("checks = %d, want 3 (2 check runs + 1 commit status)", len(checks))
	}

	if checks[0].Name != "build" || checks[0].Status != "in_progress" ||
		checks[0].Source != "check_run" || checks[0].CompletedAt != nil {
		t.Errorf("live check run = %+v, want in_progress preserved", checks[0])
	}

	if checks[1].Name != "unit" || checks[1].Status != "failure" ||
		checks[1].Source != "check_run" || checks[1].CompletedAt == nil {
		t.Errorf("completed check run = %+v, want its conclusion as the status", checks[1])
	}

	if checks[2].Name != "ci/check" || checks[2].Status != "failure" ||
		checks[2].Description != "formatting failed" ||
		checks[2].URL != "https://ci.example/job/12" ||
		checks[2].Source != "commit_status" {
		t.Errorf("commit status = %+v, want raw state and description preserved", checks[2])
	}
}

func TestListPullRequestChecks_MissingEndpointsYieldEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No commit found for SHA"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	checks, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abc")

	if err != nil {
		t.Fatalf("ListPullRequestChecks() error: %v", err)
	}

	if len(checks) != 0 {
		t.Errorf("checks = %d, want empty for 404 sub-queries", len(checks))
	}
}

func TestListPullRequestChecks_EmptyEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count": 0, "check_runs": [], "state": "no_statuses", "statuses": []}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	checks, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abc")

	if err != nil {
		t.Fatalf("ListPullRequestChecks() error: %v", err)
	}

	if len(checks) != 0 {
		t.Errorf("checks = %d, want 0 for an empty envelope", len(checks))
	}
}

func TestListPullRequestChecks_CheckRunServerErrorMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abc")

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestListPullRequestChecks_CompletedRunWithoutConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "check-runs") {
			w.Write([]byte(`{
				"total_count": 1,
				"check_runs": [{
					"name": "gate",
					"status": "completed",
					"conclusion": null,
					"html_url": "https://github.com/o/r/actions/runs/1"
				}]
			}`))
			return
		}
		w.Write([]byte(`{"state": "success", "total_count": 0, "statuses": []}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	checks, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abc")

	if err != nil {
		t.Fatalf("ListPullRequestChecks() error: %v", err)
	}

	if len(checks) != 1 || checks[0].Status != "completed" {
		t.Errorf("checks = %+v, want completed fallback when conclusion is empty", checks)
	}
}

func TestListPullRequestChecks_RejectsMalformedEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not-json`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.ListPullRequestChecks(
		context.Background(), issuesTestAccessToken, "o", "r", "abc")

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want github_invalid_response", err)
	}
}
