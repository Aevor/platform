package repositories

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/repoinfo"

	"github.com/google/uuid"
)

// GetIntelligence returns the persisted, revision-bound repository
// intelligence profile for one owned repository. Ownership is resolved first;
// a foreign or missing repository is indistinguishable. When the persisted
// revision differs from the current workspace HEAD revision, the profile is
// reported stale and is never silently presented as current. A repository with
// no profile yet yields ErrIntelligenceMissing.
func (s *Service) GetIntelligence(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
) (*RepositoryIntelligence, error) {
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	intelligence, err := s.store.GetIntelligence(selected.ID)
	if err != nil {
		return nil, err
	}
	if intelligence == nil {
		return nil, ErrIntelligenceMissing
	}

	s.applyStaleness(ctx, selected, intelligence)

	return intelligence, nil
}

// RefreshIntelligence regenerates and persists the repository intelligence
// profile for one owned repository against the current HEAD revision. It is
// the single write path behind POST /repositories/:id/intelligence/refresh.
// When includeAI is set, the deterministic structure is sent to the external
// AI service for architecture interpretation; any AI failure degrades to a
// deterministic-only profile (status ai_unavailable, never an error), because
// deterministic facts are never blocked behind an AI dependency.
func (s *Service) RefreshIntelligence(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	includeAI bool,
) (*RepositoryIntelligence, error) {
	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)
	if err != nil {
		return nil, err
	}

	intelligence, err := s.generateIntelligence(ctx, selected, includeAI)
	if err != nil {
		return nil, err
	}

	if err := s.store.UpsertIntelligence(intelligence); err != nil {
		return nil, err
	}

	return intelligence, nil
}

// generateIntelligence builds a fresh, revision-bound profile from the
// deterministic representation pipeline, optionally enriching it with
// AI-inferred architecture. It never persists.
func (s *Service) generateIntelligence(
	ctx context.Context,
	selected *SelectedRepository,
	includeAI bool,
) (*RepositoryIntelligence, error) {
	represented, err := s.RepresentRepositoryContent(ctx, selected.UserID, selected.ID)
	if err != nil {
		return nil, err
	}

	profile := repoinfo.Discover(represented.Chunks, repoinfo.DefaultLimits())

	revision, err := s.currentRevision(ctx, selected)
	if err != nil {
		log.Printf("revision resolution failed for repository %s: %v", selected.ID, err)
		revision = ""
	}

	intelligence := &RepositoryIntelligence{
		SelectedRepositoryID: selected.ID,
		Revision:             revision,
		Status:               IntelligenceStatusCurrent,
		GeneratedAt:          time.Now(),
	}

	if includeAI {
		s.enrichIntelligenceWithAI(ctx, selected, profile, intelligence)
	}

	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	intelligence.ProfileJSON = string(profileJSON)

	return intelligence, nil
}

// currentRevision resolves the current HEAD commit SHA of the selected
// repository's workspace. An empty revision (environment cannot resolve) is
// not an error — it just means staleness cannot be attested.
func (s *Service) currentRevision(ctx context.Context, selected *SelectedRepository) (string, error) {
	if s.publisher == nil || s.workspaces == nil {
		return "", nil
	}

	return s.publisher.HeadSHA(ctx, s.workspaces.Dir(selected.ID))
}

// applyStaleness derives freshness from the current HEAD revision at read time.
// A profile whose revision is unknown, or whose current revision cannot be
// resolved, keeps its stored claims; only a real mismatch marks it stale.
func (s *Service) applyStaleness(ctx context.Context, selected *SelectedRepository, intelligence *RepositoryIntelligence) {
	if intelligence.Revision == "" {
		return
	}

	current, err := s.currentRevision(ctx, selected)
	if err != nil || current == "" {
		if err != nil {
			log.Printf("revision resolution failed for repository %s: %v", selected.ID, err)
		}
		return
	}

	intelligence.Stale = current != intelligence.Revision
}

// enrichIntelligenceWithAI asks the external AI service to interpret the
// deterministic structure. AI failure degrades in place (status
// ai_unavailable + note) and never blocks deterministic intelligence.
func (s *Service) enrichIntelligenceWithAI(
	ctx context.Context,
	selected *SelectedRepository,
	profile *repoinfo.Profile,
	intelligence *RepositoryIntelligence,
) {
	if s.aiClient == nil {
		intelligence.Status = IntelligenceStatusAIUnavailable
		intelligence.AINote = "ai analysis not configured; deterministic intelligence used"
		return
	}

	response, err := s.aiClient.AnalyzeRepository(ctx, &ai.AnalyzeRepositoryRequest{
		RepositoryID:   selected.ID.String(),
		RepositoryName: selected.Name,
		Structure:      aiStructureOf(profile),
	})
	if err != nil {
		log.Printf("repository intelligence AI enrichment failed for repository %s: %v",
			selected.ID, err)
		intelligence.Status = IntelligenceStatusAIUnavailable
		intelligence.AINote = "ai analysis unavailable; deterministic intelligence used"
		return
	}

	profile.ApplyArchitecture(architectureFromAI(response))
}

// aiStructureOf converts the deterministic profile into the compact
// AI-request structure. Only bounded, repository-derived facts are sent.
func aiStructureOf(profile *repoinfo.Profile) ai.RepositoryStructure {
	structure := profile.Structure

	return ai.RepositoryStructure{
		Languages:   sortedKeys(structure.LanguageCounts),
		Files:       structure.Files,
		Chunks:      structure.Chunks,
		EntryPoints: entryPointLabels(structure.EntryPoints),
		AppScopes:   appScopeLabels(structure.AppScopes),
		ConfigFiles: fileRefLabels(structure.ConfigFiles),
		TestFiles:   fileRefLabels(structure.TestFiles),
		BuildFiles:  fileRefLabels(structure.BuildFiles),
	}
}

// architectureFromAI converts a validated AI interpretation into the labeled
// profile architecture. Every item is LevelInferred by construction.
func architectureFromAI(response *ai.AnalyzeRepositoryResponse) repoinfo.Architecture {
	architecture := repoinfo.Architecture{
		Overview:    strings.TrimSpace(response.Overview),
		Uncertainty: append([]string(nil), response.Uncertainty...),
	}

	for _, item := range response.Components {
		architecture.Components = append(architecture.Components, repoinfo.Component{
			Name:             strings.TrimSpace(item.Name),
			Kind:             strings.TrimSpace(item.Kind),
			Path:             strings.TrimSpace(item.Path),
			Description:      strings.TrimSpace(item.Description),
			Responsibilities: append([]string(nil), item.Responsibilities...),
			Level:            repoinfo.LevelInferred,
		})
	}

	for _, item := range response.Relationships {
		architecture.Relationships = append(architecture.Relationships, repoinfo.Relationship{
			From:  strings.TrimSpace(item.From),
			To:    strings.TrimSpace(item.To),
			Kind:  strings.TrimSpace(item.Kind),
			Level: repoinfo.LevelInferred,
		})
	}

	for _, text := range response.EngineeringDecisions {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			architecture.EngineeringDecisions = append(architecture.EngineeringDecisions, repoinfo.Statement{
				Text:  trimmed,
				Level: repoinfo.LevelInferred,
			})
		}
	}

	for _, text := range response.Databases {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			architecture.Databases = append(architecture.Databases, repoinfo.Statement{
				Text:  trimmed,
				Level: repoinfo.LevelInferred,
			})
		}
	}

	for _, text := range response.ExternalIntegrations {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			architecture.ExternalIntegrations = append(architecture.ExternalIntegrations, repoinfo.Statement{
				Text:  trimmed,
				Level: repoinfo.LevelInferred,
			})
		}
	}

	return architecture
}

// repositoryContextForSelected builds the compact repository intelligence
// context for one owned selected repository, or returns nil when no fresh
// profile exists. A missing or stale profile yields nil: stale context is
// never silently presented to the AI service.
func (s *Service) repositoryContextForSelected(ctx context.Context, selected *SelectedRepository) *ai.RepositoryContext {
	intelligence, err := s.store.GetIntelligence(selected.ID)
	if err != nil || intelligence == nil {
		return nil
	}

	s.applyStaleness(ctx, selected, intelligence)
	if intelligence.Stale {
		return nil
	}

	var profile repoinfo.Profile
	if err := json.Unmarshal([]byte(intelligence.ProfileJSON), &profile); err != nil {
		log.Printf("repository intelligence decode failed for repository %s: %v", selected.ID, err)
		return nil
	}

	return &ai.RepositoryContext{
		DeterministicSummary: deterministicSummary(&profile),
		Languages:            sortedKeys(profile.Structure.LanguageCounts),
		EntryPoints:          entryPointLabels(profile.Structure.EntryPoints),
		Components:           appScopeLabels(profile.Structure.AppScopes),
		Conventions:          conventionLabels(profile.Conventions),
		ArchitectureOverview: profile.Architecture.Overview,
		Relationships:        relationshipLabels(profile.Architecture.Relationships),
		Uncertainty:          profile.Architecture.Uncertainty,
	}
}

// deterministicSummary is a compact, machine-readable one-line structure
// summary: languages, entry points, app scopes, test count.
func deterministicSummary(profile *repoinfo.Profile) string {
	structure := profile.Structure
	parts := []string{
		"languages=[" + strings.Join(sortedKeys(structure.LanguageCounts), ",") + "]",
	}
	if len(structure.EntryPoints) > 0 {
		parts = append(parts, "entry_points=["+strings.Join(entryPointLabels(structure.EntryPoints), ",")+"]")
	}
	if len(structure.AppScopes) > 0 {
		parts = append(parts, "app_scopes=["+strings.Join(appScopeLabels(structure.AppScopes), ",")+"]")
	}
	if len(structure.TestFiles) > 0 {
		parts = append(parts, "test_files="+itoa(len(structure.TestFiles)))
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func entryPointLabels(entries []repoinfo.EntryPoint) []string {
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		labels = append(labels, entry.Path+"("+entry.Kind+")")
	}
	return labels
}

func appScopeLabels(scopes []repoinfo.AppScope) []string {
	labels := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		labels = append(labels, scope.Path+"("+scope.Kind+")")
	}
	return labels
}

func fileRefLabels(refs []repoinfo.FileRef) []string {
	labels := make([]string, 0, len(refs))
	for _, ref := range refs {
		labels = append(labels, ref.Path)
	}
	return labels
}

func conventionLabels(conventions []repoinfo.Convention) []string {
	labels := make([]string, 0, len(conventions))
	for _, convention := range conventions {
		labels = append(labels, convention.Name+" ["+string(convention.Level)+"]")
	}
	return labels
}

func relationshipLabels(relationships []repoinfo.Relationship) []string {
	labels := make([]string, 0, len(relationships))
	for _, relationship := range relationships {
		labels = append(labels, relationship.From+" "+relationship.Kind+" "+relationship.To)
	}
	return labels
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
