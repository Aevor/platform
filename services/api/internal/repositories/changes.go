package repositories

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aevor/platform/services/api/internal/ai"
)

// The operations an AI-generated change set may carry. Anything else is
// rejected before any file is touched.
var allowedChangeOperations = map[string]bool{
	"modify": true,
	"add":    true,
	"delete": true,
}

// GeneratedChange is one validated change applied against an isolated
// in-memory snapshot of the repository workspace, together with the ACTUAL
// diff computed between the original file state and the proposed state.
type GeneratedChange struct {
	FilePath        string `json:"file_path"`
	Operation       string `json:"operation"`
	Symbol          string `json:"symbol"`
	Rationale       string `json:"rationale"`
	Diff            string `json:"diff"`
	ProposedContent string `json:"proposed_content"`
}

// ChangeApplication is the result of validating and applying a structured AI
// change set in memory. The original workspace is never modified: diffs are
// computed between the on-disk original and the proposed content, and nothing
// is written back.
type ChangeApplication struct {
	Changes []GeneratedChange `json:"changes"`
}

// applyGeneratedChanges validates every file operation of an AI change set
// against the given workspace directory and applies it IN MEMORY only. On any
// unsafe or ambiguous change the WHOLE set is rejected and no partial result
// is returned: a rejected set must never leave a half-applied state behind.
//
// Security contract:
//   - operation must be modify/add/delete
//   - file paths must be repository-relative, clean, and stay inside the
//     workspace (no absolute paths, no ".." traversal, no Windows volumes, no
//     symlinks on the target or any parent component)
//   - modify requires the file to exist and, when an original context was
//     supplied, the context must match the workspace state
//   - add requires the file to NOT exist
//   - delete requires the file to exist (and is preserved for review)
func applyGeneratedChanges(
	workspaceDir string,
	changeSet *ai.GenerateChangesResponse,
) (*ChangeApplication, error) {
	fileChanges := make([]ai.FileChange, 0, len(changeSet.Changes)+len(changeSet.Tests))
	fileChanges = append(fileChanges, changeSet.Changes...)
	for _, test := range changeSet.Tests {
		fileChanges = append(fileChanges, ai.FileChange{
			FilePath:        test.FilePath,
			Operation:       test.Operation,
			Symbol:          "test",
			Rationale:       test.Rationale,
			ProposedContent: test.ProposedContent,
		})
	}

	if len(fileChanges) == 0 {
		return nil, fmt.Errorf("change set contains no file changes")
	}

	seen := make(map[string]string, len(fileChanges))
	for _, change := range fileChanges {
		operation := strings.ToLower(strings.TrimSpace(change.Operation))
		if !allowedChangeOperations[operation] {
			return nil, fmt.Errorf("unsupported operation %q for %s", change.Operation, change.FilePath)
		}
		if err := validateRepoRelativePath(change.FilePath); err != nil {
			return nil, fmt.Errorf("invalid file path %q: %w", change.FilePath, err)
		}
		if prior, ok := seen[change.FilePath]; ok && prior != operation {
			return nil, fmt.Errorf("conflicting operations %q and %q for %s", prior, operation, change.FilePath)
		}
		seen[change.FilePath] = operation
	}

	applied := &ChangeApplication{Changes: make([]GeneratedChange, 0, len(fileChanges))}

	for _, change := range fileChanges {
		operation := strings.ToLower(strings.TrimSpace(change.Operation))

		original, exists, err := readWorkspaceFile(workspaceDir, change.FilePath)
		if err != nil {
			return nil, err
		}

		var diff string

		switch operation {
		case "modify":
			if !exists {
				return nil, fmt.Errorf("cannot modify %s: file does not exist in workspace", change.FilePath)
			}
			if strings.TrimSpace(change.OriginalContext) != "" &&
				!strings.Contains(original, change.OriginalContext) {
				return nil, fmt.Errorf("cannot modify %s: original context does not match workspace state", change.FilePath)
			}
			if change.ProposedContent == original {
				return nil, fmt.Errorf("change for %s produces no diff", change.FilePath)
			}
			diff = generateUnifiedDiff(change.FilePath, original, change.ProposedContent)

		case "add":
			if exists {
				return nil, fmt.Errorf("cannot add %s: file already exists in workspace", change.FilePath)
			}
			if strings.TrimSpace(change.ProposedContent) == "" {
				return nil, fmt.Errorf("change for %s produces no diff", change.FilePath)
			}
			diff = generateUnifiedDiff(change.FilePath, "", change.ProposedContent)

		case "delete":
			if !exists {
				return nil, fmt.Errorf("cannot delete %s: file does not exist in workspace", change.FilePath)
			}
			if strings.TrimSpace(original) == "" {
				return nil, fmt.Errorf("change for %s produces no diff", change.FilePath)
			}
			diff = generateUnifiedDiff(change.FilePath, original, "")
		}

		if strings.TrimSpace(diff) == "" {
			return nil, fmt.Errorf("change for %s produces no diff", change.FilePath)
		}

		applied.Changes = append(applied.Changes, GeneratedChange{
			FilePath:        change.FilePath,
			Operation:       operation,
			Symbol:          change.Symbol,
			Rationale:       change.Rationale,
			Diff:            diff,
			ProposedContent: change.ProposedContent,
		})
	}

	return applied, nil
}

// validateRepoRelativePath enforces the path rules for change targets. An AI
// output is never trusted, so rules are strict and the input is rejected
// outright on any ambiguity. Paths are expected to use forward slashes /
// POSIX-style relative segments.
func validateRepoRelativePath(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(raw, '\x00') {
		return fmt.Errorf("contains a null byte")
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "~") {
		return fmt.Errorf("home-expansion path not allowed")
	}
	if strings.Contains(raw, "\\") {
		return fmt.Errorf("Windows-style separators not allowed")
	}
	if strings.Contains(raw, ":") {
		return fmt.Errorf("path carries a filesystem volume")
	}

	path := filepath.Clean(filepath.FromSlash(raw))

	if filepath.IsAbs(path) {
		return fmt.Errorf("absolute path not allowed")
	}
	if filepath.VolumeName(path) != "" {
		return fmt.Errorf("path carries a filesystem volume")
	}
	if path == "." || path == ".." {
		return fmt.Errorf("path is not a file")
	}

	for _, segment := range strings.Split(path, string(filepath.Separator)) {
		if segment == ".." {
			return fmt.Errorf("path traverses outside the repository")
		}
	}

	if strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path traverses outside the repository")
	}

	return nil
}

// readWorkspaceFile reads one repository-relative file from the workspace.
// It re-checks containment and rejects symlinks on the target and on every
// parent component, so an AI-supplied path can never be redirected outside
// the workspace even if the clone itself contains a malicious link. Returns
// exists=false when the path does not exist.
func readWorkspaceFile(workspaceDir, rel string) (string, bool, error) {
	full := filepath.Join(workspaceDir, filepath.FromSlash(rel))

	if !pathContainedIn(workspaceDir, full) {
		return "", false, fmt.Errorf("path %q escapes the workspace", rel)
	}

	// Reject a symlink on the target itself.
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("could not stat %q: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("path %q is a symlink", rel)
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("path %q is a directory", rel)
	}

	// Reject any parent component that is a symlink.
	parent := filepath.Dir(full)
	for pathContainedIn(workspaceDir, parent) && parent != workspaceDir {
		parentInfo, err := os.Lstat(parent)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return "", false, fmt.Errorf("could not stat %q: %w", rel, err)
		}
		if parentInfo.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("path %q traverses a symlink", rel)
		}
		parent = filepath.Dir(parent)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		return "", false, fmt.Errorf("could not read %q: %w", rel, err)
	}

	return string(data), true, nil
}

func pathContainedIn(root, path string) bool {
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	return cleanPath == cleanRoot ||
		strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Line-level unified diff generation (no external dependency).
// ---------------------------------------------------------------------------

type diffOp int

const (
	opKeep diffOp = iota
	opDelete
	opInsert
)

// splitLines splits content into lines, always returning at least one line.
func splitLines(content string) []string {
	if content == "" {
		return []string{}
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// computeDiffOps returns the operation sequence between two line slices using
// a length-capped LCS. For very large inputs it degrades to a whole-file
// replacement (delete-all / insert-all), which is a valid unified diff.
func computeDiffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n*m > 2_500_000 {
		ops := make([]diffOp, 0, n+m)
		for range a {
			ops = append(ops, opDelete)
		}
		for range b {
			ops = append(ops, opInsert)
		}
		return ops
	}

	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, opKeep)
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, opDelete)
			i++
		default:
			ops = append(ops, opInsert)
			j++
		}
	}
	for i < n {
		ops = append(ops, opDelete)
		i++
	}
	for j < m {
		ops = append(ops, opInsert)
		j++
	}
	return ops
}

// generateUnifiedDiff produces a reviewable unified diff between the original
// and proposed content of one repository-relative file. Both the header
// (--- a/ / +++ b/) and the hunks are real: the diff is computed from the
// actual original workspace content vs the proposed content.
func generateUnifiedDiff(filePath, original, proposed string) string {
	a := splitLines(original)
	b := splitLines(proposed)

	ops := computeDiffOps(a, b)

	var body bytes.Buffer
	writeHunks(&body, a, b, ops)

	return fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", filePath, filePath, body.String())
}

const diffContextLines = 3

func writeHunks(buf *bytes.Buffer, a, b []string, ops []diffOp) {
	type span struct{ start, end int }
	spans := make([]span, 0, 8)

	for start := 0; start < len(ops); {
		if ops[start] == opKeep {
			start++
			continue
		}
		end := start
		for end < len(ops) && ops[end] != opKeep {
			end++
		}
		s := start - diffContextLines
		if s < 0 {
			s = 0
		}
		e := end + diffContextLines
		if e > len(ops) {
			e = len(ops)
		}
		if len(spans) > 0 && s <= spans[len(spans)-1].end {
			spans[len(spans)-1].end = e
		} else {
			spans = append(spans, span{start: s, end: e})
		}
		start = end
	}

	for _, sp := range spans {
		aLine, bLine := 0, 0
		for k := 0; k < sp.start; k++ {
			switch ops[k] {
			case opKeep, opDelete:
				aLine++
			}
			switch ops[k] {
			case opKeep, opInsert:
				bLine++
			}
		}

		aCount, bCount := 0, 0
		for _, op := range ops[sp.start:sp.end] {
			if op == opKeep || op == opDelete {
				aCount++
			}
			if op == opKeep || op == opInsert {
				bCount++
			}
		}

		fmt.Fprintf(buf, "@@ -%d,%d +%d,%d @@\n", aLine+1, aCount, bLine+1, bCount)

		for _, op := range ops[sp.start:sp.end] {
			switch op {
			case opKeep:
				buf.WriteString(" " + a[aLine] + "\n")
				aLine++
				bLine++
			case opDelete:
				buf.WriteString("-" + a[aLine] + "\n")
				aLine++
			case opInsert:
				buf.WriteString("+" + b[bLine] + "\n")
				bLine++
			}
		}
	}
}
