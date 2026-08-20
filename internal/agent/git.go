package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	gitOpTimeout   = 30 * time.Second
	gitPushTimeout = 180 * time.Second
)

// gitCommitterEmail and gitCommitterName identify every commit
// git_commit makes — the agent acts as itself, not as the user it's
// working for, which PRD doesn't specify otherwise.
const (
	gitCommitterEmail = "agent@anvil.dev"
	gitCommitterName  = "Anvil Agent"
)

// githubSecretName is the well-known secret name a linked GitHub
// account's access token is stored under. Must match
// internal/auth/github.go's own constant of the same name — agent
// cannot import auth (CLAUDE.md PK5's dependency graph: agent depends
// on llm, sandbox, storage, events only), so this is intentionally a
// duplicated literal, not a shared symbol.
const githubSecretName = "GITHUB_TOKEN"

const defaultGitHubAPIBaseURL = "https://api.github.com"

// secretResolver is declared at the consumer (CODE-STANDARDS §3.1):
// *auth.Service satisfies this structurally without agent importing
// auth. Wired at construction in cmd/anvil/main.go, which is free to
// import both packages.
type secretResolver interface {
	ResolveSecret(ctx context.Context, userID uuid.UUID, name string) (string, error)
}

// NewGitCommitTool returns the SAFE git_commit tool bound to client —
// it never leaves the sandbox, so it needs no credential.
func NewGitCommitTool(client sandboxClient) Tool {
	return Tool{
		Name:        "git_commit",
		Description: "Stage all changes in the workspace and create a git commit.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"message": {"type": "string"}
			},
			"required": ["message"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(ctx context.Context, sandboxID string, _ uuid.UUID, args json.RawMessage) (string, error) {
			var in struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: git_commit: decode args: %w", err)
			}

			cmd := fmt.Sprintf("git add -A && git -c user.email=%s -c user.name=%s commit -m %s --quiet && git rev-parse HEAD",
				shellQuote(gitCommitterEmail), shellQuote(gitCommitterName), shellQuote(in.Message))
			res, err := runInSandbox(ctx, client, sandboxID, cmd, gitOpTimeout)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return fmt.Sprintf("git commit failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return fmt.Sprintf("committed %s: %s", strings.TrimSpace(string(res.Stdout)), in.Message), nil
		},
	}
}

// NewGitPushTool returns the PRIVILEGED git_push tool. httpClient nil
// defaults to http.DefaultClient; githubAPIBaseURL "" defaults to
// https://api.github.com — both overridable so tests never make a
// real network call (CLAUDE.md T3's analog for a non-LLM external
// API).
//
// On first push in a sandbox (no "origin" remote configured yet), it
// creates a fresh private GitHub repository under the token's account
// and sets it as origin — this is what makes "scoped to a repository
// Anvil created" (PRD §16.5) true by construction: the repo name is
// generated here, never taken from the model's tool call arguments,
// so there is no path from an LLM-controlled string to an existing
// user repo. A later git_push call in the same sandbox reuses the
// already-configured origin instead of creating a second repository.
//
// Known limitation, not solved here: if the sandbox is recreated after
// a crash (ADR-012 — sandbox filesystem is ephemeral tmpfs, unlike the
// database-backed job/step/turn state that crash recovery actually
// relies on), the new sandbox has no origin remote, and a retried
// git_push creates a second repository rather than rediscovering the
// first. Acceptable for v1; revisit if this proves disruptive in
// practice.
func NewGitPushTool(client sandboxClient, secrets secretResolver, httpClient *http.Client, githubAPIBaseURL string) Tool {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if githubAPIBaseURL == "" {
		githubAPIBaseURL = defaultGitHubAPIBaseURL
	}

	return Tool{
		Name:        "git_push",
		Description: "Push the current branch to GitHub. Creates a new private repository automatically on the first push. Requires the user to have linked a GitHub account.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"branch": {"type": "string"}
			},
			"required": ["branch"]
		}`),
		PolicyClass: PolicyPrivileged,
		Handler: func(ctx context.Context, sandboxID string, userID uuid.UUID, args json.RawMessage) (string, error) {
			var in struct {
				Branch string `json:"branch"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: git_push: decode args: %w", err)
			}

			token, err := secrets.ResolveSecret(ctx, userID, githubSecretName)
			if err != nil {
				return "git_push failed: no GitHub account linked for this user — connect one via /auth/github before pushing", nil
			}

			if obs, err := ensureGitHubOrigin(ctx, client, sandboxID, httpClient, githubAPIBaseURL, token); err != nil {
				return "", err
			} else if obs != "" {
				return obs, nil
			}

			res, err := gitPushWithCredential(ctx, client, sandboxID, token, in.Branch)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return fmt.Sprintf("git_push failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return fmt.Sprintf("pushed branch %s", in.Branch), nil
		},
	}
}

// githubDefaultBaseBranch is the base branch every github_open_pr call
// opens against. GitHub creates new repositories (createGitHubRepo)
// with "main" as the default branch, so this is not independently
// configurable — there is exactly one branch it could be.
const githubDefaultBaseBranch = "main"

// NewGitHubOpenPRTool returns the PRIVILEGED github_open_pr tool.
// Every tool call is already wrapped in callIdempotent by
// Executor.dispatch (turn.go) before reaching this Handler — a replay
// of the exact same (job, step, args) after a successful open returns
// the cached PR URL instead of calling GitHub's API a second time
// (PRD §14.2(d), §14.3 row 5), with no special-casing needed here.
func NewGitHubOpenPRTool(client sandboxClient, secrets secretResolver, httpClient *http.Client, githubAPIBaseURL string) Tool {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if githubAPIBaseURL == "" {
		githubAPIBaseURL = defaultGitHubAPIBaseURL
	}

	return Tool{
		Name:        "github_open_pr",
		Description: "Open a pull request on the workspace's GitHub repository. Requires a prior git_push to have created the repository and pushed the branch.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string"},
				"body": {"type": "string"},
				"branch": {"type": "string"}
			},
			"required": ["title", "body", "branch"]
		}`),
		PolicyClass: PolicyPrivileged,
		Handler: func(ctx context.Context, sandboxID string, userID uuid.UUID, args json.RawMessage) (string, error) {
			var in struct {
				Title  string `json:"title"`
				Body   string `json:"body"`
				Branch string `json:"branch"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: github_open_pr: decode args: %w", err)
			}

			token, err := secrets.ResolveSecret(ctx, userID, githubSecretName)
			if err != nil {
				return "github_open_pr failed: no GitHub account linked for this user — connect one via /auth/github before opening a PR", nil
			}

			origin, err := runInSandbox(ctx, client, sandboxID, "git remote get-url origin", gitOpTimeout)
			if err != nil {
				return "", err
			}
			if origin.ExitCode != 0 {
				return "github_open_pr failed: no GitHub repository configured for this workspace — call git_push first", nil
			}
			owner, repo, err := parseGitHubOwnerRepo(strings.TrimSpace(string(origin.Stdout)))
			if err != nil {
				return fmt.Sprintf("github_open_pr failed: %s", err.Error()), nil
			}

			prURL, err := openGitHubPR(ctx, httpClient, githubAPIBaseURL, token, owner, repo, in.Title, in.Body, in.Branch)
			if err != nil {
				return fmt.Sprintf("github_open_pr failed: %s", err.Error()), nil
			}
			return fmt.Sprintf("opened pull request: %s", prURL), nil
		},
	}
}

// parseGitHubOwnerRepo extracts "owner", "repo" from an HTTPS GitHub
// clone URL of the form https://github.com/<owner>/<repo>.git.
func parseGitHubOwnerRepo(remoteURL string) (owner, repo string, err error) {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", "", fmt.Errorf("parse remote url: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("unexpected remote url shape: %q", remoteURL)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

type githubCreatePRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

type githubCreatePRResponse struct {
	HTMLURL string `json:"html_url"`
}

// openGitHubPR opens a pull request via GitHub's REST API and returns
// its web URL.
func openGitHubPR(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo, title, body, head string) (string, error) {
	reqBody, err := json.Marshal(githubCreatePRRequest{Title: title, Body: body, Head: head, Base: githubDefaultBaseBranch})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls", baseURL, owner, repo), bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}

	var out githubCreatePRResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.HTMLURL == "" {
		return "", errors.New("github response had no html_url")
	}
	return out.HTMLURL, nil
}

// ensureGitHubOrigin makes sure the sandbox's "origin" remote exists,
// creating a fresh GitHub repository if it doesn't. Returns a non-empty
// observation string (and nil error) if it fails in a way the model
// should see and can't retry past; a nil error and empty observation
// means the caller should proceed to push.
func ensureGitHubOrigin(ctx context.Context, client sandboxClient, sandboxID string, httpClient *http.Client, githubAPIBaseURL, token string) (string, error) {
	hasOrigin, err := runInSandbox(ctx, client, sandboxID, "git remote get-url origin", gitOpTimeout)
	if err != nil {
		return "", err
	}
	if hasOrigin.ExitCode == 0 {
		return "", nil // already configured, from an earlier git_push in this same sandbox
	}

	cloneURL, err := createGitHubRepo(ctx, httpClient, githubAPIBaseURL, token)
	if err != nil {
		return fmt.Sprintf("git_push failed: create repository: %s", err.Error()), nil
	}

	addRemote, err := runInSandbox(ctx, client, sandboxID, "git remote add origin "+shellQuote(cloneURL), gitOpTimeout)
	if err != nil {
		return "", err
	}
	if addRemote.ExitCode != 0 {
		return fmt.Sprintf("git_push failed: set remote: %s", strings.TrimSpace(string(addRemote.Stderr))), nil
	}
	return "", nil
}

type githubCreateRepoResponse struct {
	CloneURL string `json:"clone_url"`
}

// createGitHubRepo creates a new private repository under token's
// account, named randomly (never from model-controlled input — see
// NewGitPushTool's doc comment) and returns its HTTPS clone URL.
func createGitHubRepo(ctx context.Context, httpClient *http.Client, baseURL, token string) (string, error) {
	name := "anvil-" + uuid.NewString()[:8]
	body, err := json.Marshal(map[string]any{"name": name, "private": true})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/user/repos", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}

	var out githubCreateRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.CloneURL == "" {
		return "", errors.New("github response had no clone_url")
	}
	return out.CloneURL, nil
}

// gitPushWithCredential runs `git push` with token injected via a
// named pipe a git credential helper reads from — never an env var,
// never a file on disk (SEC-020, PRD §16.5). The pipe is a FIFO
// (mkfifo): writeStdin's bytes flow straight from the control plane's
// connection to whatever reads the FIFO, never through the container's
// persisted filesystem layer.
func gitPushWithCredential(ctx context.Context, client sandboxClient, sandboxID, token, branch string) (execResult, error) {
	pipePath := "/tmp/.anvil-cred-" + uuid.NewString()

	mk, err := runInSandbox(ctx, client, sandboxID, "mkfifo "+pipePath, gitOpTimeout)
	if err != nil {
		return execResult{}, err
	}
	if mk.ExitCode != 0 {
		return execResult{}, fmt.Errorf("agent: git_push: create credential pipe: %s", strings.TrimSpace(string(mk.Stderr)))
	}
	//nolint:contextcheck // reason: cleanup must run even if ctx is already done
	defer func() {
		// Best-effort: an unread FIFO left behind is inert (no data
		// persisted, nothing to leak) and the sandbox is destroyed at
		// job end regardless.
		_, _ = runInSandbox(context.Background(), client, sandboxID, "rm -f "+pipePath, gitOpTimeout)
	}()

	// pipePath is generated by this function (a UUID, no shell
	// metacharacters), so it's safe to inline directly into both the
	// credential helper body and the mkfifo/rm commands above without
	// quoting.
	helper := fmt.Sprintf(`!f() { cat %s; }; f`, pipePath)

	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- client.WriteFile(ctx, sandboxID, pipePath, []byte("username=x-access-token\npassword="+token+"\n"))
	}()

	cmd := fmt.Sprintf("git -c credential.helper=%s push -u origin %s", shellQuote(helper), shellQuote(branch))
	res, pushErr := runInSandbox(ctx, client, sandboxID, cmd, gitPushTimeout)

	// Waited for, not fire-and-forget: WriteFile's own timeout
	// (sandbox/client.go's writeFileTimeout) bounds this regardless of
	// whether the helper ever actually read the pipe (e.g. push failed
	// before reaching authentication), so this can't hang the tool
	// call. A write error here is only interesting if the push itself
	// also failed — if push succeeded, the helper necessarily already
	// read the credential, and this is just the writer losing a benign
	// race against an already-satisfied FIFO.
	if writeErr := <-writeErrCh; writeErr != nil && pushErr == nil && res.ExitCode != 0 {
		return res, fmt.Errorf("agent: git_push: write credential: %w", writeErr)
	}
	return res, pushErr
}
