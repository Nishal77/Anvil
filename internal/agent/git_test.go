package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
)

type fakeSecretResolver struct {
	token string
	err   error
}

func (f *fakeSecretResolver) ResolveSecret(context.Context, uuid.UUID, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

func TestGitCommitTool_Success(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "abc123def\n", "", 0 }
	tool := NewGitCommitTool(sb)

	args, _ := json.Marshal(map[string]string{"message": "add feature"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if !strings.Contains(obs, "abc123def") || !strings.Contains(obs, "add feature") {
		t.Errorf("observation = %q, want it to mention the sha and message", obs)
	}
}

func TestGitCommitTool_NothingToCommitReportedNotFatal(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "", "nothing to commit, working tree clean", 1 }
	tool := NewGitCommitTool(sb)

	args, _ := json.Marshal(map[string]string{"message": "no-op"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v, want a correctable observation, not a harness error", err)
	}
	if !strings.Contains(obs, "nothing to commit") {
		t.Errorf("observation = %q, want it to surface the git error", obs)
	}
}

func TestGitPushTool_NoTokenLinkedReportedNotFatal(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	secrets := &fakeSecretResolver{err: errors.New("secret not found")}
	tool := NewGitPushTool(sb, secrets, nil, "")

	args, _ := json.Marshal(map[string]string{"branch": "main"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v, want a correctable observation", err)
	}
	if !strings.Contains(obs, "no GitHub account linked") {
		t.Errorf("observation = %q, want a clear no-linked-account message", obs)
	}
	if len(sb.commands) != 0 {
		t.Errorf("no sandbox command should run before a token is available, got %v", sb.commands)
	}
}

// TestGitPushTool_CreatesRepoWhenNoOriginConfigured proves the
// repo-creation path fires on a fresh sandbox (no "origin" remote) and
// never on a sandbox that already has one — this is what makes
// "scoped to a repository Anvil created" true (git.go's doc comment).
func TestGitPushTool_CreatesRepoWhenNoOriginConfigured(t *testing.T) {
	t.Parallel()
	var createCalls int
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		createCalls++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubCreateRepoResponse{CloneURL: "https://github.com/octocat/anvil-abc123.git"})
	}))
	defer githubServer.Close()

	sb := newFakeSandbox()
	var gotRemoteURL string
	sb.execFunc = func(cmd string) (string, string, int) {
		if strings.HasPrefix(cmd, "git remote get-url origin") {
			return "", "no such remote", 1
		}
		if strings.HasPrefix(cmd, "git remote add origin") {
			gotRemoteURL = strings.Trim(strings.TrimPrefix(cmd, "git remote add origin "), "'")
		}
		return "", "", 0
	}

	secrets := &fakeSecretResolver{token: "test-token"}
	tool := NewGitPushTool(sb, secrets, githubServer.Client(), githubServer.URL)

	args, _ := json.Marshal(map[string]string{"branch": "main"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if !strings.Contains(obs, "pushed branch main") {
		t.Errorf("observation = %q, want a pushed confirmation", obs)
	}
	if createCalls != 1 {
		t.Errorf("github repo-create called %d times, want 1", createCalls)
	}
	if gotRemoteURL != "https://github.com/octocat/anvil-abc123.git" {
		t.Errorf("remote add origin used %q, want the created repo's clone_url", gotRemoteURL)
	}
}

func TestGitPushTool_ReusesExistingOrigin(t *testing.T) {
	t.Parallel()
	var createCalls int
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		createCalls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubCreateRepoResponse{CloneURL: "https://github.com/octocat/should-not-be-created.git"})
	}))
	defer githubServer.Close()

	sb := newFakeSandbox()
	sb.execFunc = func(cmd string) (string, string, int) {
		if strings.HasPrefix(cmd, "git remote get-url origin") {
			return "https://github.com/octocat/existing.git\n", "", 0
		}
		return "", "", 0
	}

	secrets := &fakeSecretResolver{token: "test-token"}
	tool := NewGitPushTool(sb, secrets, githubServer.Client(), githubServer.URL)

	args, _ := json.Marshal(map[string]string{"branch": "main"})
	if _, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if createCalls != 0 {
		t.Errorf("github repo-create called %d times, want 0 — origin already existed", createCalls)
	}
}

// TestGitPushTool_WritesCredentialToPipe proves the token reaches the
// sandbox only through WriteFile (the named-pipe path), targeting the
// exact path mkfifo created — never as part of any exec'd command
// string, which is what SEC-020 requires.
func TestGitPushTool_WritesCredentialToPipe(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	var mkfifoPath string
	sb.execFunc = func(cmd string) (string, string, int) {
		switch {
		case strings.HasPrefix(cmd, "git remote get-url origin"):
			return "https://github.com/octocat/existing.git\n", "", 0
		case strings.HasPrefix(cmd, "mkfifo "):
			mkfifoPath = strings.TrimPrefix(cmd, "mkfifo ")
			return "", "", 0
		default:
			return "", "", 0
		}
	}

	secrets := &fakeSecretResolver{token: "s3cr3t-token"}
	tool := NewGitPushTool(sb, secrets, nil, "")

	args, _ := json.Marshal(map[string]string{"branch": "main"})
	if _, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}

	if len(sb.writes) != 1 {
		t.Fatalf("WriteFile called %d times, want 1", len(sb.writes))
	}
	if sb.writes[0].path != mkfifoPath {
		t.Errorf("WriteFile path = %q, want the mkfifo'd path %q", sb.writes[0].path, mkfifoPath)
	}
	if !strings.Contains(string(sb.writes[0].data), "s3cr3t-token") {
		t.Errorf("WriteFile data = %q, want it to contain the resolved token", sb.writes[0].data)
	}
	for _, cmd := range sb.commands {
		if strings.Contains(cmd, "s3cr3t-token") {
			t.Errorf("token leaked into an exec'd command: %q", cmd)
		}
	}
}

func TestGitHubOpenPRTool_Success(t *testing.T) {
	t.Parallel()
	var gotBody githubCreatePRRequest
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octocat/anvil-abc123/pulls" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubCreatePRResponse{HTMLURL: "https://github.com/octocat/anvil-abc123/pull/1"})
	}))
	defer githubServer.Close()

	sb := newFakeSandbox()
	sb.execFunc = func(cmd string) (string, string, int) {
		if strings.HasPrefix(cmd, "git remote get-url origin") {
			return "https://github.com/octocat/anvil-abc123.git\n", "", 0
		}
		return "", "", 0
	}

	secrets := &fakeSecretResolver{token: "test-token"}
	tool := NewGitHubOpenPRTool(sb, secrets, githubServer.Client(), githubServer.URL)

	args, _ := json.Marshal(map[string]string{"title": "Add feature", "body": "Description", "branch": "feature-1"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if !strings.Contains(obs, "https://github.com/octocat/anvil-abc123/pull/1") {
		t.Errorf("observation = %q, want the PR URL", obs)
	}
	if gotBody.Title != "Add feature" || gotBody.Head != "feature-1" || gotBody.Base != "main" {
		t.Errorf("PR request = %+v, want title/head from args and base=main", gotBody)
	}
}

func TestGitHubOpenPRTool_NoOriginReportedNotFatal(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	sb.execFunc = func(string) (string, string, int) { return "", "no such remote", 1 }
	secrets := &fakeSecretResolver{token: "test-token"}
	tool := NewGitHubOpenPRTool(sb, secrets, nil, "")

	args, _ := json.Marshal(map[string]string{"title": "t", "body": "b", "branch": "br"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v, want a correctable observation", err)
	}
	if !strings.Contains(obs, "call git_push first") {
		t.Errorf("observation = %q, want it to point at git_push", obs)
	}
}

func TestParseGitHubOwnerRepo(t *testing.T) {
	t.Parallel()
	owner, repo, err := parseGitHubOwnerRepo("https://github.com/octocat/hello-world.git")
	if err != nil {
		t.Fatalf("parseGitHubOwnerRepo() error = %v", err)
	}
	if owner != "octocat" || repo != "hello-world" {
		t.Errorf("owner, repo = %q, %q, want octocat, hello-world", owner, repo)
	}
}

// TestGitHubOpenPRTool_ReplayProducesNoDuplicate is task 8.6: a
// replayed dispatch of the exact same (job, step, github_open_pr,
// args) — the shape a crash-recovered worker re-running a step
// produces — must hit GitHub's API exactly once. Executor.dispatch
// (turn.go) wraps every tool call in callIdempotent generically;
// this proves that generic wrapping actually closes the loop for
// github_open_pr specifically (PRD §14.3 row 5), not just in the
// abstract (idempotent_test.go already covers callIdempotent alone).
func TestGitHubOpenPRTool_ReplayProducesNoDuplicate(t *testing.T) {
	t.Parallel()
	var prCalls int
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prCalls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(githubCreatePRResponse{HTMLURL: "https://github.com/octocat/anvil-abc123/pull/1"})
	}))
	defer githubServer.Close()

	sb := newFakeSandbox()
	sb.execFunc = func(cmd string) (string, string, int) {
		if strings.HasPrefix(cmd, "git remote get-url origin") {
			return "https://github.com/octocat/anvil-abc123.git\n", "", 0
		}
		return "", "", 0
	}
	secrets := &fakeSecretResolver{token: "test-token"}

	registry, err := NewRegistry(NewGitHubOpenPRTool(sb, secrets, githubServer.Client(), githubServer.URL))
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	e := &Executor{registry: registry, idem: newFakeIdemStore()}

	jobID, userID, stepID := uuid.New(), uuid.New(), uuid.New()
	call := llm.ToolCall{Name: "github_open_pr", Input: json.RawMessage(`{"title":"Add feature","body":"desc","branch":"feature-1"}`)}

	obs1, err := e.dispatch(context.Background(), Allow, "", jobID, userID, stepID, "fake-sandbox", call)
	if err != nil {
		t.Fatalf("first dispatch() error = %v", err)
	}
	obs2, err := e.dispatch(context.Background(), Allow, "", jobID, userID, stepID, "fake-sandbox", call)
	if err != nil {
		t.Fatalf("replayed dispatch() error = %v", err)
	}

	if prCalls != 1 {
		t.Errorf("github PR-open endpoint called %d times, want 1 — the replay must hit the idempotency cache, not GitHub again", prCalls)
	}
	if obs1 != obs2 {
		t.Errorf("observation = %q on replay, want the identical cached result %q", obs2, obs1)
	}
}

func TestGitPushTool_PushFailureReportedNotFatal(t *testing.T) {
	t.Parallel()
	sb := newFakeSandbox()
	sb.execFunc = func(cmd string) (string, string, int) {
		switch {
		case strings.HasPrefix(cmd, "git remote get-url origin"):
			return "https://github.com/octocat/existing.git\n", "", 0
		case strings.Contains(cmd, "push -u origin"):
			return "", "authentication failed", 1
		default:
			return "", "", 0
		}
	}

	secrets := &fakeSecretResolver{token: "test-token"}
	tool := NewGitPushTool(sb, secrets, nil, "")

	args, _ := json.Marshal(map[string]string{"branch": "main"})
	obs, err := tool.Handler(context.Background(), "fake-sandbox", uuid.New(), args)
	if err != nil {
		t.Fatalf("Handler() error = %v, want a correctable observation", err)
	}
	if !strings.Contains(obs, "git_push failed") || !strings.Contains(obs, "authentication failed") {
		t.Errorf("observation = %q, want it to surface the push failure", obs)
	}
}
