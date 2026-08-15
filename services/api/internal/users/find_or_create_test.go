package users

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/github"
)

var testEncryptionKey = func() []byte {
	key, err := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	if err != nil {
		panic(err)
	}

	return key
}()

type fakeRepository struct {
	users   map[int64]*User
	creates []*User
	upserts []*User
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		users: make(map[int64]*User),
	}
}

func (f *fakeRepository) Create(user *User) error {
	user.ID = uuid.New()
	f.creates = append(f.creates, user)
	f.users[user.GithubID] = user

	return nil
}

func (f *fakeRepository) GetByID(id uuid.UUID) (*User, error) {
	for _, user := range f.users {
		if user.ID == id {
			return user, nil
		}
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeRepository) GetByGitHubID(githubID int64) (*User, error) {
	if user, ok := f.users[githubID]; ok {
		return user, nil
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeRepository) Update(user *User) error {
	f.users[user.GithubID] = user

	return nil
}

func (f *fakeRepository) UpsertByGitHubID(user *User) error {
	if existing, ok := f.users[user.GithubID]; ok {
		user.ID = existing.ID
		user.CreatedAt = existing.CreatedAt
		*existing = *user
		f.upserts = append(f.upserts, user)

		return nil
	}

	return f.Create(user)
}

func testProfile() github.User {
	return github.User{
		ID:        583231,
		Login:     "octocat",
		Name:      "The Octocat",
		Email:     "octocat@example.com",
		AvatarURL: "https://avatars.githubusercontent.com/u/583231",
	}
}

func TestFindOrCreateByGitHubID_CreatesNewUserWithEncryptedToken(t *testing.T) {
	repository := newFakeRepository()
	service := NewService(repository)

	user, err := service.FindOrCreateByGitHubID(testProfile(), "encrypted-token-value")

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	if user == nil {
		t.Fatal("FindOrCreateByGitHubID() returned a nil user")
	}

	if user.ID == uuid.Nil {
		t.Error("created user has no Aevor UUID")
	}

	if user.GithubID != 583231 {
		t.Errorf("user.GithubID = %d, want 583231", user.GithubID)
	}

	if user.Username != "octocat" {
		t.Errorf("user.Username = %q, want octocat", user.Username)
	}

	if user.DisplayName != "The Octocat" {
		t.Errorf("user.DisplayName = %q, want The Octocat", user.DisplayName)
	}

	if user.Email != "octocat@example.com" {
		t.Errorf("user.Email = %q, want octocat@example.com", user.Email)
	}

	if user.GitHubAccessToken == nil {
		t.Fatal("user.GitHubAccessToken is nil for a new user")
	}

	if *user.GitHubAccessToken != "encrypted-token-value" {
		t.Errorf("user.GitHubAccessToken = %q, want the encrypted token", *user.GitHubAccessToken)
	}

	if len(repository.creates) != 1 {
		t.Errorf("Create() calls = %d, want 1", len(repository.creates))
	}

	if len(repository.upserts) != 0 {
		t.Errorf("Upsert() calls = %d, want 0 for a new user", len(repository.upserts))
	}
}

func TestFindOrCreateByGitHubID_ReLoginKeepsAevorUUIDAndReplacesToken(t *testing.T) {
	repository := newFakeRepository()

	existing := &User{
		ID:                uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		GithubID:          583231,
		Username:          "octocat",
		GitHubAccessToken: stringPtr("encrypted-old-token"),
	}

	repository.users[583231] = existing

	service := NewService(repository)

	user, err := service.FindOrCreateByGitHubID(testProfile(), "encrypted-new-token")

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	if user.ID != existing.ID {
		t.Errorf("user.ID = %q, want the stored Aevor UUID %q (must never be replaced)", user.ID, existing.ID)
	}

	if *user.GitHubAccessToken != "encrypted-new-token" {
		t.Errorf("user.GitHubAccessToken = %q, want the new encrypted token", *user.GitHubAccessToken)
	}

	if len(repository.users) != 1 {
		t.Errorf("stored users = %d, want 1 (no duplicate on re-login)", len(repository.users))
	}

	if len(repository.creates) != 0 {
		t.Errorf("Create() calls = %d, want 0 for an existing user", len(repository.creates))
	}

	if len(repository.upserts) != 1 {
		t.Errorf("Upsert() calls = %d, want 1 for an existing user", len(repository.upserts))
	}
}

func TestFindOrCreateByGitHubID_ReLoginUpdatesProfileFields(t *testing.T) {
	repository := newFakeRepository()

	repository.users[583231] = &User{
		ID:                uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		GithubID:          583231,
		Username:          "octocat",
		DisplayName:       "Stale Display Name",
		GitHubAccessToken: stringPtr("encrypted-old-token"),
	}

	service := NewService(repository)

	user, err := service.FindOrCreateByGitHubID(testProfile(), "encrypted-new-token")

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	if user.DisplayName != "The Octocat" {
		t.Errorf("user.DisplayName = %q, want the new profile value (last write wins)", user.DisplayName)
	}

	if user.Email != "octocat@example.com" {
		t.Errorf("user.Email = %q, want the new profile value", user.Email)
	}
}

func TestFindOrCreateByGitHubID_TokenStoredEncryptedNotPlaintext(t *testing.T) {
	repository := newFakeRepository()
	service := NewService(repository)

	plaintext := "gho_super-secret-access-token"

	encryptedToken, err := Encrypt(plaintext, testEncryptionKey)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	user, err := service.FindOrCreateByGitHubID(testProfile(), encryptedToken)

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	if user.GitHubAccessToken == nil {
		t.Fatal("stored user has no GitHubAccessToken")
	}

	if *user.GitHubAccessToken == plaintext {
		t.Error("plaintext token was persisted")
	}

	if strings.Contains(*user.GitHubAccessToken, plaintext) {
		t.Error("stored value contains the plaintext token")
	}

	decrypted, err := Decrypt(*user.GitHubAccessToken, testEncryptionKey)

	if err != nil {
		t.Fatalf("stored token does not decrypt: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("stored token decrypts to %q, want %q", decrypted, plaintext)
	}
}

func TestFindOrCreateByGitHubID_ModelJSONNeverIncludesToken(t *testing.T) {
	repository := newFakeRepository()
	service := NewService(repository)

	secret := "enc-very-secret-token-value"

	user, err := service.FindOrCreateByGitHubID(testProfile(), secret)

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	raw, err := json.Marshal(user)

	if err != nil {
		t.Fatalf("json.Marshal(user) error: %v", err)
	}

	if strings.Contains(string(raw), secret) {
		t.Error("marshaled User JSON contains the GitHub access token")
	}
}

func TestFindOrCreateByGitHubID_EncryptionKeyNeverPersisted(t *testing.T) {
	repository := newFakeRepository()
	service := NewService(repository)

	plaintext := "gho_key-not-in-db-token"

	encryptedToken, err := Encrypt(plaintext, testEncryptionKey)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	user, err := service.FindOrCreateByGitHubID(testProfile(), encryptedToken)

	if err != nil {
		t.Fatalf("FindOrCreateByGitHubID() error: %v", err)
	}

	keyHex := hex.EncodeToString(testEncryptionKey)

	raw, err := json.Marshal(user)

	if err != nil {
		t.Fatalf("json.Marshal(user) error: %v", err)
	}

	for _, sensitive := range []string{keyHex, string(testEncryptionKey), plaintext} {
		if strings.Contains(string(raw), sensitive) {
			t.Errorf("marshaled User JSON contains %q (encryption key must never be persisted or serialized)", sensitive)
		}
	}

	stored := repository.users[user.GithubID]

	if stored == nil {
		t.Fatal("no stored user row")
	}

	if stored.GitHubAccessToken != nil && strings.Contains(*stored.GitHubAccessToken, keyHex) {
		t.Error("stored token value contains the encryption key hex")
	}
}

func TestFindOrCreateByGitHubID_PropagatesRepositoryErrors(t *testing.T) {
	wantErr := errors.New("database down")

	service := NewService(&errorRepository{err: wantErr})

	_, err := service.FindOrCreateByGitHubID(testProfile(), "encrypted-token")

	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func stringPtr(s string) *string {
	return &s
}

type errorRepository struct {
	err error
}

func (r *errorRepository) Create(user *User) error {
	return r.err
}

func (r *errorRepository) GetByID(id uuid.UUID) (*User, error) {
	return nil, r.err
}

func (r *errorRepository) GetByGitHubID(githubID int64) (*User, error) {
	return nil, r.err
}

func (r *errorRepository) Update(user *User) error {
	return r.err
}

func (r *errorRepository) UpsertByGitHubID(user *User) error {
	return r.err
}
