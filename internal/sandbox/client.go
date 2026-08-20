package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxExecLineBytes bounds how big a single line of output from the Runner
// can be. A compiler can emit one very long line, so this needs to be
// generous — but it still needs a limit, or one runaway line could eat
// unbounded memory.
const maxExecLineBytes = 1 << 20 // 1 MiB

// Config configures a Client.
type Config struct {
	RunnerAddr string // e.g. http://127.0.0.1:9090
	Logger     *slog.Logger
	HTTPClient *http.Client // nil defaults to http.DefaultClient
}

func (c *Config) setDefaults() {
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
}

func (c Config) validate() error {
	if c.RunnerAddr == "" {
		return errors.New("sandbox: config: RunnerAddr is required")
	}
	if c.Logger == nil {
		return errors.New("sandbox: config: Logger is required")
	}
	return nil
}

// Client is the control-plane side of the sandbox protocol.
type Client struct {
	addr string
	log  *slog.Logger
	http *http.Client
}

// New constructs a Client from cfg, or returns an error if cfg is invalid.
func New(cfg Config) (*Client, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Client{addr: cfg.RunnerAddr, log: cfg.Logger, http: cfg.HTTPClient}, nil
}

// Create asks the Runner to create a sandbox for jobID and returns its ID.
func (c *Client) Create(ctx context.Context, jobID uuid.UUID) (string, error) {
	body, err := json.Marshal(CreateRequest{JobID: jobID})
	if err != nil {
		return "", fmt.Errorf("sandbox: create: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("sandbox: create: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("sandbox: create: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("sandbox: create: runner returned %s", resp.Status)
	}

	var out CreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("sandbox: create: decode response: %w", err)
	}
	return out.SandboxID, nil
}

// Exec runs command in sandboxID and invokes onChunk for each output chunk
// as it arrives over the HTTP chunked response — never buffers until the
// command exits. Exec returns once the command exits or ctx is cancelled.
func (c *Client) Exec(ctx context.Context, sandboxID, command string, timeout time.Duration, onChunk func(ExecChunk)) error {
	body, err := json.Marshal(ExecRequest{
		SandboxID: sandboxID,
		Command:   command,
		TimeoutS:  int(timeout.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("sandbox: exec: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/sandboxes/"+sandboxID+"/exec", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sandbox: exec: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: exec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox: exec: runner returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxExecLineBytes)
	for scanner.Scan() {
		var chunk ExecChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return fmt.Errorf("sandbox: exec: decode chunk: %w", err)
		}
		onChunk(chunk)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sandbox: exec: read stream: %w", err)
	}
	return nil
}

// writeFileTimeout bounds WriteFile — it only ever moves a few hundred
// bytes (a credential, SEC-020), so a short, fixed timeout is enough.
const writeFileTimeout = 15 * time.Second

// WriteFile writes data to path inside sandboxID. This is not part of
// the agent tool registry the LLM can call (RULE S1 — nothing in the
// sandbox is trusted with a general-purpose file write); it exists
// only for internal/agent's git credential-injection path (SEC-020),
// which writes a secret to a named pipe it created itself.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, data []byte) error {
	body, err := json.Marshal(WriteRequest{SandboxID: sandboxID, Path: path, Data: data})
	if err != nil {
		return fmt.Errorf("sandbox: write file: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, writeFileTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/sandboxes/"+sandboxID+"/write", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sandbox: write file: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: write file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrSandboxNotFound
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("sandbox: write file: runner returned %s", resp.Status)
	}
	return nil
}

const exportWorkspaceTimeout = 60 * time.Second

// exportWorkspaceCommand tars and gzips /workspace, then base64-encodes
// the result before printing it. Two reasons this can't be simpler:
//
//  1. /workspace is a tmpfs mount (container.go's hardening config).
//     Docker's own CopyFromContainer/`docker cp` cannot see into a
//     tmpfs mount at all — it reads the container's persisted
//     filesystem layer on the host, and tmpfs contents exist only in
//     the container's own kernel mount namespace. Running tar THROUGH
//     exec (inside that namespace) is the only thing that sees the
//     files.
//  2. The exec protocol's output streaming (scanLines, stream.go) is
//     line-oriented: it splits on '\n' and never returns the
//     delimiter, which is exactly right for human/LLM-readable command
//     output and exactly wrong for a raw binary stream — silently
//     dropping every 0x0A byte corrupts a gzip stream. base64's own
//     default line wrapping (~76 chars) keeps every line well under
//     both scanners' 1 MiB token limit (stream.go, client.go); the
//     stripped newlines are pure formatting, never part of the
//     alphabet, so simple concatenation before decoding loses nothing.
//     -w0 (no wrapping) was tried first and failed exactly this way:
//     it forces the entire archive onto one line, which is larger
//     than either scanner's buffer, so the export just breaks instead
//     of streaming.
//
// --exclude=.cache drops Go's build cache (go build/go test populate
// /workspace/.cache/go-build, which a real Go module easily makes
// tens of megabytes of object files) — it is disposable compiler
// state, not part of the deliverable, and including it was found live
// to be large enough to matter for export reliability besides.
//
// set -o pipefail is not optional here. Without it, a `tar | base64`
// pipeline's exit code is base64's alone: if tar dies partway through
// for any reason, base64 still happily encodes whatever partial bytes
// it received and exits 0 — producing perfectly valid, perfectly
// truncated base64 of a broken gzip stream, silently. This was found
// live: the exec itself reported success while the extracted artifact
// failed with "unexpected EOF" partway through a file. With pipefail,
// tar's failure becomes the pipeline's failure, which ExportWorkspace
// below now actually checks instead of trusting a decodable payload
// to mean a complete one.
const exportWorkspaceCommand = "set -o pipefail; tar czf - -C /workspace --exclude=.cache . | base64"

// exportWorkspaceAttempts is how many times ExportWorkspace retries a
// corrupted transfer before giving up. Found live, twice, even after
// fixing two confirmed contributing bugs (a tool-call ID mismatch
// unrelated to this path, and an unchecked tar failure via a
// non-pipefail pipeline): occasionally the base64 text captured over
// the exec streaming protocol is itself invalid — not just short, but
// containing bytes outside the base64 alphabet — under real load (a
// long-running compile immediately before the export command). The
// exact mechanism inside Docker's exec attach demuxing wasn't
// isolated further within reasonable effort; retrying a fresh export
// (a new tar/base64/exec run, not a resend of the same bytes) and
// verifying the result decompresses cleanly before accepting it is
// the honest mitigation for a confirmed-live, not-fully-root-caused
// intermittent transport fault, not a substitute for finding the real
// cause if it recurs enough to justify more investigation.
const exportWorkspaceAttempts = 3

// ExportWorkspace returns sandboxID's /workspace as a gzipped tar
// archive. Called before Destroy on every terminal path (SUCCEEDED,
// FAILED, CANCELLED) — a job's artifact is preserved regardless of why
// it ended (ADR-012).
func (c *Client) ExportWorkspace(ctx context.Context, sandboxID string) (io.ReadCloser, error) {
	var lastErr error
	for attempt := 1; attempt <= exportWorkspaceAttempts; attempt++ {
		decoded, err := c.exportWorkspaceOnce(ctx, sandboxID)
		if err == nil {
			return io.NopCloser(bytes.NewReader(decoded)), nil
		}
		lastErr = err
		c.log.WarnContext(ctx, "export workspace attempt failed", slog.String("sandbox_id", sandboxID), slog.Int("attempt", attempt), slog.Any("err", err))
	}
	return nil, fmt.Errorf("sandbox: export workspace: %d attempts failed, last error: %w", exportWorkspaceAttempts, lastErr)
}

// exportWorkspaceOnce runs exportWorkspaceCommand once and validates
// the result is a complete, readable gzip+tar stream before returning
// it — decodability alone is not proof of completeness (a pipeline
// that failed partway can still produce syntactically valid, merely
// truncated, base64 — the exact bug pipefail above closes one way
// into) so this reads the archive all the way through instead of
// trusting the byte count.
func (c *Client) exportWorkspaceOnce(ctx context.Context, sandboxID string) ([]byte, error) {
	var b64 strings.Builder
	var exitCode int
	err := c.Exec(ctx, sandboxID, exportWorkspaceCommand, exportWorkspaceTimeout, func(chunk ExecChunk) {
		if chunk.Stream == "stdout" {
			b64.Write(chunk.Data)
		}
		if chunk.Final {
			exitCode = chunk.ExitCode
		}
	})
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("command exited %d", exitCode)
	}

	decoded, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := verifyTarGz(decoded); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	return decoded, nil
}

// verifyTarGz reads archive all the way to its end, discarding
// content, to confirm the gzip stream and every tar entry in it are
// complete — the only way to distinguish a genuinely finished archive
// from one that merely decoded without a base64-level error.
func verifyTarGz(archive []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return fmt.Errorf("read content of %s: %w", header.Name, err)
		}
	}
}

// Destroy tears down sandboxID. Safe to call on an already-destroyed or
// unknown sandbox.
func (c *Client) Destroy(ctx context.Context, sandboxID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.addr+"/sandboxes/"+sandboxID, nil)
	if err != nil {
		return fmt.Errorf("sandbox: destroy: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: destroy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("sandbox: destroy: runner returned %s", resp.Status)
	}
	return nil
}

// BuildPreview builds buildContext (a tar or tar.gz stream with a
// Dockerfile at its root) into an image and runs it as a preview
// deployment for jobID (PRD §9.7, FR-060/FR-061). Returns the
// container ID and the host port its exposed port was published to.
func (c *Client) BuildPreview(ctx context.Context, jobID uuid.UUID, buildContext io.Reader) (BuildPreviewResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/previews/"+jobID.String(), buildContext)
	if err != nil {
		return BuildPreviewResponse{}, fmt.Errorf("sandbox: build preview: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := c.http.Do(req)
	if err != nil {
		return BuildPreviewResponse{}, fmt.Errorf("sandbox: build preview: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return BuildPreviewResponse{}, fmt.Errorf("sandbox: build preview: runner returned %s", resp.Status)
	}

	var out BuildPreviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return BuildPreviewResponse{}, fmt.Errorf("sandbox: build preview: decode response: %w", err)
	}
	return out, nil
}

// DestroyPreview tears down jobID's preview deployment. Safe to call
// on a job with no preview (never deployed, or already torn down).
func (c *Client) DestroyPreview(ctx context.Context, jobID uuid.UUID) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.addr+"/previews/"+jobID.String(), nil)
	if err != nil {
		return fmt.Errorf("sandbox: destroy preview: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: destroy preview: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("sandbox: destroy preview: runner returned %s", resp.Status)
	}
	return nil
}
