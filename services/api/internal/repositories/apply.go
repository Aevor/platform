package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// originalStateHash returns a deterministic digest of the ORIGINAL workspace
// content of the given repository-relative paths. It is computed at
// generation time and re-verified at approve and apply: if the workspace has
// drifted, the hash no longer matches and the set is refused with
// ErrWorkspaceChanged rather than silently overwriting newer work. Reading
// goes through readWorkspaceFile so symlink escapes and containment violations
// are re-checked on every verification.
func originalStateHash(workspaceDir string, paths []string) (string, error) {
	hash := sha256.New()

	for _, rel := range paths {
		content, exists, err := readWorkspaceFile(workspaceDir, rel)
		if err != nil {
			return "", err
		}

		io.WriteString(hash, rel)
		hash.Write([]byte{0})

		if exists {
			hash.Write([]byte{1})
			_, _ = hash.Write([]byte(content))
		} else {
			hash.Write([]byte{0})
		}

		hash.Write([]byte{0})
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ApplyChangeSet applies the EXACT approved change set to the live workspace:
// no AI reinterpretation, no approximation. The applied content is the same
// ProposedContent whose diff the user reviewed and approved. The workspace is
// locked for the duration, the original state hash is re-verified so work
// done since generation is never destroyed, and the write is all-or-nothing
// (any failure rolls back every already-written file). Apply is idempotent:
// re-applying an already-applied set returns the current state without
// touching the workspace again. No git operation is ever performed.
func (s *Service) ApplyChangeSet(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	issueID uuid.UUID,
	changeSetID uuid.UUID,
) (*ChangeSetState, error) {
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	unlock := s.workspaces.LockFor(selected.ID)
	defer unlock()

	ready, err := s.workspaces.Ready(selected.ID)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrWorkspaceNotReady
	}

	set, files, err := s.store.GetChangeSetByID(selected.ID, changeSetID)
	if err != nil {
		return nil, err
	}
	if set == nil || set.RepositoryIssueID != issueID {
		return nil, ErrChangeSetNotFound
	}

	switch set.Status {
	case changeSetStatusApplied, changeSetStatusValidated, changeSetStatusFailed:
		// Already applied (or beyond): nothing to re-run, the workspace keeps
		// the approved content. Return the current state idempotently.
		return s.stateWithValidation(selected.ID, issueID, set, files)
	case changeSetStatusGenerated:
		return nil, ErrChangeSetNotApproved
	}

	if set.Status != changeSetStatusApproved {
		// Validating is not a state apply may enter.
		return nil, ErrChangeSetInvalidState
	}

	dir := s.workspaces.Dir(selected.ID)

	hash, err := originalStateHash(dir, changeFilePaths(files))
	if err != nil {
		return nil, err
	}
	if hash != set.OriginalHash {
		return nil, ErrWorkspaceChanged
	}

	if err := applyChangeSetToWorkspace(dir, files); err != nil {
		return nil, err
	}

	transitioned, err := s.store.MarkChangeSetApplied(set.ID)
	if err != nil {
		return nil, err
	}
	if !transitioned {
		reload, reloaded, err := s.store.GetChangeSetByID(selected.ID, changeSetID)
		if err != nil {
			return nil, err
		}
		if reload == nil || len(reloaded) == 0 {
			return nil, ErrChangeSetNotFound
		}
		return s.stateWithValidation(selected.ID, issueID, reload, reloaded)
	}

	set.Status = changeSetStatusApplied
	now := time.Now()
	set.AppliedAt = &now

	return buildChangeSetState(selected.ID, issueID, set, files, nil), nil
}

// applyTarget is one planned file operation with everything needed to write
// it and restore it on rollback.
type applyTarget struct {
	rel       string
	full      string
	operation string
	existed   bool
	original  string
	proposed  string
}

// applyChangeSetToWorkspace physically applies a validated change set to the
// workspace in two phases:
//
//  1. PLAN every file operation and capture the original content — any unsafe
//     or ambiguous operation aborts here, BEFORE any file is touched.
//  2. WRITE each planned change; on ANY error, roll back every already
//     applied write (restoring original content or removing created files and
//     dirs), so a failed apply never leaves a partially modified workspace.
//
// Path security follows the same contract as generation: repository-relative
// paths only, no traversal, no volumes, no symlink targets or parents.
func applyChangeSetToWorkspace(workspaceDir string, files []RepositoryChangeSetFile) error {
	plans := make([]applyTarget, 0, len(files))
	seen := make(map[string]string, len(files))

	for _, file := range files {
		operation := normalizeChangeOperation(file.Operation)
		if !allowedChangeOperations[operation] {
			return fmt.Errorf("unsupported operation %q for %s", file.Operation, file.FilePath)
		}
		rel := file.FilePath
		if err := validateRepoRelativePath(rel); err != nil {
			return fmt.Errorf("invalid file path %q: %w", rel, err)
		}
		if prior, ok := seen[rel]; ok && prior != operation {
			return fmt.Errorf("conflicting operations %q and %q for %s", prior, operation, rel)
		}
		seen[rel] = operation

		original, exists, err := readWorkspaceFile(workspaceDir, rel)
		if err != nil {
			return err
		}

		switch operation {
		case "modify":
			if !exists {
				return fmt.Errorf("cannot modify %s: file does not exist in workspace", rel)
			}
			if file.ProposedContent == original {
				return fmt.Errorf("change for %s produces no diff", rel)
			}
		case "add":
			if exists {
				return fmt.Errorf("cannot add %s: file already exists in workspace", rel)
			}
			if len(file.ProposedContent) == 0 {
				return fmt.Errorf("change for %s produces no diff", rel)
			}
		case "delete":
			if !exists {
				return fmt.Errorf("cannot delete %s: file does not exist in workspace", rel)
			}
			if len(original) == 0 {
				return fmt.Errorf("change for %s produces no diff", rel)
			}
		}

		plans = append(plans, applyTarget{
			rel:       rel,
			full:      filepath.Join(workspaceDir, filepath.FromSlash(rel)),
			operation: operation,
			existed:   exists,
			original:  original,
			proposed:  file.ProposedContent,
		})
	}

	// Phase 2: write. Track every executed write and every created directory
	// so any failure can be rolled back completely.
	var applied []applyTarget
	var createdDirs []string

	fail := func(err error) error {
		for i := len(applied) - 1; i >= 0; i-- {
			target := applied[i]
			if target.existed {
				_ = os.WriteFile(target.full, []byte(target.original), 0o600)
			} else {
				_ = os.Remove(target.full)
			}
		}
		for i := len(createdDirs) - 1; i >= 0; i-- {
			// Only removes empty directories; shared dirs stay.
			_ = os.Remove(createdDirs[i])
		}
		return err
	}

	for _, target := range plans {
		// Register the target BEFORE writing so even a partially written file
		// is restored by the rollback.
		applied = append(applied, target)

		switch target.operation {
		case "modify", "add":
			dirs, err := writeWorkspaceFile(workspaceDir, target.rel, target.proposed)
			if err != nil {
				return fail(err)
			}
			createdDirs = append(createdDirs, dirs...)
		case "delete":
			if err := os.Remove(target.full); err != nil {
				return fail(err)
			}
		}
	}

	return nil
}

// writeWorkspaceFile writes one repository-relative file into the workspace
// with the same security contract as readWorkspaceFile: containment is
// re-verified, symlink targets and symlink parent components are rejected,
// and any missing parent directories are created. It returns the directories
// it created (deepest first) so a rollback can remove them.
func writeWorkspaceFile(workspaceDir, rel, content string) ([]string, error) {
	full := filepath.Join(workspaceDir, filepath.FromSlash(rel))

	if !pathContainedIn(workspaceDir, full) {
		return nil, fmt.Errorf("path %q escapes the workspace", rel)
	}

	// Reject a symlink on the target itself.
	if info, err := os.Lstat(full); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q is a symlink", rel)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("path %q is a directory", rel)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not stat %q: %w", rel, err)
	}

	// Reject any existing parent component that is a symlink.
	parent := filepath.Dir(full)
	for pathContainedIn(workspaceDir, parent) && parent != workspaceDir {
		info, err := os.Lstat(parent)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return nil, fmt.Errorf("could not stat %q: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q traverses a symlink", rel)
		}
		parent = filepath.Dir(parent)
	}

	created, err := ensureParentDirs(workspaceDir, full)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("could not write %q: %w", rel, err)
	}

	return created, nil
}

// ensureParentDirs creates any missing parent directories of target inside
// the workspace, returning the newly created directories deepest first (the
// safe removal order for rollback).
func ensureParentDirs(workspaceDir, target string) ([]string, error) {
	parent := filepath.Dir(target)
	if !pathContainedIn(workspaceDir, parent) {
		return nil, fmt.Errorf("path escapes the workspace")
	}
	if _, err := os.Lstat(parent); err == nil {
		return nil, nil
	}

	// Collect the missing chain, deepest first, stopping at the nearest
	// existing ancestor (the workspace root always exists).
	var missing []string
	for cur := parent; pathContainedIn(workspaceDir, cur); cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("could not stat %q: %w", cur, err)
		}
		missing = append(missing, cur)
	}

	// Create shallow-first, then return deepest-first for rollback.
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.MkdirAll(missing[i], 0o755); err != nil {
			return nil, fmt.Errorf("could not create directory %q: %w", missing[i], err)
		}
	}

	return missing, nil
}