package repositories

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/indexing"
)

// PullRequestFeedbackResult wraps the structured feedback analysis returned
// by the external AI service for one pull request. It never contains raw AI
// output; the response is parsed and validated by the ai.Client before it
// reaches business logic.
type PullRequestFeedbackResult struct {
	Response *ai.AnalyzePRFeedbackResponse
}

// AnalyzePullRequestFeedback builds a live, grounded feedback analysis for
// one pull request owned by the authenticated user. The flow:
//
//  1. Verify ownership via the JWT-derived userID (same as every other method)
//  2. Fail closed if aiClient is nil (AI not configured)
//  3. Fetch GitHub's authoritative PR record with its head SHA (source of
//     truth; nothing is cached)
//  4. Fetch the PR's checks, reviews, review comments, issue-thread comments,
//     and changed files exactly as GitHub reports them
//  5. Assemble bounded ContextChunks scoped to the PR's changed files,
//     enriching each from the represented source content
//  6. Build AnalyzePRFeedbackRequest with repository identity + GitHub facts
//     + bounded chunks
//  7. Call aiClient.AnalyzePRFeedback (HTTP to the separate AI service)
//  8. Return the structured, validated response
//
// Security: the request carries ONLY repository identity, GitHub's own pull
// request facts, and bounded context chunks. NEVER GitHub tokens, JWT
// secrets, encryption keys, credentials, or environment variables. All
// free-form text (PR body, reviews, comments) travels as data — never as
// instructions. Source content stays bounded and is forwarded to the
// SEPARATE AI service only. Raw AI output never leaves this method.
func (s *Service) AnalyzePullRequestFeedback(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	number int,
) (*PullRequestFeedbackResult, error) {
	if s.aiClient == nil {
		return nil, fmt.Errorf("ai analysis subsystem is not configured")
	}

	selected, err := s.store.FindByUserAndID(userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	token, err := s.users.DecryptedGitHubToken(userID, s.encryptionKey)

	if err != nil {
		return nil, err
	}

	pr, err := s.github.GetPullRequest(ctx, token, selected.OwnerLogin, selected.Name, number)

	if err != nil {
		log.Printf("github pull request fetch failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	checks, err := s.github.ListPullRequestChecks(ctx, token, selected.OwnerLogin, selected.Name, pr.Head.SHA)

	if err != nil {
		log.Printf("github check list failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	reviews, err := s.github.ListPullRequestReviews(ctx, token, selected.OwnerLogin, selected.Name, number)

	if err != nil {
		log.Printf("github review list failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	reviewComments, err := s.github.ListPullRequestReviewComments(ctx, token, selected.OwnerLogin, selected.Name, number)

	if err != nil {
		log.Printf("github review comment list failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	issueComments, err := s.github.ListPullRequestIssueComments(ctx, token, selected.OwnerLogin, selected.Name, number)

	if err != nil {
		log.Printf("github issue comment list failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	files, err := s.github.ListPullRequestFiles(ctx, token, selected.OwnerLogin, selected.Name, number)

	if err != nil {
		log.Printf("github file list failed for user %s repository %s PR %d: %v", userID, selected.ID, number, err)
		return nil, err
	}

	// A feedback analysis is grounded in the repository's own code, so the
	// workspace must already be prepared. The gate sits AFTER GitHub's
	// authoritative PR facts are known and BEFORE any AI call: an unprepared
	// workspace is a conflict the caller can fix, never a silent analysis of
	// stale or empty context.
	ready, err := s.workspaces.Ready(selected.ID)

	if err != nil {
		return nil, err
	}

	if !ready {
		return nil, ErrWorkspaceNotReady
	}

	chunks, err := s.prFeedbackContextChunks(ctx, userID, selected.ID, files)

	if err != nil {
		return nil, err
	}

	request := &ai.AnalyzePRFeedbackRequest{
		RepositoryID:   selected.ID.String(),
		RepositoryName: selected.Name,
		PullRequest: ai.PullRequestInfo{
			Number:      pr.Number,
			Title:       pr.Title,
			Body:        pr.Body,
			AuthorLogin: pr.User.Login,
			State:       pr.State,
			HeadSHA:     pr.Head.SHA,
		},
		Checks:            toAIChecks(checks),
		Reviews:           toAIReviews(reviews),
		ReviewComments:    toAIReviewComments(reviewComments),
		IssueComments:     toAIIssueComments(issueComments),
		Files:             toAIFiles(files),
		ContextChunks:     chunks,
		RepositoryContext: ai.SanitizeRepositoryContext(s.repositoryContextForSelected(ctx, selected)),
	}

	aiStarted := time.Now()
	response, err := s.aiClient.AnalyzePRFeedback(ctx, request)
	aiDuration := time.Since(aiStarted).Milliseconds()

	if err != nil {
		log.Printf("ai pull request feedback analysis failed for user %s repository %s PR %d: %v",
			userID, selected.ID, number, err)
		return nil, err
	}

	// Attach the feedback to the run that delivered this pull request (when one
	// exists): blocking items open an iteration, a clean review completes the
	// run. PRs that were never delivered by Aevor produce no run to update.
	run, runErr := s.store.FindRunByPullRequest(selected.ID, number)
	if runErr != nil {
		return nil, runErr
	}
	if run != nil {
		_ = s.recordEvent(ctx, run, EventPullRequestFeedbackReceived, EventStatusSuccess, aiDuration,
			aiStageMetadata("pr_feedback", aiDuration, true, len(chunks), true), "", "")
		if len(response.Blocking) == 0 {
			_ = s.recordEvent(ctx, run, EventRunCompleted, EventStatusSuccess, 0, nil, "", "")
			s.advanceRun(ctx, run, RunStatusCompleted, "COMPLETED", map[string]any{"completed_at": time.Now()})
		} else {
			_ = s.recordEvent(ctx, run, EventIterationStarted, EventStatusSuccess, 0, nil, "", "")
			s.advanceRun(ctx, run, RunStatusIterating, "ITERATING", nil)
		}
	}

	return &PullRequestFeedbackResult{Response: response}, nil
}

// prFeedbackContextChunks assembles the bounded, file-scoped context for a
// pull request feedback analysis. It reuses the representation pipeline for
// actual source content and the metadata index for traceability, then keeps
// only the chunks belonging to files the pull request changed (or, when no
// changed file maps to the index, a bounded fallback subset so an analysis
// is still attempted with whatever context exists). Selection is
// deterministic and metadata-only; each eligible chunk is enriched with its
// bounded source content for grounding, exactly like the query-based flow.
func (s *Service) prFeedbackContextChunks(
	ctx context.Context,
	userID uuid.UUID,
	selectedRepositoryID uuid.UUID,
	files []github.PullRequestFile,
) ([]ai.ContextChunk, error) {
	represented, err := s.RepresentRepositoryContent(ctx, userID, selectedRepositoryID)

	if err != nil {
		return nil, err
	}

	contentByID := make(map[string]string, len(represented.Chunks))
	for i := range represented.Chunks {
		contentByID[represented.Chunks[i].ID] = represented.Chunks[i].Content
	}

	records := s.index.Lookup(indexing.Query{
		RepositoryID: selectedRepositoryID.String(),
	})

	records = selectRecordsForFiles(files, records, maxRelevantChunksPerQuery)

	chunks := make([]ai.ContextChunk, 0, len(records))

	for i := range records {
		record := &records[i]

		chunk := ai.ContextChunk{
			ID:         record.ID,
			FilePath:   record.FilePath,
			Language:   record.Language,
			FileRole:   record.FileRole,
			ChunkIndex: record.ChunkIndex,
			StartLine:  record.StartLine,
			EndLine:    record.EndLine,
			SymbolType: record.SymbolType,
		}

		if record.SymbolName != nil {
			chunk.SymbolName = record.SymbolName
		}

		if record.ParentSymbol != nil {
			chunk.ParentSymbol = record.ParentSymbol
		}

		// Enrich with the actual bounded source content for this chunk. When
		// the deterministic ID has no matching representation (should not
		// happen when index and representation come from the same pipeline),
		// the chunk is still sent but without content.
		chunk.Content = contentByID[record.ID]

		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// selectRecordsForFiles keeps only the indexed records whose repository-relative
// file path is one of the pull request's changed files, in deterministic order,
// bounded to limit. When the changed files cannot be matched to the index (no
// index yet, phantom paths, or a foreign-to-Aevor repository), it degrades to
// the first limit records in deterministic index order — the AI still gets the
// feedback and can flag insufficient context rather than failing outright.
func selectRecordsForFiles(files []github.PullRequestFile, records []indexing.Record, limit int) []indexing.Record {
	if len(files) == 0 {
		return boundedRecords(records, limit)
	}

	changed := make(map[string]struct{}, len(files))
	for _, file := range files {
		if path := strings.ToLower(strings.TrimSpace(file.Filename)); path != "" {
			changed[path] = struct{}{}
		}
	}

	selected := make([]indexing.Record, 0, len(records))
	for i := range records {
		record := records[i]
		if _, ok := changed[strings.ToLower(record.FilePath)]; ok {
			selected = append(selected, record)
		}
	}

	if len(selected) == 0 {
		return boundedRecords(records, limit)
	}

	sort.SliceStable(selected, func(a, b int) bool {
		return precedesDeterministically(selected[a], selected[b])
	})

	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}

	return selected
}

func toAIChecks(checks []github.Check) []ai.PRCheckInfo {
	out := make([]ai.PRCheckInfo, 0, len(checks))
	for _, check := range checks {
		out = append(out, ai.PRCheckInfo{
			Name:        check.Name,
			Status:      check.Status,
			Description: check.Description,
			URL:         check.URL,
			Source:      check.Source,
		})
	}
	return out
}

func toAIReviews(reviews []github.Review) []ai.PRReview {
	out := make([]ai.PRReview, 0, len(reviews))
	for _, review := range reviews {
		out = append(out, ai.PRReview{
			ID:          int(review.ID),
			AuthorLogin: review.User.Login,
			State:       review.State,
			Body:        review.Body,
		})
	}
	return out
}

func toAIReviewComments(comments []github.ReviewComment) []ai.PRReviewComment {
	out := make([]ai.PRReviewComment, 0, len(comments))
	for _, comment := range comments {
		// GitHub anchors a review comment either to a line in the new file or
		// only to the file/diff hunk. GitHub's own anchor travels unchanged, so
		// a position-less (outdated) comment stays null instead of acquiring a
		// fabricated line number.
		out = append(out, ai.PRReviewComment{
			ID:          int(comment.ID),
			AuthorLogin: comment.User.Login,
			Body:        comment.Body,
			Path:        comment.Path,
			Line:        comment.Line,
			DiffHunk:    comment.DiffHunk,
		})
	}
	return out
}

func toAIIssueComments(comments []github.IssueComment) []ai.PRIssueComment {
	out := make([]ai.PRIssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, ai.PRIssueComment{
			ID:          int(comment.ID),
			AuthorLogin: comment.User.Login,
			Body:        comment.Body,
		})
	}
	return out
}

func toAIFiles(files []github.PullRequestFile) []ai.PRFileInfo {
	out := make([]ai.PRFileInfo, 0, len(files))
	for _, file := range files {
		out = append(out, ai.PRFileInfo{
			Filename:  file.Filename,
			Status:    file.Status,
			Additions: file.Additions,
			Deletions: file.Deletions,
		})
	}
	return out
}
