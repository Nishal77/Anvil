package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// githubSecretName is the well-known secret name a linked GitHub
// account's access token is stored under (PRD §16.5) — this is what
// the Runner's credential-injection path (git_push, github_open_pr,
// SEC-020) resolves via ResolveSecret.
const githubSecretName = "GITHUB_TOKEN"

// githubOAuthScope requests exactly what SEC-020/PRD §16.5 need: write
// access to repository contents and pull requests, nothing broader.
const githubOAuthScope = "repo"

const (
	defaultGitHubOAuthBaseURL = "https://github.com"
	defaultGitHubAPIBaseURL   = "https://api.github.com"
)

// BeginGitHubOAuth returns the URL to send userID's browser to, to
// start linking a GitHub account. Returns ErrGitHubNotConfigured if no
// GitHub OAuth app is configured for this deployment.
func (s *Service) BeginGitHubOAuth(userID uuid.UUID) (string, error) {
	if !s.githubConfigured() {
		return "", ErrGitHubNotConfigured
	}

	state, err := encodeOAuthState(s.signingKey, userID)
	if err != nil {
		return "", fmt.Errorf("auth: begin github oauth: %w", err)
	}

	q := url.Values{
		"client_id":    {s.githubClientID},
		"redirect_uri": {s.githubRedirectURL},
		"scope":        {githubOAuthScope},
		"state":        {state},
		"allow_signup": {"false"},
	}
	return s.githubOAuthBaseURL + "/login/oauth/authorize?" + q.Encode(), nil
}

// CompleteGitHubOAuth verifies state, exchanges code for a GitHub
// access token, links the GitHub account to the state's user, and
// stores the token as that user's GITHUB_TOKEN secret. Returns the URL
// the caller's browser should be sent to next — githubWebURL if
// configured, or "" if the deployment has no frontend configured (the
// API layer returns a JSON confirmation instead in that case).
func (s *Service) CompleteGitHubOAuth(ctx context.Context, code, state string) (string, error) {
	if !s.githubConfigured() {
		return "", ErrGitHubNotConfigured
	}

	userID, err := decodeOAuthState(s.signingKey, state)
	if err != nil {
		return "", err
	}

	token, err := s.exchangeGitHubCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("auth: complete github oauth: %w", err)
	}

	githubID, githubLogin, err := s.fetchGitHubUser(ctx, token)
	if err != nil {
		return "", fmt.Errorf("auth: complete github oauth: %w", err)
	}

	if err := s.store.SetGitHubIdentity(ctx, userID, githubID, githubLogin); err != nil {
		if errors.Is(err, storage.ErrDuplicateGitHubID) {
			return "", ErrGitHubAccountAlreadyLinked
		}
		return "", fmt.Errorf("auth: complete github oauth: link identity: %w", err)
	}

	if err := s.PutSecret(ctx, userID, githubSecretName, token); err != nil {
		return "", fmt.Errorf("auth: complete github oauth: store token: %w", err)
	}

	s.log.InfoContext(ctx, "github account linked", slog.String("user_id", userID.String()))
	return s.githubWebURL, nil
}

func (s *Service) githubConfigured() bool {
	return s.githubClientID != "" && s.githubClientSecret != "" && s.githubRedirectURL != ""
}

type githubTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// exchangeGitHubCode redeems an OAuth authorization code for an access
// token via GitHub's token endpoint.
func (s *Service) exchangeGitHubCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {s.githubClientID},
		"client_secret": {s.githubClientSecret},
		"code":          {code},
		"redirect_uri":  {s.githubRedirectURL},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.githubOAuthBaseURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out githubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token exchange response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("github rejected the authorization code: %s: %s", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", errors.New("github token exchange returned no access_token")
	}
	return out.AccessToken, nil
}

type githubUserResponse struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// fetchGitHubUser returns the GitHub account identity behind token.
func (s *Service) fetchGitHubUser(ctx context.Context, token string) (id int64, login string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.githubAPIBaseURL+"/user", nil)
	if err != nil {
		return 0, "", fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("user request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("github user endpoint returned %s", resp.Status)
	}

	var out githubUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, "", fmt.Errorf("decode user response: %w", err)
	}
	return out.ID, out.Login, nil
}
