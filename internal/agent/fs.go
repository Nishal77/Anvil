package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// workspaceRoot is fixed by SEC-001's tmpfs mount — every sandbox
// container has its writable workspace at exactly this path, not a
// per-sandbox configurable value.
const workspaceRoot = "/workspace"

const (
	resolveTimeout = 10 * time.Second
	fsOpTimeout    = 30 * time.Second
	fsListCap      = 100
	fsSearchCap    = 100
)

// resolveWorkspacePath resolves rawPath against workspaceRoot and
// returns the fully symlink-resolved absolute path, or ErrPathEscape
// if it falls outside workspaceRoot.
//
// This runs `readlink -f` INSIDE the sandbox via client/sandboxID.
// Resolving symlinks against the control plane's own filesystem would
// resolve nothing real — the sandbox's filesystem is a different
// filesystem entirely. filepath.Clean alone is purely lexical and
// knows nothing about a symlink a malicious agent planted inside the
// workspace (e.g. /workspace/link -> /etc): Clean would leave
// ".../link/passwd" untouched, a naive prefix check would pass it,
// and the read would land on /etc/passwd.
func resolveWorkspacePath(ctx context.Context, client sandboxClient, sandboxID, rawPath string) (string, error) {
	candidate := rawPath
	if path.IsAbs(candidate) {
		candidate = path.Clean(candidate)
	} else {
		candidate = path.Join(workspaceRoot, candidate)
	}

	// readlink -f resolves every symlink component in the path and
	// does not require the final component to exist — a fs_write
	// target legitimately may not exist yet, only its parent
	// directories need to resolve.
	cmd := "readlink -f -- " + shellQuote(candidate)
	res, err := runInSandbox(ctx, client, sandboxID, cmd, resolveTimeout)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("resolve workspace path: readlink: %s", strings.TrimSpace(string(res.Stderr)))
	}
	resolved := strings.TrimSpace(string(res.Stdout))

	if resolved != workspaceRoot && !strings.HasPrefix(resolved, workspaceRoot+"/") {
		return "", fmt.Errorf("%w: %q resolves to %q", ErrPathEscape, rawPath, resolved)
	}
	return resolved, nil
}

// shellQuote wraps s in single quotes for safe interpolation into a
// shell command, escaping any single quote already in s.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewFSTools returns fs_read, fs_write, fs_list, fs_search bound to
// client — the four SAFE (workspace-scoped) tools of PRD §12.2.
func NewFSTools(client sandboxClient) []Tool {
	return []Tool{fsReadTool(client), fsWriteTool(client), fsListTool(client), fsSearchTool(client)}
}

func fsReadTool(client sandboxClient) Tool {
	return Tool{
		Name:        "fs_read",
		Description: "Read a file's contents, line-numbered, optionally restricted to a line range.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the workspace root, e.g. \"app/main.go\" — NOT \"workspace/app/main.go\". An absolute path starting with /workspace also works."},
				"start_line": {"type": "integer", "minimum": 1},
				"end_line": {"type": "integer", "minimum": 1}
			},
			"required": ["path"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(ctx context.Context, sandboxID string, args json.RawMessage) (string, error) {
			var in struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: fs_read: decode args: %w", err)
			}
			resolved, err := resolveWorkspacePath(ctx, client, sandboxID, in.Path)
			if err != nil {
				return "", err
			}

			start, end := in.StartLine, in.EndLine
			if start == 0 {
				start = 1
			}
			if end == 0 {
				end = 1 << 30
			}
			// No "--" before the filename: the image's awk (mawk) does
			// not treat it as an end-of-options marker the way GNU awk
			// does and instead tries to open a file literally named
			// "--", failing every call. Resolved paths are always
			// absolute (workspaceRoot-rooted), so there is no
			// leading-dash ambiguity for awk to misparse without it.
			cmd := fmt.Sprintf(`awk 'NR>=%d && NR<=%d {printf "%%6d\t%%s\n", NR, $0}' %s`, start, end, shellQuote(resolved))
			res, err := runInSandbox(ctx, client, sandboxID, cmd, fsOpTimeout)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return fmt.Sprintf("fs_read failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return string(res.Stdout), nil
		},
	}
}

func fsWriteTool(client sandboxClient) Tool {
	return Tool{
		Name:        "fs_write",
		Description: "Write a file's complete contents, creating parent directories as needed.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the workspace root, e.g. \"app/main.go\" — NOT \"workspace/app/main.go\". An absolute path starting with /workspace also works."},
				"content": {"type": "string"}
			},
			"required": ["path", "content"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(ctx context.Context, sandboxID string, args json.RawMessage) (string, error) {
			var in struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: fs_write: decode args: %w", err)
			}
			resolved, err := resolveWorkspacePath(ctx, client, sandboxID, in.Path)
			if err != nil {
				return "", err
			}

			encoded := base64.StdEncoding.EncodeToString([]byte(in.Content))
			cmd := fmt.Sprintf("mkdir -p -- %s && echo %s | base64 -d > %s",
				shellQuote(path.Dir(resolved)), encoded, shellQuote(resolved))
			res, err := runInSandbox(ctx, client, sandboxID, cmd, fsOpTimeout)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return fmt.Sprintf("fs_write failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
		},
	}
}

func fsListTool(client sandboxClient) Tool {
	return Tool{
		Name:        "fs_list",
		Description: "List files and directories under path as a tree, up to depth (default 3).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Path relative to the workspace root, e.g. \"app\" or \".\" — NOT \"workspace/app\". An absolute path starting with /workspace also works."},
				"depth": {"type": "integer", "minimum": 1, "maximum": 10}
			},
			"required": ["path"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(ctx context.Context, sandboxID string, args json.RawMessage) (string, error) {
			var in struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: fs_list: decode args: %w", err)
			}
			resolved, err := resolveWorkspacePath(ctx, client, sandboxID, in.Path)
			if err != nil {
				return "", err
			}
			depth := in.Depth
			if depth == 0 {
				depth = 3
			}
			cmd := fmt.Sprintf("find -- %s -maxdepth %d | sort | head -n %d", shellQuote(resolved), depth, fsListCap)
			res, err := runInSandbox(ctx, client, sandboxID, cmd, fsOpTimeout)
			if err != nil {
				return "", err
			}
			if res.ExitCode != 0 {
				return fmt.Sprintf("fs_list failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return string(res.Stdout), nil
		},
	}
}

func fsSearchTool(client sandboxClient) Tool {
	return Tool{
		Name:        "fs_search",
		Description: "Search file contents for a ripgrep pattern under path (default workspace root), capped at 100 matches.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string"},
				"path": {"type": "string", "description": "Path relative to the workspace root, e.g. \"app\" — NOT \"workspace/app\". Defaults to the workspace root if omitted."}
			},
			"required": ["pattern"]
		}`),
		PolicyClass: PolicySafe,
		Handler: func(ctx context.Context, sandboxID string, args json.RawMessage) (string, error) {
			var in struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("agent: fs_search: decode args: %w", err)
			}
			searchPath := in.Path
			if searchPath == "" {
				searchPath = workspaceRoot
			}
			resolved, err := resolveWorkspacePath(ctx, client, sandboxID, searchPath)
			if err != nil {
				return "", err
			}
			cmd := fmt.Sprintf("rg --no-heading --line-number -- %s %s | head -n %d",
				shellQuote(in.Pattern), shellQuote(resolved), fsSearchCap)
			res, err := runInSandbox(ctx, client, sandboxID, cmd, fsOpTimeout)
			if err != nil {
				return "", err
			}
			// rg exits 1 for "no matches", which is a normal empty
			// result, not a failure the model needs to see as an error.
			if res.ExitCode > 1 {
				return fmt.Sprintf("fs_search failed: %s", strings.TrimSpace(string(res.Stderr))), nil
			}
			return string(res.Stdout), nil
		},
	}
}
