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

const issuesTestAccessToken = "ghs_issues_test_token_1234567890"

func issueJSON(id int64, number int, state string) string {
	return `{
		"id": ` + fmt.Sprint(id) + `,
		"number": ` + fmt.Sprint(number) + `,
		"title": "Fix login bug",
		"state": "` + state + `",
		"user": {"login": "octocat"},
		"html_url": "https://github.com/octocat/hello-world/issues/` + fmt.Sprint(number) + `",
		"created_at": "2026-01-02T03:04:05Z",
		"updated_at": "2026-02-03T04:05:06Z",
		"closed_at": null
	}`
}

func TestListRepositoryIssues_Success(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth, gotAccept, gotVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")

		w.Header().Set("Link", `<`+serverURL(r)+`?page=2&per_page=100>; rel="next"`)

		w.Write([]byte(`[` +
			issueJSON(1001, 1, "open") + `,` +
			issueJSON(1002, 2, "closed") + `,` +
			// A pull request: must be filtered out before validation.
			`{"id":1003,"number":3,"title":"PR entry","state":"open",
			  "user":{"login":"octocat"},
			  "html_url":"https://github.com/octocat/hello-world/pull/3",
			  "pull_request":{"html_url":"x"},"created_at":"2026-01-02T03:04:05Z"}` +
			`]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	issues, hasMore, err := client.ListRepositoryIssues(
		context.Background(),
		issuesTestAccessToken,
		"octocat",
		"hello-world",
		2,
		100,
	)

	if err != nil {
		t.Fatalf("ListRepositoryIssues() error: %v", err)
	}

	if gotPath != "/repos/octocat/hello-world/issues" {
		t.Errorf("path = %q, want /repos/octocat/hello-world/issues", gotPath)
	}

	for _, want := range []string{"page=2", "per_page=100", "state=all", "sort=updated", "direction=desc"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("query = %q, missing %q", gotRawQuery, want)
		}
	}

	if gotAuth != "Bearer "+issuesTestAccessToken {
		t.Errorf("Authorization = %q, want the caller's token as Bearer", gotAuth)
	}

	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}

	if gotVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", gotVersion)
	}

	if !hasMore {
		t.Error("hasMore = false, want true (Link rel=next present)")
	}

	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2 (pull request filtered out)", len(issues))
	}

	first := issues[0]

	if first.ID != 1001 || first.Number != 1 || first.State != "open" ||
		first.Title != "Fix login bug" || first.User.Login != "octocat" ||
		first.ClosedAt != nil || first.CreatedAt.IsZero() {
		t.Errorf("decoded issue = %+v, want mapped open-issue metadata", first)
	}

	if issues[1].State != "closed" {
		t.Errorf("second issue state = %q, want closed", issues[1].State)
	}
}

func serverURL(r *http.Request) string {
	scheme := "http"

	return scheme + "://" + r.Host + r.URL.Path
}

func TestListRepositoryIssues_NoNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, hasMore, err := client.ListRepositoryIssues(context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if hasMore {
		t.Error("hasMore = true for an empty last page")
	}
}

func TestListRepositoryIssues_StatusMapping(t *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		wantCode error
	}{
		{"404 unknown or renamed repository", http.StatusNotFound, ErrRepositoryNotFound},
		{"401 unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"403 forbidden", http.StatusForbidden, ErrUnauthorized},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrUnavailable},
		{"422 unexpected", http.StatusUnprocessableEntity, ErrAPIError},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.status)

				if sc.status != http.StatusNotFound {
					w.Write([]byte(`{"message":"nope"}`))
				}
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListRepositoryIssues(
				context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

			if !errors.Is(err, sc.wantCode) {
				t.Errorf("error = %v, want errors.Is(err, %v)", err, sc.wantCode)
			}

			if strings.Contains(err.Error(), issuesTestAccessToken) {
				t.Errorf("error contains the access token: %q", err.Error())
			}
		})
	}
}

func TestListRepositoryIssues_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, _, err := client.ListRepositoryIssues(context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestListRepositoryIssues_MalformedResponsesRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"not json", `{not-json`},
		{"trailing garbage", `[{"id":1,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}] trailing`},
		{"negative id", `[{"id":-3,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`},
		{"zero number", `[{"id":5,"number":0,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank title", `[{"id":5,"number":1,"title":"   ","state":"open","user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`},
		{"unsupported state", `[{"id":5,"number":1,"title":"t","state":"reopened","user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank author", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":""},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank html_url", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"","created_at":"2026-01-02T03:04:05Z"}]`},
		{"missing created_at", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u"}]`},
		{"object instead of array", `{"message":"moved"}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListRepositoryIssues(
				context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}

func TestListRepositoryIssues_OversizedResponseRejected(t *testing.T) {
	// One valid-looking array whose single element carries a >1MB title:
	// the response-size guard must turn it into an invalid response rather
	// than buffering unbounded attacker-controlled data.
	hugeTitle := strings.Repeat("A", maxResponseSize+1024)

	body := `[{"id":5,"number":1,"title":"` + hugeTitle + `","state":"open",
	          "user":{"login":"a"},"html_url":"u","created_at":"2026-01-02T03:04:05Z"}]`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, _, err := client.ListRepositoryIssues(context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("error = %v, want github_invalid_response for an oversized response", err)
	}
}
