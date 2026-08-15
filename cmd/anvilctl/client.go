package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// apiClient is a thin, authenticated HTTP client against cmd/anvil's
// API — anvilctl's only reason for existing, so it stays this small
// rather than growing a generated SDK for two commands.
type apiClient struct {
	addr  string
	token string
	http  *http.Client
}

// newAPIClient logs in (or registers, if login fails with not-found —
// convenient for a first `anvilctl run` against a fresh dev database)
// using ANVIL_EMAIL/ANVIL_PASSWORD, or --email/--password if set, and
// returns a client holding the access token for every subsequent call.
func newAPIClient(ctx context.Context, addr, email, password string) (*apiClient, error) {
	if email == "" {
		email = os.Getenv("ANVIL_EMAIL")
	}
	if password == "" {
		password = os.Getenv("ANVIL_PASSWORD")
	}
	if email == "" || password == "" {
		return nil, fmt.Errorf("anvilctl: --email/--password or ANVIL_EMAIL/ANVIL_PASSWORD required")
	}

	c := &apiClient{addr: addr, http: http.DefaultClient}

	token, err := c.login(ctx, email, password)
	if err != nil {
		// A fresh dev database has no users yet — register once, then
		// proceed with the same credentials, rather than requiring a
		// separate manual curl step before the very first run.
		token, err = c.register(ctx, email, password)
		if err != nil {
			return nil, fmt.Errorf("anvilctl: authenticate: %w", err)
		}
	}
	c.token = token
	return c, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

func (c *apiClient) login(ctx context.Context, email, password string) (string, error) {
	return c.authRequest(ctx, "/auth/login", email, password)
}

func (c *apiClient) register(ctx context.Context, email, password string) (string, error) {
	return c.authRequest(ctx, "/auth/register", email, password)
}

func (c *apiClient) authRequest(ctx context.Context, path, email, password string) (string, error) {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s: status %s: %s", path, resp.Status, string(b))
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", path, err)
	}
	return out.AccessToken, nil
}

// do sends an authenticated request against the control plane API and
// decodes a JSON response into out (nil to skip decoding).
func (c *apiClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, reqBody)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}
