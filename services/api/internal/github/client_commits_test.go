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

const (
	commitSHAA = "3a1f9c4b7d2e8f6051a4b3c2d1e0f9876543210a"
	commitSHAB = "bb29c8d7e6f504132a1b0c9d8e7f6a5b4c3d2e1f"
)

// commitJSON builds one GitHub commit list entry. login may be empty to
// emulate a commit whose email matches no GitHub account.
func commitJSON(sha, message, authorName, authorEmail, authorLogin string) string {
	var account string

	if authorLogin == "" {
		account = `"author": null,`
	} else {
		account = fmt.Sprintf(`"author": {"login": "%s"},`, authorLogin)
	}

	urlSHA := sha

	if len(urlSHA) > 12 {
		urlSHA = urlSHA[:12]
	}

	return fmt.Sprintf(`{
		"sha": "%s",
		%s
		"html_url": "https://github.com/octocat/hello-world/commit/%s",
		"commit": {
			"message": "%s",
			"author": {"name": "%s", "email": "%s", "date": "2026-01-02T03:04:05Z"},
			"committer": {"name": "Committer One", "email": "c@example.com", "date": "2026-02-03T04:05:06Z"}
		}
	}`, sha, strings.TrimSpace(account), urlSHA, message, authorName, authorEmail)
}

func TestListRepositoryCommits_Success(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth, gotVersion, gotAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		gotAccept = r.Header.Get("Accept")

		w.Header().Set("Link", `<`+serverURL(r)+`?page=2&per_page=100>; rel="next"`)

		w.Write([]byte(`[` +
			commitJSON(commitSHAA, "Add pagination", "Alice Author", "alice@example.com", "octocat") + `,` +
			commitJSON(commitSHAB, "Fix race", "Bob Builder", "bob@example.com", "") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	commits, hasMore, err := client.ListRepositoryCommits(
		context.Background(),
		issuesTestAccessToken,
		"octocat",
		"hello-world",
		1,
		100,
	)

	if err != nil {
		t.Fatalf("ListRepositoryCommits() error: %v", err)
	}

	if gotPath != "/repos/octocat/hello-world/commits" {
		t.Errorf("path = %q, want /repos/octocat/hello-world/commits", gotPath)
	}

	for _, want := range []string{"page=1", "per_page=100"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("query = %q, missing %q", gotRawQuery, want)
		}
	}

	if gotAuth != "Bearer "+issuesTestAccessToken {
		t.Errorf("Authorization = %q, want the caller's token as Bearer", gotAuth)
	}

	if gotVersion != "2022-11-28" || gotAccept != "application/vnd.github+json" {
		t.Errorf("headers wrong: version=%q accept=%q", gotVersion, gotAccept)
	}

	if !hasMore {
		t.Error("hasMore = false, want true (Link rel=next present)")
	}

	if len(commits) != 2 {
		t.Fatalf("commits = %d, want 2", len(commits))
	}

	first := commits[0]

	if first.SHA != commitSHAA ||
		first.Commit.Message != "Add pagination" ||
		first.Commit.Author.Name != "Alice Author" ||
		first.Commit.Author.Email != "alice@example.com" ||
		first.Commit.Committer.Name != "Committer One" ||
		first.Author == nil || first.Author.Login != "octocat" ||
		first.HTMLURL != "https://github.com/octocat/hello-world/commit/"+commitSHAA[:12] ||
		first.Commit.Author.Date.IsZero() || first.Commit.Committer.Date.IsZero() {
		t.Errorf("decoded commit = %+v, want mapped commit metadata", first)
	}

	if commits[1].Author != nil {
		t.Errorf("unlinked commit should decode with nil account, got %+v", commits[1].Author)
	}
}

func TestListRepositoryCommits_NormalizesSHAToLowercase(t *testing.T) {
	mixedCase := "3A1F9C4B7D2E8F6051A4B3C2D1E0F9876543210A"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[` + commitJSON(mixedCase, "Mixed case SHA", "Alice", "a@e.com", "octocat") + `]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	commits, _, err := client.ListRepositoryCommits(
		context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if commits[0].SHA != commitSHAA {
		t.Errorf("SHA = %q, want normalized %q", commits[0].SHA, commitSHAA)
	}
}

func TestListRepositoryCommits_NoNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, hasMore, err := client.ListRepositoryCommits(
		context.Background(), issuesTestAccessToken, "o", "r", 3, 30)

	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if hasMore {
		t.Error("hasMore = true for an empty last page")
	}
}

func TestListRepositoryCommits_StatusMapping(t *testing.T) {
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

			_, _, err := client.ListRepositoryCommits(
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

func TestListRepositoryCommits_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, _, err := client.ListRepositoryCommits(
		context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestListRepositoryCommits_MalformedResponsesRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"not json", `{not-json`},
		{"trailing garbage", `[` + commitJSON(commitSHAA, "ok", "Alice", "a@e.com", "x") + `] trailing`},
		{"short sha", `[` + commitJSON("abc123", "ok", "Alice", "a@e.com", "x") + `]`},
		{"sha with non-hex characters", `[` + commitJSON("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "ok", "Alice", "a@e.com", "x") + `]`},
		{"blank message", `[` + commitJSON(commitSHAA, "   ", "Alice", "a@e.com", "x") + `]`},
		{"blank html_url", `[{"sha":"` + commitSHAA + `","message":"ok","html_url":"","commit":{"author":{"name":"Alice","email":"a@e.com","date":"2026-01-02T03:04:05Z"},"committer":{"name":"C","email":"c@e.com","date":"2026-02-03T04:05:06Z"}},"author":null}]`},
		{"blank author name", `[` + commitJSON(commitSHAA, "ok", "  ", "a@e.com", "x") + `]`},
		{"missing authored date", `[{"sha":"` + commitSHAA + `","message":"ok","html_url":"u","commit":{"author":{"name":"Alice","email":"a@e.com"},"committer":{"name":"C","email":"c@e.com","date":"2026-02-03T04:05:06Z"}},"author":null}]`},
		{"missing committed date", `[{"sha":"` + commitSHAA + `","message":"ok","html_url":"u","commit":{"author":{"name":"Alice","email":"a@e.com","date":"2026-01-02T03:04:05Z"},"committer":{"name":"C","email":"c@e.com"}},"author":null}]`},
		{"object instead of array", `{"message":"moved"}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListRepositoryCommits(
				context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}

func TestListRepositoryCommits_OversizedResponseRejected(t *testing.T) {
	hugeMessage := strings.Repeat("M", maxResponseSize+1024)

	body := `[{"sha":"` + commitSHAA + `","message":"` + hugeMessage + `","html_url":"u",
	          "commit":{"author":{"name":"Alice","email":"a@e.com","date":"2026-01-02T03:04:05Z"},
	                    "committer":{"name":"C","email":"c@e.com","date":"2026-02-03T04:05:06Z"}},
	          "author":null}]`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, _, err := client.ListRepositoryCommits(
		context.Background(), issuesTestAccessToken, "o", "r", 1, 30)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("error = %v, want github_invalid_response for an oversized response", err)
	}
}
