package repositories

import (
	"time"

	"github.com/Aevor/platform/services/api/internal/github"
)

// RepositoryResponse is the Aevor-safe view of a GitHub repository. It is
// built field-by-field from github.Repository so the raw GitHub payload (and
// anything secret-bearing) can never leak into a response.
type RepositoryResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	OwnerLogin    string `json:"owner_login"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	APIURL        string `json:"api_url"`
}

type ListResponse struct {
	Repositories []RepositoryResponse `json:"repositories"`

	Page    int `json:"page"`
	PerPage int `json:"per_page"`

	HasMore bool `json:"has_more"`
}

func ToRepositoryResponse(repository github.Repository) RepositoryResponse {
	return RepositoryResponse{
		ID:            repository.ID,
		Name:          repository.Name,
		FullName:      repository.FullName,
		Description:   repository.Description,
		Private:       repository.Private,
		DefaultBranch: repository.DefaultBranch,
		OwnerLogin:    repository.Owner.Login,
		HTMLURL:       repository.HTMLURL,
		CloneURL:      repository.CloneURL,
		APIURL:        repository.APIURL,
	}
}

func ToListResponse(
	repositories []github.Repository,
	page int,
	perPage int,
	hasMore bool,
) ListResponse {
	response := ListResponse{
		Repositories: make([]RepositoryResponse, 0, len(repositories)),
		Page:         page,
		PerPage:      perPage,
		HasMore:      hasMore,
	}

	for _, repository := range repositories {
		response.Repositories = append(response.Repositories, ToRepositoryResponse(repository))
	}

	return response
}

// SelectRequest carries only the GitHub repository identifier. The backend
// resolves everything else from the authoritative GitHub API using the
// authenticated user's own credentials.
type SelectRequest struct {
	GithubRepositoryID int64 `json:"github_repository_id"`
}

// SelectedRepositoryResponse is the Aevor-safe view of a persisted repository
// context. UserID is deliberately omitted: ownership is implicit in the
// authenticated session.
type SelectedRepositoryResponse struct {
	ID                 string    `json:"id"`
	GithubRepositoryID int64     `json:"github_repository_id"`
	Name               string    `json:"name"`
	FullName           string    `json:"full_name"`
	OwnerLogin         string    `json:"owner_login"`
	Private            bool      `json:"private"`
	DefaultBranch      string    `json:"default_branch"`
	HTMLURL            string    `json:"html_url"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func ToSelectedRepositoryResponse(selected SelectedRepository) SelectedRepositoryResponse {
	return SelectedRepositoryResponse{
		ID:                 selected.ID.String(),
		GithubRepositoryID: selected.GithubRepositoryID,
		Name:               selected.Name,
		FullName:           selected.FullName,
		OwnerLogin:         selected.OwnerLogin,
		Private:            selected.Private,
		DefaultBranch:      selected.DefaultBranch,
		HTMLURL:            selected.HTMLURL,
		CreatedAt:          selected.CreatedAt,
		UpdatedAt:          selected.UpdatedAt,
	}
}

func ToSelectedListResponse(selected []SelectedRepository) map[string][]SelectedRepositoryResponse {
	response := make([]SelectedRepositoryResponse, 0, len(selected))

	for _, repository := range selected {
		response = append(response, ToSelectedRepositoryResponse(repository))
	}

	return map[string][]SelectedRepositoryResponse{"repositories": response}
}
