package repositories

import "github.com/Aevor/platform/services/api/internal/github"

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
