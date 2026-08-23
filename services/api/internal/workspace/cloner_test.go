package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestMakeCloneURLValidator(t *testing.T) {
	validator := MakeCloneURLValidator(DefaultAllowedHosts, false)

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "https github", url: "https://github.com/octocat/hello-world.git"},
		{name: "https github no git suffix", url: "https://github.com/octocat/hello-world"},
		{name: "empty string", url: "", wantErr: true},
		{name: "not a url", url: "::::", wantErr: true},
		{name: "http scheme", url: "http://github.com/octocat/hello-world.git", wantErr: true},
		{name: "disallowed host", url: "https://evil.example.com/octocat/hello.git", wantErr: true},
		{name: "host lookalike", url: "https://github.com.evil.example.com/octocat/hello.git", wantErr: true},
		{name: "ssh scp syntax", url: "git@github.com:octocat/hello.git", wantErr: true},
		{name: "ssh scheme", url: "ssh://git@github.com/octocat/hello.git", wantErr: true},
		{name: "file transport denied by default", url: "file:///tmp/repo", wantErr: true},
		{
			name:    "embedded userinfo",
			url:     "https://ghs_token@github.com/octocat/hello.git",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validator(tc.url)

			if tc.wantErr && !errors.Is(err, ErrInvalidCloneURL) {
				t.Errorf("validator(%q) = %v, want ErrInvalidCloneURL", tc.url, err)
			}

			if !tc.wantErr && err != nil {
				t.Errorf("validator(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

func TestMakeCloneURLValidator_FileOptIn(t *testing.T) {
	validator := MakeCloneURLValidator(DefaultAllowedHosts, true)

	if err := validator("file:///tmp/aevor-source"); err != nil {
		t.Errorf("file transport with opt-in = %v, want nil", err)
	}
}

func TestGoGitCloner_RejectsUnreachableURL(t *testing.T) {
	cloner := NewGoGitCloner()

	err := cloner.Clone(context.Background(), "https://127.0.0.1:1/nope.git", "", "", t.TempDir()+"/dest")

	if !errors.Is(err, ErrCloneFailed) {
		t.Errorf("unreachable clone error = %v, want ErrCloneFailed", err)
	}
}
