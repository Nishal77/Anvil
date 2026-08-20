package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

func TestPreviewImageTag(t *testing.T) {
	t.Parallel()
	if got := previewImageTag("abc-123"); got != "anvil-preview-abc-123" {
		t.Errorf("previewImageTag(%q) = %q, want %q", "abc-123", got, "anvil-preview-abc-123")
	}
}

// TestReadBuildOutput_CleanStreamSucceeds proves a normal build log —
// no "error" field on any message — is not mistaken for a failure.
func TestReadBuildOutput_CleanStreamSucceeds(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(`{"stream":"Step 1/3 : FROM busybox\n"}
{"stream":"Step 2/3 : COPY . .\n"}
{"stream":"Successfully built abc123\n"}
`)
	if err := readBuildOutput(body); err != nil {
		t.Errorf("readBuildOutput() error = %v, want nil for a stream with no error field", err)
	}
}

// TestReadBuildOutput_ErrorFieldFails is the exact failure mode
// buildPreviewImage's doc comment describes: Docker's build API keeps
// streaming HTTP 200 even after the build itself has failed, so the
// only signal is an "error" field appearing on one of the messages.
func TestReadBuildOutput_ErrorFieldFails(t *testing.T) {
	t.Parallel()
	body := strings.NewReader(`{"stream":"Step 1/2 : FROM this-does-not-exist\n"}
{"errorDetail":{"message":"pull access denied"},"error":"pull access denied"}
`)
	err := readBuildOutput(body)
	if err == nil {
		t.Fatal("readBuildOutput() error = nil, want an error for a stream carrying an error field")
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("error = %v, want it to include the build's own error message", err)
	}
}

func TestReadBuildOutput_MalformedJSONFails(t *testing.T) {
	t.Parallel()
	body := strings.NewReader("not json at all")
	if err := readBuildOutput(body); err == nil {
		t.Fatal("readBuildOutput() error = nil, want an error for a malformed stream")
	}
}

// buildContextTarGz builds an in-memory build context containing just
// a Dockerfile with the given content.
func buildContextTarGz(t *testing.T, dockerfile string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(dockerfile)), Mode: 0o644}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// busyboxHTTPDDockerfile serves a fixed response on previewExposedPort
// using only busybox's built-in httpd — no external image pull beyond
// busybox itself, which is tiny and typically already cached.
const busyboxHTTPDDockerfile = `FROM busybox:latest
RUN mkdir -p /www && echo "anvil preview ok" > /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
`

func newTestDockerClient(t *testing.T) *client.Client {
	t.Helper()
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("construct docker client: %v", err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Errorf("cleanup: close docker client: %v", err)
		}
	})
	return docker
}

// TestBuildPreviewImage_RunPreviewContainer_ServesTraffic is the
// end-to-end proof for FR-060/FR-061: a build context is built into a
// tagged image, run as a hardened container, and the result is
// actually reachable over HTTP on the published host port — not just
// "no error returned."
func TestBuildPreviewImage_RunPreviewContainer_ServesTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker := newTestDockerClient(t)
	ctx := context.Background()
	if err := ensurePreviewNetwork(ctx, docker); err != nil {
		t.Fatalf("ensurePreviewNetwork: %v", err)
	}

	jobID := uuid.NewString()
	var containerID string
	t.Cleanup(func() {
		if err := destroyPreview(context.Background(), docker, jobID, containerID); err != nil {
			t.Errorf("cleanup: destroyPreview: %v", err)
		}
	})

	buildCtx := bytes.NewReader(buildContextTarGz(t, busyboxHTTPDDockerfile))
	if err := buildPreviewImage(ctx, docker, jobID, buildCtx); err != nil {
		t.Fatalf("buildPreviewImage: %v", err)
	}

	var hostPort int
	containerID, hostPort = buildAndVerifyPreview(t, docker, jobID)

	if err := destroyPreview(ctx, docker, jobID, containerID); err != nil {
		t.Fatalf("destroyPreview: %v", err)
	}
	assertPreviewUnreachable(t, hostPort)
}

// buildAndVerifyPreview runs jobID's already-built image and confirms
// it actually serves the fixed page over HTTP — split out of the test
// function that calls it purely to keep that function's branching
// within CLAUDE.md's cyclomatic-complexity limit.
func buildAndVerifyPreview(t *testing.T, docker *client.Client, jobID string) (containerID string, hostPort int) {
	t.Helper()
	containerID, hostPort, err := runPreviewContainer(context.Background(), docker, jobID)
	if err != nil {
		t.Fatalf("runPreviewContainer: %v", err)
	}
	if containerID == "" {
		t.Fatal("runPreviewContainer returned an empty container ID")
	}
	if hostPort <= 0 {
		t.Fatalf("hostPort = %d, want a positive published port", hostPort)
	}

	body := fetchWithRetry(t, fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
	if body != "anvil preview ok\n" {
		t.Errorf("response body = %q, want the container's fixed page", body)
	}
	return containerID, hostPort
}

// assertPreviewUnreachable confirms nothing answers on hostPort
// anymore — the state that must hold immediately after destroyPreview.
func assertPreviewUnreachable(t *testing.T, hostPort int) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", hostPort), nil)
	if err != nil {
		t.Fatalf("build post-destroy request: %v", err)
	}
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Error("preview still reachable after destroyPreview")
	}
}

// fetchWithRetry polls url briefly via a ticker (CLAUDE.md T4 — no
// time.Sleep in tests) — the container needs a moment after
// ContainerStart before its process is actually listening.
func fetchWithRetry(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read response body: %v", readErr)
			}
			return string(body)
		}
		lastErr = err

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("never got a successful response from %s: %v", url, lastErr)
		}
	}
}

func TestBuildPreviewImage_BadDockerfileFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker := newTestDockerClient(t)
	jobID := uuid.NewString()
	buildCtx := bytes.NewReader(buildContextTarGz(t, "FROM this-image-does-not-exist-anywhere:latest\n"))

	err := buildPreviewImage(context.Background(), docker, jobID, buildCtx)
	if err == nil {
		t.Fatal("buildPreviewImage() error = nil, want an error for a Dockerfile referencing a nonexistent base image")
	}
}

func TestDestroyPreview_MissingContainerIsNotAnError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	t.Parallel()

	docker := newTestDockerClient(t)
	if err := destroyPreview(context.Background(), docker, "no-such-job", "no-such-container"); err != nil {
		t.Errorf("destroyPreview() error = %v, want nil for an already-gone preview", err)
	}
}
