package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetRepository_Success(t *testing.T) {
	var gotPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{
			"id": 1296269,
			"name": "hello-world",
			"full_name": "octocat/hello-world",
			"private": true,
			"description": "desc",
			"default_branch": "main",
			"owner": {"login": "octocat"},
			"html_url": "https://github.com/octocat/hello-world",
			"clone_url": "https://github.com/octocat/hello-world.git",
			"url": "https://api.github.com/repos/octocat/hello-world"
		}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	repository, err := client.GetRepository(context.Background(), testAccessToken, 1296269)

	if err != nil {
		t.Fatalf("GetRepository() error: %v", err)
	}

	if gotPath != "/repositories/1296269" {
		t.Errorf("request path = %q, want /repositories/1296269", gotPath)
	}

	if repository.ID != 1296269 ||
		repository.Name != "hello-world" ||
		repository.FullName != "octocat/hello-world" ||
		!repository.Private ||
		repository.DefaultBranch != "main" ||
		repository.Owner.Login != "octocat" ||
		repository.HTMLURL != "https://github.com/octocat/hello-world" {
		t.Errorf("decoded repository = %+v, want the full GitHub repository payload", repository)
	}
}

func TestGetRepository_NotFoundIsDistinct(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetRepository(context.Background(), testAccessToken, 999)

	if !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("error = %v, want ErrRepositoryNotFound", err)
	}
}

func TestGetRepository_StatusMapping(t *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		wantCode error
	}{
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
				w.Write([]byte(`{"message":"nope"}`))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.GetRepository(context.Background(), testAccessToken, 42)

			if !errors.Is(err, sc.wantCode) {
				t.Errorf("error = %v, want errors.Is(err, %v)", err, sc.wantCode)
			}

			if strings.Contains(err.Error(), testAccessToken) {
				t.Errorf("error contains the GitHub access token: %q", err.Error())
			}
		})
	}
}

func TestGetRepository_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, err := client.GetRepository(context.Background(), testAccessToken, 42)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestGetRepository_MalformedResponsesRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"not json", `{not-json`},
		{"trailing garbage", `{"id":1,"full_name":"a/b"} trailing`},
		{"negative id", `{"id":-3,"full_name":"evil/repo"}`},
		{"blank full_name", `{"id":7,"full_name":"   "}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.GetRepository(context.Background(), testAccessToken, 42)

			if !errors.Is(err, ErrInvalidResponse) {
				t.Errorf("error = %v, want github_invalid_response", err)
			}
		})
	}
}
