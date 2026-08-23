package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const twoRepositoriesJSON = `[
  {
    "id": 1296269,
    "name": "hello-world",
    "full_name": "octocat/hello-world",
    "private": false,
    "description": "My first repository",
    "default_branch": "master",
    "owner": {"login": "octocat"},
    "html_url": "https://github.com/octocat/hello-world",
    "clone_url": "https://github.com/octocat/hello-world.git",
    "url": "https://api.github.com/repos/octocat/hello-world"
  },
  {
    "id": 1296270,
    "name": "hello-wasm",
    "full_name": "octocat/hello-wasm",
    "private": true,
    "description": "",
    "default_branch": "main",
    "owner": {"login": "octocat"},
    "html_url": "https://github.com/octocat/hello-wasm",
    "clone_url": "https://github.com/octocat/hello-wasm.git",
    "url": "https://api.github.com/repos/octocat/hello-wasm"
  }
]`

func TestListUserRepositories_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(twoRepositoriesJSON))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	repositories, hasMore, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

	if err != nil {
		t.Fatalf("ListUserRepositories() error: %v", err)
	}

	if len(repositories) != 2 {
		t.Fatalf("repository count = %d, want 2", len(repositories))
	}

	first := repositories[0]

	if first.ID != 1296269 ||
		first.Name != "hello-world" ||
		first.FullName != "octocat/hello-world" ||
		first.Private ||
		first.Description != "My first repository" ||
		first.DefaultBranch != "master" ||
		first.Owner.Login != "octocat" ||
		first.HTMLURL != "https://github.com/octocat/hello-world" ||
		first.CloneURL != "https://github.com/octocat/hello-world.git" ||
		first.APIURL != "https://api.github.com/repos/octocat/hello-world" {
		t.Errorf("decoded repository = %+v, want the full GitHub repository payload", first)
	}

	if !repositories[1].Private {
		t.Errorf("second repository private = false, want true")
	}

	if repositories[1].DefaultBranch != "main" {
		t.Errorf("second repository default_branch = %q, want main", repositories[1].DefaultBranch)
	}

	if hasMore {
		t.Error("hasMore = true without a Link rel=next header")
	}
}

func TestListUserRepositories_SendsRequestShape(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotAccept, gotAPIVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")

		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	if _, _, err := client.ListUserRepositories(context.Background(), testAccessToken, 2, 50); err != nil {
		t.Fatalf("ListUserRepositories() error: %v", err)
	}

	if gotPath != "/user/repos" {
		t.Errorf("request path = %q, want /user/repos", gotPath)
	}

	wantQuery := "sort=full_name&direction=asc&page=2&per_page=50"

	if gotQuery != wantQuery {
		t.Errorf("query = %q, want %q (deterministic ordering)", gotQuery, wantQuery)
	}

	if gotAuth != "Bearer "+testAccessToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+testAccessToken)
	}

	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want the GitHub JSON media type", gotAccept)
	}

	if gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q, want 2022-11-28", gotAPIVersion)
	}
}

func TestListUserRepositories_EmptyListIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	repositories, _, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

	if err != nil {
		t.Fatalf("ListUserRepositories() error: %v", err)
	}

	if len(repositories) != 0 {
		t.Errorf("repository count = %d, want 0", len(repositories))
	}
}

func TestListUserRepositories_HasNextPageFromLinkHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(
			"Link",
			`<https://api.github.com/user/repos?sort=full_name&page=2>; rel="next", <https://api.github.com/user/repos?sort=full_name&page=5>; rel="last"`,
		)
		w.Write([]byte(twoRepositoriesJSON))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, hasMore, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

	if err != nil {
		t.Fatalf("ListUserRepositories() error: %v", err)
	}

	if !hasMore {
		t.Error("hasMore = false, want true when Link advertises rel=\"next\"")
	}
}

func TestListUserRepositories_LastPageHasNoNext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(
			"Link",
			`<https://api.github.com/user/repos?sort=full_name&page=4>; rel="prev", <https://api.github.com/user/repos?sort=full_name&page=1>; rel="first"`,
		)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, hasMore, err := client.ListUserRepositories(context.Background(), testAccessToken, 4, 30)

	if err != nil {
		t.Fatalf("ListUserRepositories() error: %v", err)
	}

	if hasMore {
		t.Error("hasMore = true on the last page")
	}
}

func TestListUserRepositories_StatusMapping(t *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		wantCode error
	}{
		{"401 unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"403 forbidden", http.StatusForbidden, ErrUnauthorized},
		{"429 rate limited", http.StatusTooManyRequests, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, ErrUnavailable},
		{"502 bad gateway", http.StatusBadGateway, ErrUnavailable},
		{"404 unexpected", http.StatusNotFound, ErrAPIError},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.status)
				w.Write([]byte(`{"message":"nope"}`))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

			if !errors.Is(err, sc.wantCode) {
				t.Errorf("error = %v, want errors.Is(err, %v)", err, sc.wantCode)
			}

			if strings.Contains(err.Error(), testAccessToken) {
				t.Errorf("error contains the GitHub access token: %q", err.Error())
			}
		})
	}
}

func TestListUserRepositories_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, _, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestListUserRepositories_MalformedResponsesRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"not json", `{not-json`},
		{"object instead of array", `{"id":1}`},
		{"trailing garbage", `[{"id":1,"full_name":"a/b"}] trailing`},
		{"negative id", `[{"id":-3,"full_name":"evil/repo"}]`},
		{"blank full_name", `[{"id":7,"full_name":"   "}]`},
		{"missing full_name", `[{"id":7}]`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, _, err := client.ListUserRepositories(context.Background(), testAccessToken, 1, 30)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}
