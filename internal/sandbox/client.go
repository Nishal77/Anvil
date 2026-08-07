package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
