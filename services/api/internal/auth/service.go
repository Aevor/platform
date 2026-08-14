package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

var (
	ErrInvalidState        = errors.New("invalid oauth state")
	ErrAuthorizationDenied = errors.New("github authorization denied")
	ErrInvalidCode         = errors.New("invalid authorization code")
	ErrGitHubUnavailable   = errors.New("github unavailable")
	ErrInternal            = errors.New("internal error")
)

type Service struct {
	oauthConfig   *oauth2.Config
	users         *users.Service
	jwtManager    *JWTManager
	ghClient      *github.Client
	encryptionKey []byte
}

func NewService(
	oauthConfig *oauth2.Config,
	userService *users.Service,
	jwtManager *JWTManager,
	ghClient *github.Client,
	encryptionKey []byte,
) *Service {
	return &Service{
		oauthConfig:   oauthConfig,
		users:         userService,
		jwtManager:    jwtManager,
		ghClient:      ghClient,
		encryptionKey: encryptionKey,
	}
}

type CallbackParams struct {
	Code          string
	ActualState   string
	ExpectedState string
	CodeVerifier  string
	GitHubError   string
}

func (s *Service) LoginURL() (string, string, string, error) {
	state, err := GenerateState()

	if err != nil {
		return "", "", "", err
	}

	verifier := oauth2.GenerateVerifier()

	url := s.oauthConfig.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
	)

	return url, state, verifier, nil
}

func (s *Service) HandleCallback(ctx context.Context, params CallbackParams) (*GitHubUser, error) {
	if params.ExpectedState == "" || params.CodeVerifier == "" {
		return nil, ErrInvalidState
	}

	if !VerifyState(params.ExpectedState, params.ActualState) {
		return nil, ErrInvalidState
	}

	if params.GitHubError == "access_denied" {
		return nil, ErrAuthorizationDenied
	}

	if params.GitHubError != "" {
		return nil, ErrGitHubUnavailable
	}

	if params.Code == "" {
		return nil, ErrInvalidCode
	}

	token, err := s.oauthConfig.Exchange(ctx, params.Code, oauth2.VerifierOption(params.CodeVerifier))

	if err != nil {
		return nil, classifyExchangeError(err)
	}

	profile, err := s.ghClient.GetCurrentUser(ctx, token.AccessToken)

	if err != nil {
		return nil, err
	}

	return profile, nil
}

func classifyExchangeError(err error) error {
	var retrieveErr *oauth2.RetrieveError

	if errors.As(err, &retrieveErr) {
		if retrieveErr.Response != nil && retrieveErr.Response.StatusCode >= http.StatusInternalServerError {
			return ErrGitHubUnavailable
		}

		return ErrInvalidCode
	}

	return ErrGitHubUnavailable
}

func (s *Service) GetProfile(ctx context.Context, userID uuid.UUID) (*users.User, error) {
	user, err := s.users.GetUserByID(userID)

	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, users.ErrNotFound
		}

		return nil, ErrInternal
	}

	return user, nil
}
