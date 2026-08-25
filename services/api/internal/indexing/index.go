// Package indexing provides deterministic, repository-scoped lookup over the
// metadata produced by Task 3f representations. It is deliberately an
// in-memory metadata index: it stores no source content, performs no semantic
// matching, and has no filesystem or database access.
package indexing

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/filtering"
	"github.com/Aevor/platform/services/api/internal/representation"
)

var (
	// ErrInvalidRepositoryID rejects missing repository identity. Authorization
	// remains the responsibility of the service boundary; this package only
	// guarantees that every indexed record is bound to one supplied identity.
	ErrInvalidRepositoryID = errors.New("invalid repository id")

	// ErrRepositoryMismatch rejects representations from more than one
	// repository in a replacement snapshot.
	ErrRepositoryMismatch = errors.New("representation repository mismatch")

	// ErrInvalidRepresentation rejects metadata that cannot safely be indexed.
	ErrInvalidRepresentation = errors.New("invalid representation")

	// ErrDuplicateRepresentation rejects ambiguous content identities rather
	// than allowing input order to decide which metadata wins.
	ErrDuplicateRepresentation = errors.New("duplicate representation")

	// ErrRepositoryLimit and ErrEntryLimit bound in-memory retention.
	ErrRepositoryLimit = errors.New("repository index limit exceeded")
	ErrEntryLimit      = errors.New("index entry limit exceeded")
)

const (
	// DefaultMaxRepositories bounds independent repository snapshots retained
	// by an Index. Callers that need a different retained working set may pass
	// explicit Options; no unbounded default is permitted.
	DefaultMaxRepositories = 8

	// DefaultMaxEntriesPerRepository reuses Task 3e's total chunk ceiling: one
	// representation is derived from one chunk, so this layer cannot amplify a
	// single upstream result.
	DefaultMaxEntriesPerRepository = chunking.DefaultMaxChunksTotal

	// DefaultMaxEntriesTotal bounds aggregate in-memory metadata. It permits
	// several complete upstream results while preventing retained indexes from
	// growing with every repository a process happens to serve.
	DefaultMaxEntriesTotal = 64000
)

// Options bounds retained index metadata. Zero values use the safe defaults.
type Options struct {
	MaxRepositories         int
	MaxEntriesPerRepository int
	MaxEntriesTotal         int
}

func (o Options) maxRepositories() int {
	if o.MaxRepositories > 0 {
		return o.MaxRepositories
	}

	return DefaultMaxRepositories
}

func (o Options) maxEntriesPerRepository() int {
	if o.MaxEntriesPerRepository > 0 {
		return o.MaxEntriesPerRepository
	}

	return DefaultMaxEntriesPerRepository
}

func (o Options) maxEntriesTotal() int {
	if o.MaxEntriesTotal > 0 {
		return o.MaxEntriesTotal
	}

	return DefaultMaxEntriesTotal
}

// Record is the metadata-only indexed view of a representation. Content is
// intentionally absent: callers can use this record to locate provenance,
// while source content stays in the upstream, bounded representation flow.
type Record struct {
	ID              string
	RepositoryID    string
	FilePath        string
	Directory       string
	Extension       string
	FileSize        int64
	FileRole        string
	Language        string
	ChunkIndex      int
	StartLine       int
	EndLine         int
	ByteSize        int64
	ContentHash     string
	SymbolName      *string
	SymbolType      string
	ParentSymbol    *string
	PrevChunkIndex  *int
	NextChunkIndex  *int
	SourceUnderTest string
}

// Query restricts retrieval to one repository. Empty optional fields mean
// "do not filter by this dimension". Prefix matching is lexical over
// repository-relative slash paths only.
type Query struct {
	RepositoryID string
	FilePath     string
	PathPrefix   string
	Language     string
	FileRole     string
	SymbolName   string
	SymbolType   string
	ChunkIndex   *int
	ContentHash  string
}

// Index holds bounded repository snapshots. Replace is atomic: readers see
// either the preceding complete snapshot or the next complete snapshot, never
// an intermediate mix. It is safe for concurrent readers and replacements.
type Index struct {
	mu      sync.RWMutex
	options Options
	repos   map[string]*repositorySnapshot
}

type repositorySnapshot struct {
	records       map[string]Record
	allIDs        []string
	byFile        map[string][]string
	byLanguage    map[string][]string
	byRole        map[string][]string
	bySymbolName  map[string][]string
	bySymbolType  map[string][]string
	byContentHash map[string][]string
	byChunkIndex  map[int][]string
}

// New creates an empty, bounded index.
func New(options Options) *Index {
	return &Index{
		options: options,
		repos:   make(map[string]*repositorySnapshot),
	}
}

// Replace deterministically replaces one repository's entire indexed
// snapshot. Consequently unchanged records retain their established IDs,
// changed records receive their new Task 3f identity, deleted records vanish,
// and newly present records appear exactly once. An empty slice safely records
// an empty repository snapshot.
func (i *Index) Replace(repositoryID string, representations []representation.Representation) error {
	if !validRepositoryID(repositoryID) {
		return ErrInvalidRepositoryID
	}

	if len(representations) > i.options.maxEntriesPerRepository() {
		return ErrEntryLimit
	}

	snapshot, err := buildSnapshot(repositoryID, representations)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	previous, exists := i.repos[repositoryID]
	if !exists && len(i.repos) >= i.options.maxRepositories() {
		return ErrRepositoryLimit
	}

	entries := len(snapshot.records)
	if exists {
		entries += i.entryCountLocked() - len(previous.records)
	} else {
		entries += i.entryCountLocked()
	}

	if entries > i.options.maxEntriesTotal() {
		return ErrEntryLimit
	}

	i.repos[repositoryID] = snapshot

	return nil
}

// Remove deletes one complete repository snapshot. It is idempotent and does
// not reveal whether a repository had previously been indexed.
func (i *Index) Remove(repositoryID string) {
	if !validRepositoryID(repositoryID) {
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.repos, repositoryID)
}

// Files returns the indexed repository-relative files in lexical order.
func (i *Index) Files(repositoryID string) []string {
	if !validRepositoryID(repositoryID) {
		return []string{}
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	snapshot := i.repos[repositoryID]
	if snapshot == nil {
		return []string{}
	}

	files := make([]string, 0, len(snapshot.byFile))
	for filePath := range snapshot.byFile {
		files = append(files, filePath)
	}
	sort.Strings(files)

	return files
}

// Lookup returns metadata-only records satisfying every populated query
// dimension. Records are ordered by file path, chunk index, then stable ID.
func (i *Index) Lookup(query Query) []Record {
	if !validRepositoryID(query.RepositoryID) {
		return []Record{}
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	snapshot := i.repos[query.RepositoryID]
	if snapshot == nil {
		return []Record{}
	}

	ids := snapshot.candidates(query)
	records := make([]Record, 0, len(ids))

	for _, id := range ids {
		record := snapshot.records[id]
		if matches(record, query) {
			records = append(records, copyRecord(record))
		}
	}

	sortRecords(records)

	return records
}

func (i *Index) entryCountLocked() int {
	total := 0
	for _, snapshot := range i.repos {
		total += len(snapshot.records)
	}

	return total
}

func buildSnapshot(repositoryID string, representations []representation.Representation) (*repositorySnapshot, error) {
	snapshot := &repositorySnapshot{
		records:       make(map[string]Record, len(representations)),
		byFile:        make(map[string][]string),
		byLanguage:    make(map[string][]string),
		byRole:        make(map[string][]string),
		bySymbolName:  make(map[string][]string),
		bySymbolType:  make(map[string][]string),
		byContentHash: make(map[string][]string),
		byChunkIndex:  make(map[int][]string),
	}

	for _, item := range representations {
		if item.RepositoryID != repositoryID {
			return nil, ErrRepositoryMismatch
		}

		if !validRepresentation(item) {
			return nil, ErrInvalidRepresentation
		}

		if _, exists := snapshot.records[item.ID]; exists {
			return nil, ErrDuplicateRepresentation
		}

		record := recordOf(item)
		snapshot.records[record.ID] = record
		snapshot.allIDs = append(snapshot.allIDs, record.ID)
		snapshot.byFile[record.FilePath] = append(snapshot.byFile[record.FilePath], record.ID)
		snapshot.byLanguage[record.Language] = append(snapshot.byLanguage[record.Language], record.ID)
		snapshot.byRole[record.FileRole] = append(snapshot.byRole[record.FileRole], record.ID)
		snapshot.bySymbolType[record.SymbolType] = append(snapshot.bySymbolType[record.SymbolType], record.ID)
		snapshot.byContentHash[record.ContentHash] = append(snapshot.byContentHash[record.ContentHash], record.ID)
		snapshot.byChunkIndex[record.ChunkIndex] = append(snapshot.byChunkIndex[record.ChunkIndex], record.ID)

		if record.SymbolName != nil {
			snapshot.bySymbolName[*record.SymbolName] = append(snapshot.bySymbolName[*record.SymbolName], record.ID)
		}
	}

	sort.Strings(snapshot.allIDs)
	for _, ids := range snapshot.byFile {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.byLanguage {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.byRole {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.bySymbolName {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.bySymbolType {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.byContentHash {
		sort.Strings(ids)
	}
	for _, ids := range snapshot.byChunkIndex {
		sort.Strings(ids)
	}

	return snapshot, nil
}

func (s *repositorySnapshot) candidates(query Query) []string {
	switch {
	case query.FilePath != "":
		return s.byFile[query.FilePath]
	case query.Language != "":
		return s.byLanguage[query.Language]
	case query.FileRole != "":
		return s.byRole[query.FileRole]
	case query.SymbolName != "":
		return s.bySymbolName[query.SymbolName]
	case query.SymbolType != "":
		return s.bySymbolType[query.SymbolType]
	case query.ContentHash != "":
		return s.byContentHash[query.ContentHash]
	case query.ChunkIndex != nil:
		return s.byChunkIndex[*query.ChunkIndex]
	default:
		return s.allIDs
	}
}

func matches(record Record, query Query) bool {
	if query.FilePath != "" && record.FilePath != query.FilePath {
		return false
	}
	if query.PathPrefix != "" && !strings.HasPrefix(record.FilePath, query.PathPrefix) {
		return false
	}
	if query.Language != "" && record.Language != query.Language {
		return false
	}
	if query.FileRole != "" && record.FileRole != query.FileRole {
		return false
	}
	if query.SymbolName != "" && (record.SymbolName == nil || *record.SymbolName != query.SymbolName) {
		return false
	}
	if query.SymbolType != "" && record.SymbolType != query.SymbolType {
		return false
	}
	if query.ChunkIndex != nil && record.ChunkIndex != *query.ChunkIndex {
		return false
	}

	return query.ContentHash == "" || record.ContentHash == query.ContentHash
}

func validRepositoryID(repositoryID string) bool {
	return strings.TrimSpace(repositoryID) != "" && !strings.ContainsAny(repositoryID, "\x00\r\n")
}

func validRepresentation(item representation.Representation) bool {
	return item.ID != "" && item.ContentHash != "" && item.FileRole != "" &&
		item.SymbolType != "" && item.ChunkIndex >= 0 && item.StartLine > 0 &&
		item.EndLine >= item.StartLine && filtering.ValidRelativePath(item.FilePath)
}

func recordOf(item representation.Representation) Record {
	return Record{
		ID:              item.ID,
		RepositoryID:    item.RepositoryID,
		FilePath:        item.FilePath,
		Directory:       item.Directory,
		Extension:       item.Extension,
		FileSize:        item.FileSize,
		FileRole:        item.FileRole,
		Language:        item.Language,
		ChunkIndex:      item.ChunkIndex,
		StartLine:       item.StartLine,
		EndLine:         item.EndLine,
		ByteSize:        item.ByteSize,
		ContentHash:     item.ContentHash,
		SymbolName:      copyString(item.SymbolName),
		SymbolType:      item.SymbolType,
		ParentSymbol:    copyString(item.ParentSymbol),
		PrevChunkIndex:  copyInt(item.PrevChunkIndex),
		NextChunkIndex:  copyInt(item.NextChunkIndex),
		SourceUnderTest: item.SourceUnderTest,
	}
}

func copyRecord(record Record) Record {
	record.SymbolName = copyString(record.SymbolName)
	record.ParentSymbol = copyString(record.ParentSymbol)
	record.PrevChunkIndex = copyInt(record.PrevChunkIndex)
	record.NextChunkIndex = copyInt(record.NextChunkIndex)

	return record
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}

	copy := *value
	return &copy
}

func copyInt(value *int) *int {
	if value == nil {
		return nil
	}

	copy := *value
	return &copy
}

func sortRecords(records []Record) {
	sort.Slice(records, func(left int, right int) bool {
		if records[left].FilePath != records[right].FilePath {
			return records[left].FilePath < records[right].FilePath
		}
		if records[left].ChunkIndex != records[right].ChunkIndex {
			return records[left].ChunkIndex < records[right].ChunkIndex
		}

		return records[left].ID < records[right].ID
	})
}
