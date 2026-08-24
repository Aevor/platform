package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Sentinel errors for the clone pipeline. Handlers translate these into the
// Aevor error contract; underlying causes are logged server-side only and
// never returned to clients (they can contain URLs or transport details).
var (
	// ErrInvalidCloneURL means the authoritative clone URL from GitHub did
	// not pass validation (scheme/host policy). Never a client-input problem.
	ErrInvalidCloneURL = errors.New("invalid clone url")

	// ErrTimeout means the clone exceeded its deadline.
	ErrTimeout = errors.New("clone timeout")

	// ErrAuthRejected means GitHub's Git endpoint rejected the credentials.
	ErrAuthRejected = errors.New("clone authentication rejected")

	// ErrCloneFailed covers every other clone/checkout failure.
	ErrCloneFailed = errors.New("clone failed")
)

// Cloner performs one authenticated repository clone into destDir. The
// accessToken is used ONLY for this operation: it must never be embedded in
// cloneURL, persisted to disk, logged, or stored in the cloned
// repository's configuration.
type Cloner interface {
	Clone(ctx context.Context, cloneURL string, branch string, accessToken string, destDir string) error
}

// DefaultAllowedHosts is the production allowlist for Git transports.
var DefaultAllowedHosts = []string{"github.com"}

// MakeCloneURLValidator builds the URL policy applied to the AUTHORITATIVE
// clone URL that GitHub itself reported. Only https to explicitly allowed
// hosts is accepted in production. file:// is opt-in for local development
// and integration verification via configuration — it is never enabled by
// default.
func MakeCloneURLValidator(allowedHosts []string, allowFileTransport bool) func(string) error {
	hosts := make(map[string]struct{}, len(allowedHosts))

	for _, host := range allowedHosts {
		hosts[strings.ToLower(strings.TrimSpace(host))] = struct{}{}
	}

	return func(cloneURL string) error {
		parsed, err := url.Parse(cloneURL)

		if err != nil || parsed.Scheme == "" || parsed.Host == "" && parsed.Scheme != "file" {
			return ErrInvalidCloneURL
		}

		// Embedded credentials in URLs are never acceptable: tokens belong
		// to per-operation transport auth only.
		if parsed.User != nil {
			return ErrInvalidCloneURL
		}

		switch strings.ToLower(parsed.Scheme) {
		case "https":
			if _, ok := hosts[strings.ToLower(parsed.Host)]; !ok {
				return ErrInvalidCloneURL
			}
		case "file":
			if !allowFileTransport {
				return ErrInvalidCloneURL
			}
		default:
			return ErrInvalidCloneURL
		}

		return nil
	}
}

// GoGitCloner is the production Cloner backed by go-git: pure Go, no shell,
// no subprocess, and therefore no hook execution, no repository-provided
// configuration execution, and no command-injection surface. Credentials are
// supplied per-request as HTTP basic auth to the Git smart-HTTP transport;
// the URL persisted into the workspace's .git/config stays credential-free.
//
// Clones are SHALLOW by default (depth 1, single branch): Task 3a needs the
// working-tree snapshot for future codebase ingestion, not history. This is
// also the primary resource guard against huge repositories.
type GoGitCloner struct {
	depth int
}

func NewGoGitCloner() *GoGitCloner {
	return &GoGitCloner{depth: 1}
}

// WithDepth overrides the shallow-clone depth; 0 disables shallowness
// entirely (used where the transport does not support it).
func (g *GoGitCloner) WithDepth(depth int) *GoGitCloner {
	g.depth = depth

	return g
}

func (g *GoGitCloner) Clone(
	ctx context.Context,
	cloneURL string,
	branch string,
	accessToken string,
	destDir string,
) error {
	options := &git.CloneOptions{
		URL:          cloneURL,
		SingleBranch: true,
		// The clean URL goes into .git/config; the token travels ONLY here.
		Auth: &http.BasicAuth{
			Username: "x-access-token",
			Password: accessToken,
		},
	}

	if branch != "" {
		options.ReferenceName = plumbing.NewBranchReferenceName(branch)
	}

	if g.depth > 0 {
		options.Depth = g.depth
	}

	_, err := git.PlainCloneContext(ctx, destDir, false, options)

	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed),
		errors.Is(err, transport.ErrInvalidAuthMethod):
		return fmt.Errorf("%w: %v", ErrAuthRejected, err)
	default:
		return fmt.Errorf("%w: %v", ErrCloneFailed, err)
	}
}

// VerifyTimeout maps a service-level deadline onto ErrTimeout so handlers can
// distinguish slow clones from other failures even when the cloner itself
// returned early.
func VerifyTimeout(started time.Time, timeout time.Duration, err error) error {
	if err != nil && time.Since(started) >= timeout {
		return errors.Join(ErrTimeout, err)
	}

	return err
}
