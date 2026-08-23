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

func pullRequestJSON(id int64, number int, title string, state string) string {
	return `{
		"id": ` + fmt.Sprint(id) + `,
		"number": ` + fmt.Sprint(number) + `,
		"title": "` + title + `",
		"state": "` + state + `",
		"user": {"login": "octocat"},
		"html_url": "https://github.com/octocat/hello-world/pull/` + fmt.Sprint(number) + `",
		"head": {"ref": "feature-branch"},
		"base": {"ref": "main"},
		"draft": false,
		"merged": false,
		"created_at": "2026-01-02T03:04:05Z",
		"updated_at": "2026-02-03T04:05:06Z",
		"closed_at": null,
		"merged_at": null
	}`
}

func TestListRepositoryPullRequests_Success(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth, gotVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")

		w.Header().Set("Link", `<`+serverURL(r)+`?page=2&per_page=100>; rel="next"`)

		w.Write([]byte(`[` +
			pullRequestJSON(7001, 10, "Add pagination", "open") + `,` +
			pullRequestJSON(7002, 11, "Fix race", "closed") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	pullRequests, hasMore, err := client.ListRepositoryPullRequests(
		context.Background(),
		issuesTestAccessToken,
		"octocat",
		"hello-world",
		1,
		100,
	)

	if err != nil {
		t.Fatalf("ListRepositoryPullRequests() error: %v", err)
	}

	if gotPath != "/repos/octocat/hello-world/pulls" {
		t.Errorf("path = %q, want /repos/octocat/hello-world/pulls", gotPath)
	}

	for _, want := range []string{"page=1", "per_page=100", "state=all", "sort=updated", "direction=desc"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("query = %q, missing %q", gotRawQuery, want)
		}
	}

	if gotAuth != "Bearer "+issuesTestAccessToken {
		t.Errorf("Authorization = %q, want the caller's token as Bearer", gotAuth)
	}

	if gotVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", gotVersion)
	}

	if !hasMore {
		t.Error("hasMore = false, want true (Link rel=next present)")
	}

	if len(pullRequests) != 2 {
		t.Fatalf("pull requests = %d, want 2", len(pullRequests))
	}

	first := pullRequests[0]

	if first.ID != 7001 || first.Number != 10 || first.State != "open" ||
		first.Title != "Add pagination" || first.User.Login != "octocat" ||
		first.Head.Ref != "feature-branch" || first.Base.Ref != "main" ||
		first.Draft || first.Merged || first.ClosedAt != nil || first.MergedAt != nil ||
		first.CreatedAt.IsZero() {
		t.Errorf("decoded PR = %+v, want mapped open-PR metadata", first)
	}

	if pullRequests[1].State != "closed" {
		t.Errorf("second PR state = %q, want closed", pullRequests[1].State)
	}
}

func TestListRepositoryPullRequests_NoNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, hasMore, err := client.ListRepositoryPullRequests(
		context.Background(), issuesTestAccessToken, "o", "r", 3, 30)

	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if hasMore {
		t.Error("hasMore = true for an empty last page")
	}
}

func TestListRepositoryPullRequests_StatusMapping(t *testing.T) {
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

			_, _, err := client.ListRepositoryPullRequests(
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

func TestListRepositoryPullRequests_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, _, err := client.ListRepositoryPullRequests(
		context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestListRepositoryPullRequests_MalformedResponsesRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"not json", `{not-json`},
		{"trailing garbage", `[{"id":1,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}] trailing`},
		{"negative id", `[{"id":-3,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"zero number", `[{"id":5,"number":0,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank title", `[{"id":5,"number":1,"title":"   ","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"unsupported state", `[{"id":5,"number":1,"title":"t","state":"draft-only","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank author", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":""},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank html_url", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"","head":{"ref":"h"},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank head ref", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":""},"base":{"ref":"b"},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"blank base ref", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"  "},"created_at":"2026-01-02T03:04:05Z"}]`},
		{"missing created_at", `[{"id":5,"number":1,"title":"t","state":"open","user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"}}]`},
		{"object instead of array", `{"message":"moved"}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListRepositoryPullRequests(
				context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}

func TestListRepositoryPullRequests_OversizedResponseRejected(t *testing.T) {
	hugeTitle := strings.Repeat("P", maxResponseSize+1024)

	body := `[{"id":5,"number":1,"title":"` + hugeTitle + `","state":"open",
	          "user":{"login":"a"},"html_url":"u","head":{"ref":"h"},"base":{"ref":"b"},
	          "created_at":"2026-01-02T03:04:05Z"}]`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, _, err := client.ListRepositoryPullRequests(
		context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("error = %v, want github_invalid_response for an oversized response", err)
	}
}
