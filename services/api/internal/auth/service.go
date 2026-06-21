package auth

import "golang.org/x/oauth2"

type Service struct {
	oauthConfig *oauth2.Config
}

func NewService(
	oauthConfig *oauth2.Config,
) *Service {
	return &Service{
		oauthConfig: oauthConfig,
	}
}

func (s *Service) GetGitHubLoginURL() string {
	return s.oauthConfig.AuthCodeURL(
		"aevor-state",
	)
}
