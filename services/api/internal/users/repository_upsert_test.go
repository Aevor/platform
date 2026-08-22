package users

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func parsedUserSchema(t *testing.T) *schema.Schema {
	t.Helper()

	userSchema, err := schema.Parse(&User{}, &sync.Map{}, schema.NamingStrategy{})

	if err != nil {
		t.Fatalf("could not parse the User model schema: %v", err)
	}

	return userSchema
}

func mappedColumnNames(t *testing.T) map[string]bool {
	t.Helper()

	userSchema := parsedUserSchema(t)

	columns := make(map[string]bool)

	for _, field := range userSchema.Fields {
		if field.DBName != "" {
			columns[field.DBName] = true
		}
	}

	return columns
}

// TestUpsertByGitHubID_ColumnsMatchModelSchema guards against a repeat of the
// production incident where UpsertByGitHubID referenced "github_access_token"
// while GORM maps User.GitHubAccessToken to the column
// "git_hub_access_token" — which made every re-login upsert fail with
// "column excluded.github_access_token does not exist".
func TestUpsertByGitHubID_ColumnsMatchModelSchema(t *testing.T) {
	columns := mappedColumnNames(t)

	for _, column := range upsertConflictColumns {
		if !columns[column.Name] {
			t.Errorf(
				"upsert conflict column %q is not mapped by the User model; ON CONFLICT would reference a nonexistent column",
				column.Name,
			)
		}
	}

	if len(upsertConflictColumns) == 0 || upsertConflictColumns[0].Name != "github_id" {
		t.Errorf("upsert conflict target = %v, want github_id (the alternate key)", upsertConflictColumns)
	}

	for _, column := range upsertAssignmentColumns {
		if !columns[column] {
			t.Errorf(
				"upsert assignment column %q is not mapped by the User model; DO UPDATE SET would fail at runtime",
				column,
			)
		}
	}
}

// TestUpsertByGitHubID_TokenColumnIsPinnedToModelField ensures the encrypted
// GitHub access token is written to the exact column the User model maps,
// so re-login token rotation can never silently target the wrong column.
func TestUpsertByGitHubID_TokenColumnIsPinnedToModelField(t *testing.T) {
	userSchema := parsedUserSchema(t)

	field := userSchema.LookUpField("git_hub_access_token")

	if field == nil {
		t.Fatal("the User model does not map a git_hub_access_token column")
	}

	if field.Name != "GitHubAccessToken" {
		t.Errorf("column git_hub_access_token is bound to field %q, want GitHubAccessToken", field.Name)
	}

	found := false

	for _, column := range upsertAssignmentColumns {
		if column == "git_hub_access_token" {
			found = true

			break
		}
	}

	if !found {
		t.Error("upsert assignment columns do not refresh git_hub_access_token on re-login")
	}

	for _, column := range upsertAssignmentColumns {
		if column == "github_access_token" {
			t.Error("upsert references github_access_token, a column that does not exist")
		}
	}
}

// TestUserModel_TokenColumnNeverSerialized ensures the pinned tag did not
// change JSON behavior: the GitHub access token must stay out of responses.
func TestUserModel_TokenColumnNeverSerialized(t *testing.T) {
	userSchema := parsedUserSchema(t)

	field := userSchema.LookUpField("git_hub_access_token")

	if field == nil {
		t.Fatal("the User model does not map a git_hub_access_token column")
	}

	if _, ok := field.TagSettings["JSON"]; ok {
		t.Errorf("token field has a JSON tag %q; it must never be serialized", field.TagSettings["JSON"])
	}
}
