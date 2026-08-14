package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAccessToken = "ghs_aevor-test-token-do-not-leak"

func TestGetCurrentUser_Success(t *testing.T) {
	var gotPath, gotHost string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHost = r.Host

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":583231,"login":"octocat","name":"The Octocat","email":"octocat@example.com","avatar_url":"https://avatars.githubusercontent.com/u/583231?v=4"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	user, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	if gotPath != "/user" {
		t.Errorf("request path = %q, want /user", gotPath)
	}

	if gotHost != server.Listener.Addr().String() {
		t.Errorf("request host = %q, want the mock server %q (no real GitHub request)", gotHost, server.Listener.Addr().String())
	}

	if user.ID != 583231 {
		t.Errorf("user.ID = %d, want 583231", user.ID)
	}

	if user.Login != "octocat" {
		t.Errorf("user.Login = %q, want octocat", user.Login)
	}

	if user.Name != "The Octocat" {
		t.Errorf("user.Name = %q, want The Octocat", user.Name)
	}

	if user.Email != "octocat@example.com" {
		t.Errorf("user.Email = %q, want octocat@example.com", user.Email)
	}

	if user.AvatarURL != "https://avatars.githubusercontent.com/u/583231?v=4" {
		t.Errorf("user.AvatarURL = %q, want the GitHub avatar URL", user.AvatarURL)
	}
}

func TestGetCurrentUser_SendsBearerAuthorizationHeader(t *testing.T) {
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")

		w.Write([]byte(`{"id":583231,"login":"octocat"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	if _, err := client.GetCurrentUser(context.Background(), testAccessToken); err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	if gotAuth != "Bearer "+testAccessToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+testAccessToken)
	}
}

func TestGetCurrentUser_SendsUserAgent(t *testing.T) {
	var gotUA string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")

		w.Write([]byte(`{"id":583231,"login":"octocat"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	if _, err := client.GetCurrentUser(context.Background(), testAccessToken); err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	if gotUA == "" {
		t.Fatal("User-Agent header is missing")
	}

	if !strings.Contains(gotUA, "Aevor") {
		t.Errorf("User-Agent %q does not identify Aevor", gotUA)
	}
}

func TestGetCurrentUser_DecodesGitHubProfileJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":42,"login":"defunkt","name":"Chris Wanstrath","email":"","avatar_url":"https://avatars.githubusercontent.com/u/42"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	user, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	if user.ID != 42 || user.Login != "defunkt" || user.Name != "Chris Wanstrath" || user.Email != "" || user.AvatarURL != "https://avatars.githubusercontent.com/u/42" {
		t.Errorf("decoded user = %+v, want the full GitHub profile", user)
	}
}

func TestGetCurrentUser_UnauthorizedMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want github_api_unauthorized", err)
	}
}

func TestGetCurrentUser_ForbiddenMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want github_api_unauthorized", err)
	}
}

func TestGetCurrentUser_RateLimitedMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want github_rate_limited", err)
	}
}

func TestGetCurrentUser_ServerErrorMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Internal Server Error"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestGetCurrentUser_UnexpectedStatusMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrAPIError) {
		t.Fatalf("error = %v, want github_api_error", err)
	}
}

func TestGetCurrentUser_NetworkFailureMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewClient(nil, WithBaseURL(serverURL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestGetCurrentUser_TimeoutMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewClient(&http.Client{Timeout: 50 * time.Millisecond}, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want github_unavailable", err)
	}
}

func TestGetCurrentUser_MalformedJSONMapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not-json`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want github_invalid_response", err)
	}
}

func TestGetCurrentUser_TrailingGarbageRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":583231,"login":"octocat"} trailing-garbage`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want github_invalid_response", err)
	}
}

func TestGetCurrentUser_EmptyUserRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want github_invalid_response", err)
	}
}

func TestGetCurrentUser_DoesNotFollowRedirects(t *testing.T) {
	var redirectTargetHit bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			http.Redirect(w, r, "/evil", http.StatusFound)
			return
		}

		redirectTargetHit = true
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if !errors.Is(err, ErrAPIError) {
		t.Fatalf("error = %v, want github_api_error for a redirect response", err)
	}

	if redirectTargetHit {
		t.Error("client followed a redirect to a different destination")
	}
}

type recordingBody struct {
	io.ReadCloser
	closed *bool
}

func (b *recordingBody) Close() error {
	*b.closed = true

	return b.ReadCloser.Close()
}

type closeRecordingTransport struct {
	closed *bool
}

func (t *closeRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body: &recordingBody{
			ReadCloser: io.NopCloser(strings.NewReader(`{"id":583231,"login":"octocat"}`)),
			closed:     t.closed,
		},
		Request: req,
	}, nil
}

func TestGetCurrentUser_ClosesResponseBody(t *testing.T) {
	closed := false

	client := NewClient(&http.Client{Transport: &closeRecordingTransport{closed: &closed}}, WithBaseURL("http://mock.invalid"))

	user, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	if user == nil {
		t.Fatal("GetCurrentUser() returned a nil user")
	}

	if !closed {
		t.Error("response body was not closed")
	}
}

func TestGetCurrentUser_ErrorsDoNotContainAccessToken(t *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		body     string
		wantCode error
	}{
		{"401 unauthorized", http.StatusUnauthorized, `{"message":"Bad credentials"}`, ErrUnauthorized},
		{"403 forbidden", http.StatusForbidden, `{"message":"Forbidden"}`, ErrUnauthorized},
		{"429 rate limited", http.StatusTooManyRequests, `{"message":"API rate limit exceeded"}`, ErrRateLimited},
		{"500 server error", http.StatusInternalServerError, `{"message":"Internal Server Error"}`, ErrUnavailable},
		{"404 unexpected", http.StatusNotFound, `{"message":"Not Found"}`, ErrAPIError},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.status)
				w.Write([]byte(sc.body))
			}))
			defer server.Close()

			client := NewClient(nil, WithBaseURL(server.URL))

			_, err := client.GetCurrentUser(context.Background(), testAccessToken)

			if err == nil {
				t.Fatalf("expected an error for status %d", sc.status)
			}

			if !errors.Is(err, sc.wantCode) {
				t.Errorf("error = %v, want errors.Is(err, %v)", err, sc.wantCode)
			}

			if strings.Contains(err.Error(), testAccessToken) {
				t.Errorf("error contains the GitHub access token: %q", err.Error())
			}
		})
	}
}

func TestGetCurrentUser_DoesNotLogAccessToken(t *testing.T) {
	var buf bytes.Buffer

	oldOutput := log.Writer()
	log.SetOutput(&buf)

	defer log.SetOutput(oldOutput)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	_, _ = client.GetCurrentUser(context.Background(), testAccessToken)

	if strings.Contains(buf.String(), testAccessToken) {
		t.Error("log output contains the GitHub access token")
	}
}

func TestGetCurrentUser_UserJSONExcludesAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":583231,"login":"octocat"}`))
	}))
	defer server.Close()

	client := NewClient(nil, WithBaseURL(server.URL))

	user, err := client.GetCurrentUser(context.Background(), testAccessToken)

	if err != nil {
		t.Fatalf("GetCurrentUser() error: %v", err)
	}

	raw, err := json.Marshal(user)

	if err != nil {
		t.Fatalf("json.Marshal(user) error: %v", err)
	}

	if strings.Contains(string(raw), testAccessToken) {
		t.Error("GitHubUser JSON contains the access token")
	}
}

func TestNewClient_DefaultBaseURLIsGitHub(t *testing.T) {
	client := NewClient(nil)

	if client.baseURL != "https://api.github.com" {
		t.Errorf("default base URL = %q, want https://api.github.com", client.baseURL)
	}
}

func TestNewClient_WithBaseURLOverridesDefault(t *testing.T) {
	client := NewClient(nil, WithBaseURL("https://mock.example"))

	if client.baseURL != "https://mock.example" {
		t.Errorf("base URL = %q, want https://mock.example", client.baseURL)
	}
}
