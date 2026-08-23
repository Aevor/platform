package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
)

// DefaultCloneTimeout bounds every clone operation. A synchronous endpoint
// must never hang indefinitely on a huge repository.
const DefaultCloneTimeout = 5 * time.Minute

// Manager owns the controlled workspace root under which every repository
// workspace lives. Directory names are SERVER-GENERATED SelectedRepository
// UUIDs — never client-supplied paths or GitHub metadata — so path traversal
// is impossible by construction; Dir() additionally re-verifies containment
// as defense in depth. The Manager also serializes operations per workspace
// so two concurrent requests can never race on one directory.
type Manager struct {
	root string

	locks sync.Map // map[uuid.UUID]*sync.Mutex
}

// NewManager creates the controlled workspace root. The path must be
// non-empty; it is converted to an absolute form so all later containment
// checks compare like with like.
func NewManager(root string) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("workspace root must not be empty")
	}

	absolute, err := filepath.Abs(root)

	if err != nil {
		return nil, fmt.Errorf("could not resolve workspace root: %w", err)
	}

	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("could not create workspace root: %w", err)
	}

	return &Manager{
		root: absolute,
	}, nil
}

// Root returns the absolute workspace root (for tests and diagnostics only;
// it is never exposed through the API).
func (m *Manager) Root() string {
	return m.root
}

// Dir returns the workspace directory for one selected repository. The name
// is the server-generated UUID; containment within the root is re-checked.
func (m *Manager) Dir(id uuid.UUID) string {
	dir := filepath.Join(m.root, id.String())

	if dir != m.root && filepath.Dir(dir) != m.root {
		panic(fmt.Sprintf("workspace dir %q escaped root %q", dir, m.root))
	}

	return dir
}

// LockFor serializes operations on ONE workspace. The returned function
// releases the lock; callers must defer it immediately.
func (m *Manager) LockFor(id uuid.UUID) func() {
	entry, _ := m.locks.LoadOrStore(id, &sync.Mutex{})

	mutex := entry.(*sync.Mutex)

	mutex.Lock()

	var once atomic.Bool

	return func() {
		if once.CompareAndSwap(false, true) {
			mutex.Unlock()
		}
	}
}

// Ready reports whether the workspace holds a usable clone: the directory
// exists, contains a Git repository, and HEAD resolves to a commit.
func (m *Manager) Ready(id uuid.UUID) (bool, error) {
	dir := m.Dir(id)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, fmt.Errorf("could not inspect workspace: %w", err)
	}

	repository, err := git.PlainOpen(dir)

	if err != nil {
		return false, nil //nolint:nilerr // a broken directory is simply not ready
	}

	if _, err := repository.Head(); err != nil {
		return false, nil //nolint:nilerr // a repository without HEAD is simply not ready
	}

	return true, nil
}

// Reset discards whatever occupies the workspace (including a partially
// cloned, corrupted directory left behind by an earlier failure) and creates
// a fresh empty directory, returning its path.
func (m *Manager) Reset(id uuid.UUID) (string, error) {
	dir := m.Dir(id)

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("could not clear workspace: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not create workspace: %w", err)
	}

	return dir, nil
}

// Discard removes the workspace entirely after a failed operation, so no
// corrupted partial repository is ever left behind.
func (m *Manager) Discard(id uuid.UUID) {
	_ = os.RemoveAll(m.Dir(id))
}
