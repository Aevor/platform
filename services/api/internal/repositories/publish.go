package repositories

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Delivery states for RepositoryChangeSetDelivery.Status. The change set's
// own status is left in changeSetStatusValidated throughout: a delivery
// failure is a property of the delivery record, not of the validated change
// set.
const (
	// deliveryStatusBranchCreated is recorded once the deterministic branch
	// has been created/checked out locally.
	deliveryStatusBranchCreated = "branch_created"

	// deliveryStatusCommitted is recorded once the applied paths are
	// committed on the delivery branch.
	deliveryStatusCommitted = "committed"

	// deliveryStatusPushed is recorded once the branch is present on the
	// remote at the delivered commit.
	deliveryStatusPushed = "pushed"

	// deliveryStatusOpen is the terminal success state: the pull request
	// exists (created by us or already present) with the delivered branch as
	// its head.
	deliveryStatusOpen = "open"

	deliveryStatusBranchCreationFailed = "branch_creation_failed"
	deliveryStatusCommitFailed         = "commit_failed"
	deliveryStatusPushFailed           = "push_failed"
	deliveryStatusPRCreationFailed     = "pr_creation_failed"
)

// defaultDeliveryTimeout bounds the git side of one publish (remote ref
// listing) through a request-scoped context.
const defaultDeliveryTimeout = 3 * time.Minute

// deliveryActiveStatuses are the statuses a delivery may still be advanced
// from. The terminal open status is excluded: once opened, a delivery is
// final (re-publishes return the existing pull request).
var deliveryActiveStatuses = []string{
	deliveryStatusBranchCreated,
	deliveryStatusCommitted,
	deliveryStatusPushed,
	deliveryStatusBranchCreationFailed,
	deliveryStatusCommitFailed,
	deliveryStatusPushFailed,
	deliveryStatusPRCreationFailed,
}

// deliveryResumableStatuses mirror deliveryActiveStatuses: every active
// (non-open) status may advance forward, so a retry after a *_failed step
// resumes cleanly instead of deadlocking on a stale allowed list.
var deliveryResumableStatuses = deliveryActiveStatuses

// changeSetPublisher is the git backend for publishing a validated change
// set. It is deliberately a small deterministic surface over the workspace
// repository, mirroring the changeSetValidator contract: the production
// implementation (goGitPublisher) sits behind it and tests substitute a fake.
// All operations only ever stage the exact repository-relative paths passed
// in — never a whole-tree add.
type changeSetPublisher interface {
	// EnsureBranch makes the deterministic delivery branch exist locally and
	// checks the workspace out onto it. When the branch already exists it is
	// left untouched and checked out; a conflicting pre-existing local branch
	// is never deleted or overwritten.
	//
	// The branch is created from the remote-tracking ref of baseBranch when
	// available, otherwise from the current HEAD, so a resumed delivery never
	// branches from the middle of a previous delivery.
	EnsureBranch(ctx context.Context, dir, branch, baseBranch string) error

	// HeadSHA returns the SHA of the currently checked out commit.
	HeadSHA(ctx context.Context, dir string) (string, error)

	// PathsUpToDate reports whether every path in the change set is committed
	// cleanly at the current commit (nothing further to commit).
	PathsUpToDate(ctx context.Context, dir string, paths []string) (bool, error)

	// CommitChanges stages exactly the given paths (delete paths are staged
	// as removals) and commits them on the current branch. It returns the new
	// commit SHA. Nothing outside the listed paths is staged, and no window
	// into the working tree is committed.
	CommitChanges(ctx context.Context, dir string, paths []string, message string, author Signature) (string, error)

	// RemoteBranchExists reports whether the branch exists on the named
	// remote, and if so its tip SHA.
	RemoteBranchExists(ctx context.Context, dir, remote, branch string) (exists bool, sha string, err error)

	// PushBranch pushes the delivery branch to the named remote. auth is used
	// for http(s) remotes and ignored for file remotes; the token is passed
	// in-band, never written to config, URLs, or logs.
	PushBranch(ctx context.Context, dir, remote, branch string, auth pushAuth) error
}

// Signature identifies one delivery commit author. Aevor always commits under
// the acting user's account.
type Signature struct {
	Name  string
	Email string
}

// pushAuth carries the GitHub token used to authenticate a push. It is kept
// as an opaque value so the delivery layer can hand it to the git transport
// without ever logging or persisting it.
type pushAuth interface {
	AuthMethod() transport.AuthMethod
}

// tokenPushAuth implements pushAuth for http(s) remotes. The token travels
// in the request Authorization header only; it is never embedded in the
// remote URL or persisted anywhere.
type tokenPushAuth struct {
	token string
}

func (a tokenPushAuth) AuthMethod() transport.AuthMethod {
	return &githttp.BasicAuth{
		Username: "x-access-token",
		Password: a.token,
	}
}

// goGitPublisher is the production changeSetPublisher backed by go-git: pure
// Go, no shell invocation, no subprocess. Every operation is confined to the
// exact change-set paths and the delivery branch.
type goGitPublisher struct{}

func newGoGitPublisher() changeSetPublisher {
	return &goGitPublisher{}
}

func openWorkspaceRepo(dir string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: open workspace repository", ErrDeliveryFailed)
	}
	return repo, nil
}

func (p *goGitPublisher) EnsureBranch(ctx context.Context, dir, branch, baseBranch string) error {
	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return err
	}

	branchRef := plumbing.NewBranchReferenceName(branch)

	if _, err := repo.Reference(branchRef, false); err == nil {
		// Branch already exists locally: point HEAD at it WITHOUT a
		// checkout. A checkout would rewrite the worktree to the branch
		// tree and could destroy the still-uncommitted applied change-set
		// files; the commit step stages exactly those paths afterwards.
		return pointHEADAt(repo, branchRef)
	}

	// Determine the commit the branch is created from: the base branch's
	// remote-tracking tip when known, otherwise HEAD.
	var base plumbing.Hash
	if baseBranch != "" {
		if baseRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), false); err == nil {
			base = baseRef.Hash()
		}
	}
	if base.IsZero() {
		head, err := repo.Head()
		if err != nil {
			return fmt.Errorf("%w: resolve HEAD", ErrDeliveryFailed)
		}
		base = head.Hash()
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, base)); err != nil {
		return fmt.Errorf("%w: create delivery branch", ErrDeliveryFailed)
	}

	return pointHEADAt(repo, branchRef)
}

// pointHEADAt flips the repository's symbolic HEAD without touching the
// worktree. The filesystem is left byte-identical, so uncommitted change-set
// files survive a branch switch and the subsequent commit stages them
// verbatim.
func pointHEADAt(repo *gogit.Repository, branchRef plumbing.ReferenceName) error {
	reference := plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)

	if err := repo.Storer.SetReference(reference); err != nil {
		return fmt.Errorf("%w: point HEAD at delivery branch", ErrDeliveryFailed)
	}

	return nil
}

func (p *goGitPublisher) HeadSHA(ctx context.Context, dir string) (string, error) {
	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return "", err
	}

	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("%w: resolve HEAD", ErrDeliveryFailed)
	}

	return head.Hash().String(), nil
}

func (p *goGitPublisher) PathsUpToDate(ctx context.Context, dir string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return true, nil
	}

	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return false, err
	}

	wt, err := repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("%w: open worktree", ErrDeliveryFailed)
	}

	status, err := wt.Status()
	if err != nil {
		return false, fmt.Errorf("%w: read worktree status", ErrDeliveryFailed)
	}

	for _, path := range paths {
		// Note: Status.File(...) would INSERT a default Untracked entry for an
		// absent path, so a tracked-and-clean file (absent from the map) would
		// look perpetually dirty. Read the map directly instead.
		entry, present := status[path]
		if !present {
			// Nothing pending (tracked and clean, or absent): the exact
			// change-set paths are already the committed content.
			continue
		}
		if entry.Staging != gogit.Unmodified || entry.Worktree != gogit.Unmodified {
			return false, nil
		}
	}

	return true, nil
}

func (p *goGitPublisher) CommitChanges(ctx context.Context, dir string, paths []string, message string, author Signature) (string, error) {
	if len(paths) == 0 {
		return "", ErrDeliveryFailed
	}

	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return "", err
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("%w: open worktree", ErrDeliveryFailed)
	}

	for _, rel := range paths {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			if os.IsNotExist(err) {
				// Deletion: stage the removal explicitly.
				if _, err := wt.Remove(rel); err != nil {
					return "", fmt.Errorf("%w: stage deletion", ErrDeliveryFailed)
				}
				continue
			}
			return "", fmt.Errorf("%w: stat change", ErrDeliveryFailed)
		}

		if _, err := wt.Add(rel); err != nil {
			return "", fmt.Errorf("%w: stage change", ErrDeliveryFailed)
		}
	}

	sig := &object.Signature{
		Name:  author.Name,
		Email: author.Email,
		When:  time.Now(),
	}

	hash, err := wt.Commit(message, &gogit.CommitOptions{
		Author:    sig,
		Committer: sig,
	})
	if err != nil {
		return "", fmt.Errorf("%w: commit change set", ErrDeliveryFailed)
	}

	return hash.String(), nil
}

func (p *goGitPublisher) RemoteBranchExists(ctx context.Context, dir, remote, branch string) (bool, string, error) {
	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return false, "", err
	}

	rem, err := repo.Remote(remote)
	if err != nil {
		return false, "", fmt.Errorf("%w: resolve remote", ErrDeliveryFailed)
	}

	ctx, cancel := context.WithTimeout(ctx, defaultDeliveryTimeout)
	defer cancel()

	refs, err := rem.List(&gogit.ListOptions{})
	if err != nil {
		return false, "", fmt.Errorf("%w: list remote refs", ErrDeliveryFailed)
	}

	target := plumbing.NewBranchReferenceName(branch)
	for _, ref := range refs {
		if ref.Name() == target {
			return true, ref.Hash().String(), nil
		}
	}

	return false, "", nil
}

func (p *goGitPublisher) PushBranch(ctx context.Context, dir, remote, branch string, auth pushAuth) error {
	repo, err := openWorkspaceRepo(dir)
	if err != nil {
		return err
	}

	var authMethod transport.AuthMethod
	if auth != nil {
		authMethod = auth.AuthMethod()
	}

	refSpec := config.RefSpec(
		plumbing.NewBranchReferenceName(branch).String() + ":" + plumbing.NewBranchReferenceName(branch).String(),
	)

	err = repo.PushContext(ctx, &gogit.PushOptions{
		RemoteName: remote,
		RefSpecs:   []config.RefSpec{refSpec},
		Auth:       authMethod,
	})
	if err != nil {
		if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return nil
		}
		return fmt.Errorf("%w: push delivery branch", ErrDeliveryFailed)
	}

	return nil
}

// deliveryBranchName builds the deterministic, git/GitHub-safe branch name
// for an issue: aevor/issue-<N>-<slugified title>. The result is stable for
// a given issue, which makes re-publication idempotent (and a regenerated
// change set reusing a delivered branch surfaces a conflict instead of an
// overwrite).
func deliveryBranchName(issueNumber int, issueTitle string) string {
	const maxSlugLength = 40

	slug := slugifyBranchComponent(issueTitle, maxSlugLength)
	if slug == "" {
		slug = "fix"
	}

	return fmt.Sprintf("aevor/issue-%d-%s", issueNumber, slug)
}

// slugifyBranchComponent lowercases a string, collapses every non [a-z0-9-]
// run to a dash, trims leading/trailing dashes, and bounds the length. The
// result is safe as a git ref name and a GitHub branch name.
func slugifyBranchComponent(input string, maxLength int) string {
	lower := strings.ToLower(strings.TrimSpace(input))

	var b strings.Builder
	prevDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxLength {
		if cut := strings.LastIndexByte(slug[:maxLength+1], '-'); cut > 0 {
			slug = slug[:cut]
		} else {
			slug = slug[:maxLength]
		}
		slug = strings.TrimRight(slug, "-")
	}

	return slug
}

// deliveryCommitMessage derives the commit/PR title from the change set
// summary, falling back to the issue title. Meaningless filler summaries are
// rejected so generated commits never look like "update files".
func deliveryCommitMessage(summary, issueTitle string) string {
	trimmed := strings.TrimSpace(summary)
	if !deliveryFillerMessage(trimmed) && len(trimmed) >= 5 {
		return truncateMessage(trimmed)
	}

	return truncateMessage(fallbackIssueTitle(issueTitle))
}

// deliveryFillerMessage reports whether a summary is an uninformative
// fill-in (update files, fix, …) that must not be used as a commit or PR
// title.
func deliveryFillerMessage(summary string) bool {
	lower := strings.ToLower(strings.TrimSpace(summary))

	banned := []string{
		"update files",
		"update file",
		"updated files",
		"updates files",
		"fix changes",
		"ai changes",
		"generated changes",
		"apply changes",
		"apply generated changes",
		"changes",
		"update",
		"updates",
		"updated",
		"fix",
		"fixes",
		"patch",
		"patches",
		".",
		"",
	}

	for _, bannedWord := range banned {
		if bannedWord == lower {
			return true
		}
	}

	return false
}

// fallbackIssueTitle returns a cleaned issue title, or a neutral subject that
// still describes the action when the title is empty or unusable.
func fallbackIssueTitle(title string) string {
	clean := strings.TrimSpace(title)
	if clean == "" || deliveryFillerMessage(clean) {
		return "fix issue"
	}
	return clean
}

func truncateMessage(message string) string {
	const maxMessageLength = 72

	runes := []rune(strings.TrimSpace(message))
	if len(runes) <= maxMessageLength {
		return string(runes)
	}

	truncated := string(runes[:maxMessageLength])
	if idx := strings.LastIndexByte(truncated, ' '); idx > 0 {
		truncated = truncated[:idx]
	}
	return strings.TrimRight(truncated, " .-")
}

// deliveryAuthor builds the deterministic commit identity for a user: a
// tool identity and the GitHub noreply address, so commits are attributed
// without exposing a personal email.
func deliveryAuthor(username string) Signature {
	return Signature{
		Name:  "Aevor",
		Email: fmt.Sprintf("%s@users.noreply.github.com", strings.ToLower(strings.TrimSpace(username))),
	}
}

// deliveryPRBody renders the pull request description from persisted change
// set state only: the issue, the changed files with their rationale, and the
// validation result. Nothing is inferred or embellished.
func deliveryPRBody(
	issueNumber int,
	issueTitle string,
	files []RepositoryChangeSetFile,
	validation *RepositoryChangeSetValidation,
	checks []RepositoryValidationCheck,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Resolves #%d\n\n", issueNumber)

	if title := fallbackIssueTitle(issueTitle); title != "" {
		fmt.Fprintf(&b, "## Problem\n%s\n\n", title)
	}

	fmt.Fprintf(&b, "## What changed\n")
	for _, file := range files {
		fmt.Fprintf(&b, "- `%s` (%s", file.FilePath, file.Operation)
		if file.Kind == changeFileKindTest {
			b.WriteString(", test")
		}
		if rationale := strings.TrimSpace(file.Rationale); rationale != "" {
			fmt.Fprintf(&b, ": %s", rationale)
		}
		b.WriteByte(')')
		b.WriteByte('\n')
	}

	if validation != nil {
		fmt.Fprintf(&b, "\n## Validation\nStatus: %s\n\n", validation.Status)
		if strings.TrimSpace(validation.Summary) != "" {
			fmt.Fprintf(&b, "Summary: %s\n\n", strings.TrimSpace(validation.Summary))
		}

		for _, check := range checks {
			icon := "-"
			switch check.Status {
			case validationStatusPass:
				icon = "PASS"
			case validationStatusFail:
				icon = "FAIL"
			case validationStatusNotAvailable:
				icon = "SKIP"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", icon, check.Name)
		}
	}

	return b.String()
}

// ChangeSetDeliveryState is the service-side view of a delivery, attached to
// ChangeSetState and converted to the API response.
type ChangeSetDeliveryState struct {
	Status            string     `json:"status"`
	BranchName        string     `json:"branch_name"`
	BaseBranch        string     `json:"base_branch"`
	CommitSHA         string     `json:"commit_sha"`
	PullRequestNumber int        `json:"pull_request_number"`
	PullRequestTitle  string     `json:"pull_request_title"`
	PullRequestURL    string     `json:"pull_request_url"`
	PullRequestState  string     `json:"pull_request_state"`
	DeliveredAt       *time.Time `json:"delivered_at"`
}

func deliveryStateFromRecord(record *RepositoryChangeSetDelivery) *ChangeSetDeliveryState {
	if record == nil {
		return nil
	}

	return &ChangeSetDeliveryState{
		Status:            record.Status,
		BranchName:        record.BranchName,
		BaseBranch:        record.BaseBranch,
		CommitSHA:         record.CommitSHA,
		PullRequestNumber: record.PullRequestNumber,
		PullRequestTitle:  record.PullRequestTitle,
		PullRequestURL:    record.PullRequestURL,
		PullRequestState:  record.PullRequestState,
		DeliveredAt:       record.DeliveredAt,
	}
}

// deliverySensitiveFilePatterns are matched against repository-relative
// paths to flag likely secrets before any delivery.
var (
	deliverySensitiveFilePatterns = []struct {
		name  string
		regex *regexp.Regexp
	}{
		{name: "env file", regex: regexp.MustCompile(`(?i)(^|/)\.env(\.local)?$`)},
		{name: "pem private key", regex: regexp.MustCompile(`(?i)\.pem$`)},
		{name: "pkcs key", regex: regexp.MustCompile(`(?i)\.(p12|pfx|p8|pkcs8)$`)},
		{name: "ssh key", regex: regexp.MustCompile(`(?i)(^|/)(id_rsa|id_ed25519|id_ecdsa)(\.pub)?$`)},
		{name: "netrc", regex: regexp.MustCompile(`(?i)(^|/)\.netrc$`)},
		{name: "npmrc", regex: regexp.MustCompile(`(?i)(^|/)\.npmrc$`)},
		{name: "credentials", regex: regexp.MustCompile(`(?i)(^|/)credentials[\w.-]*\.(json|yaml|yml)$`)},
		{name: "service account", regex: regexp.MustCompile(`(?i)(^|/)[\w.-]*service-account[\w.-]*\.(json|yaml|yml)$`)},
		{name: "secret manifest", regex: regexp.MustCompile(`(?i)(^|/)(secret|secrets|\.kube)[\w.-]*\.(yaml|yml)$`)},
	}

	deliverySensitiveContentPatterns = []struct {
		name  string
		regex *regexp.Regexp
	}{
		{name: "private key block", regex: regexp.MustCompile(`(?m)-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)},
		{name: "aws access key", regex: regexp.MustCompile(`(?i)\bAKIA[0-9A-Z]{16}\b`)},
		{name: "aws secret", regex: regexp.MustCompile(`(?i)"?aws_secret_access_key"?\s*[:=]\s*"`)},
		{name: "github token", regex: regexp.MustCompile(`\b(ghp|ghs|gho|ghu|ghr)_[A-Za-z0-9]{36}\b`)},
		{name: "github fine-grained token", regex: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
		{name: "openai key", regex: regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
		{name: "slack token", regex: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
		{name: "google api key", regex: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
		{name: "jwt bearer", regex: regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)?bearer\s+ey[Ja-z][A-Za-z0-9._-]{20,}`)},
		{name: "generic password", regex: regexp.MustCompile(`(?i)(password|passwd|pwd|secret|api[_-]?key|token)\s*[:=]\s*[^\s"']{8,}`)},
	}
)

// maxDeliverySecretScanBytes bounds how much of a proposed file is scanned so
// a hostile oversized file cannot make a publish scan unbounded.
const maxDeliverySecretScanBytes = 256 << 10

// deliverySecretFinding describes one likely secret, limited to its file and
// category — never its content.
type deliverySecretFinding struct {
	FilePath string
	Category string
}

// scanChangeSetForSecrets inspects every file the change set would deliver
// (path name and proposed content). The first finding is enough to refuse
// delivery; findings never include the secret text itself.
func scanChangeSetForSecrets(files []RepositoryChangeSetFile) *deliverySecretFinding {
	for _, file := range files {
		for _, pattern := range deliverySensitiveFilePatterns {
			if pattern.regex.MatchString(file.FilePath) {
				return &deliverySecretFinding{FilePath: file.FilePath, Category: pattern.name}
			}
		}

		content := []byte(file.ProposedContent)
		if len(content) > maxDeliverySecretScanBytes {
			content = content[:maxDeliverySecretScanBytes]
		}

		for _, pattern := range deliverySensitiveContentPatterns {
			if pattern.regex.Match(content) {
				return &deliverySecretFinding{FilePath: file.FilePath, Category: pattern.name}
			}
		}
	}

	return nil
}

// secretsDetectedAt returns the offending path, or "" when the change set
// contains no likely secrets.
func secretsDetectedAt(files []RepositoryChangeSetFile) string {
	finding := scanChangeSetForSecrets(files)
	if finding == nil {
		return ""
	}
	return finding.FilePath
}

// appliedContentMatches verifies the workspace still holds exactly the
// change set's proposed content (and the absence of every deletion). Any
// drift rejects delivery so a commit never captures work the owner did after
// the change set was applied.
func appliedContentMatches(workspaceDir string, files []RepositoryChangeSetFile) bool {
	for _, file := range files {
		content, exists, err := readWorkspaceFile(workspaceDir, file.FilePath)
		if err != nil {
			return false
		}

		switch file.Operation {
		case "delete":
			if exists {
				return false
			}
		case "add", "modify":
			if !exists || !bytes.Equal([]byte(content), []byte(file.ProposedContent)) {
				return false
			}
		default:
			return false
		}
	}

	return true
}
