package users

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func strPtr(s string) *string {
	return &s
}

type fakeTokenRepository struct {
	user *User
	err  error
}

func (f *fakeTokenRepository) Create(user *User) error {
	return nil
}

func (f *fakeTokenRepository) GetByID(id uuid.UUID) (*User, error) {
	if f.err != nil {
		return nil, f.err
	}

	if f.user == nil || f.user.ID != id {
		return nil, gorm.ErrRecordNotFound
	}

	return f.user, nil
}

func (f *fakeTokenRepository) GetByGitHubID(githubID int64) (*User, error) {
	return nil, gorm.ErrRecordNotFound
}

func (f *fakeTokenRepository) Update(user *User) error {
	return nil
}

func (f *fakeTokenRepository) UpsertByGitHubID(user *User) error {
	return nil
}

func TestDecryptedGitHubToken_Success(t *testing.T) {
	key := testEncryptionKey

	ciphertext, err := Encrypt("ghs_plain-test-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	userID := uuid.New()

	service := NewService(&fakeTokenRepository{
		user: &User{ID: userID, GithubID: 42, Username: "octocat", GitHubAccessToken: &ciphertext},
	})

	token, err := service.DecryptedGitHubToken(userID, key)

	if err != nil {
		t.Fatalf("DecryptedGitHubToken() error: %v", err)
	}

	if token != "ghs_plain-test-token" {
		t.Errorf("token = %q, want the decrypted plaintext", token)
	}
}

func TestDecryptedGitHubToken_UserNotFound(t *testing.T) {
	service := NewService(&fakeTokenRepository{})

	if _, err := service.DecryptedGitHubToken(uuid.New(), testEncryptionKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestDecryptedGitHubToken_MissingTokenRejected(t *testing.T) {
	scenarios := []struct {
		name  string
		token *string
	}{
		{"nil token", nil},
		{"blank token", strPtr("   ")},
		{"empty token", strPtr("")},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			userID := uuid.New()

			service := NewService(&fakeTokenRepository{
				user: &User{ID: userID, GithubID: 42, Username: "octocat", GitHubAccessToken: sc.token},
			})

			if _, err := service.DecryptedGitHubToken(userID, testEncryptionKey); !errors.Is(err, ErrGitHubTokenMissing) {
				t.Errorf("error = %v, want ErrGitHubTokenMissing", err)
			}
		})
	}
}

func TestDecryptedGitHubToken_CorruptCiphertextRejected(t *testing.T) {
	userID := uuid.New()

	service := NewService(&fakeTokenRepository{
		user: &User{
			ID:                userID,
			GithubID:          42,
			Username:          "octocat",
			GitHubAccessToken: strPtr("not-valid-base64-ciphertext!!!"),
		},
	})

	if _, err := service.DecryptedGitHubToken(userID, testEncryptionKey); !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestDecryptedGitHubToken_WrongKeyRejected(t *testing.T) {
	key := testEncryptionKey

	ciphertext, err := Encrypt("ghs_plain-test-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	userID := uuid.New()

	service := NewService(&fakeTokenRepository{
		user: &User{ID: userID, GithubID: 42, Username: "octocat", GitHubAccessToken: &ciphertext},
	})

	wrongKey := append([]byte(nil), testEncryptionKey...)
	wrongKey[0] ^= 0xFF

	if _, err := service.DecryptedGitHubToken(userID, wrongKey); !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("error = %v, want ErrInvalidCiphertext for a wrong key", err)
	}
}

func TestDecryptedGitHubToken_ShortKeyRejected(t *testing.T) {
	ciphertext := "irrelevant"

	userID := uuid.New()

	service := NewService(&fakeTokenRepository{
		user: &User{ID: userID, GithubID: 42, Username: "octocat", GitHubAccessToken: &ciphertext},
	})

	if _, err := service.DecryptedGitHubToken(userID, []byte("short")); !errors.Is(err, ErrInvalidKeyLength) {
		t.Errorf("error = %v, want ErrInvalidKeyLength", err)
	}
}
