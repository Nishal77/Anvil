package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startTestMinIO boots a real MinIO container — CLAUDE.md T5. Generic
// container request, not a dedicated module: no MinIO-specific
// testcontainers module is a project dependency, and one container
// image is not worth adding one for.
func startTestMinIO(t *testing.T) Config {
	t.Helper()
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:latest",
			ExposedPorts: []string{"9000/tcp"},
			Env:          map[string]string{"MINIO_ROOT_USER": "anvil", "MINIO_ROOT_PASSWORD": "anvilanvil"},
			Cmd:          []string{"server", "/data"},
			WaitingFor:   wait.ForHTTP("/minio/health/live").WithPort("9000/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("get minio host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("get minio port: %v", err)
	}
	return Config{
		Endpoint: host + ":" + port.Port(), Bucket: "test-artifacts",
		AccessKey: "anvil", SecretKey: "anvilanvil",
	}
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("New() error = nil, want an error for a completely empty Config")
	}
}

// TestStore_UploadDownloadRoundTrip proves the real MinIO wiring: New
// creates the bucket if it doesn't exist, Upload streams an object in,
// Download streams it back byte-for-byte.
func TestStore_UploadDownloadRoundTrip(t *testing.T) {
	cfg := startTestMinIO(t)
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jobID := uuid.New()
	content := "this is a fake tar.gz archive"
	if _, err := store.Upload(context.Background(), jobID, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	r, err := store.Download(context.Background(), jobID)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read downloaded content: %v", err)
	}
	if string(got) != content {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
}

// TestStore_PresignedDownloadURL_ServesTheUploadedContent proves the
// URL PresignedDownloadURL returns is a real, directly-fetchable link
// to the uploaded object — not just a non-empty string.
func TestStore_PresignedDownloadURL_ServesTheUploadedContent(t *testing.T) {
	cfg := startTestMinIO(t)
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	jobID := uuid.New()
	content := "this is a fake tar.gz archive"
	if _, err := store.Upload(context.Background(), jobID, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	url, err := store.PresignedDownloadURL(context.Background(), jobID, time.Minute)
	if err != nil {
		t.Fatalf("PresignedDownloadURL: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build presigned url request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch presigned url: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned url returned status %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned response body: %v", err)
	}
	if string(got) != content {
		t.Errorf("presigned url content = %q, want %q", got, content)
	}
}

// TestStore_Download_NotFound proves a job with no uploaded artifact
// returns ErrNotFound, not a generic error the API layer would have to
// guess how to classify.
func TestStore_Download_NotFound(t *testing.T) {
	cfg := startTestMinIO(t)
	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = store.Download(context.Background(), uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Download() error = %v, want ErrNotFound", err)
	}
}
