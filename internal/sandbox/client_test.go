package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// buildTestTarGz builds a real gzipped tar archive in memory — the
// shape ExportWorkspace's verification step (verifyTarGz) requires,
// unlike arbitrary bytes.
func buildTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{RunnerAddr: srv.URL, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() error = nil, want an error for a completely empty Config")
	}
}

func TestClient_Create_ReturnsSandboxID(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(CreateResponse{SandboxID: "sb-1"})
	})

	id, err := c.Create(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "sb-1" {
		t.Errorf("id = %q, want %q", id, "sb-1")
	}
}

func TestClient_Create_NonCreatedStatusIsError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.Create(context.Background(), uuid.New()); err == nil {
		t.Fatal("Create() error = nil, want an error on a non-201 response")
	}
}

func TestClient_Destroy_NotFoundIsNotAnError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.Destroy(context.Background(), "gone"); err != nil {
		t.Errorf("Destroy() error = %v, want nil — destroying an already-gone sandbox is not a failure", err)
	}
}

func TestClient_Exec_StreamsChunksInOrder(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte("hello ")})
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte("world")})
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Final: true, ExitCode: 0})
		flusher.Flush()
	})

	var got []byte
	var sawFinal bool
	err := c.Exec(context.Background(), "sb-1", "echo hi", time.Second, func(chunk ExecChunk) {
		if chunk.Final {
			sawFinal = true
			return
		}
		got = append(got, chunk.Data...)
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("collected output = %q, want %q", got, "hello world")
	}
	if !sawFinal {
		t.Error("never saw the final chunk")
	}
}

func TestClient_Exec_NotFoundReturnsErrSandboxNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := c.Exec(context.Background(), "gone", "echo hi", time.Second, func(ExecChunk) {})
	if err == nil {
		t.Fatal("Exec() error = nil, want ErrSandboxNotFound")
	}
}

// TestClient_ExportWorkspace_DecodesBase64Payload proves ExportWorkspace
// reassembles a base64 payload the Runner streamed across multiple
// stdout chunks (exactly what a real multi-line `base64` command
// output looks like) into the original bytes.
func TestClient_ExportWorkspace_DecodesBase64Payload(t *testing.T) {
	original := buildTestTarGz(t, map[string]string{"main.go": "package main"})
	encoded := base64.StdEncoding.EncodeToString(original)
	// Split the encoded payload into two chunks, as if the Runner's
	// line-oriented scanner had split it across two output lines — the
	// exact scenario ExportWorkspace's doc comment explains.
	mid := len(encoded) / 2

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte(encoded[:mid])})
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte(encoded[mid:])})
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Final: true, ExitCode: 0})
		flusher.Flush()
	})

	r, err := c.ExportWorkspace(context.Background(), "sb-1")
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read exported workspace: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("decoded content = %q, want %q", got, original)
	}
}

// TestClient_ExportWorkspace_NonzeroExitIsError proves a failed
// pipeline is reported as an error, not silently accepted as a
// complete (but truncated) archive. Found live: without `set -o
// pipefail`, a `tar | base64` pipeline's exit code is base64's alone
// — tar dying partway through still lets base64 exit 0 on whatever
// partial input it received, producing valid-looking, truncated
// base64 that decodes without error. This test proves the exit code
// is checked at all, independent of whatever specific reason a real
// tar failure would have.
func TestClient_ExportWorkspace_NonzeroExitIsError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		// A syntactically valid (if truncated) base64 payload — the
		// failure mode this test exists to catch is exactly a decode
		// that would otherwise succeed.
		_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte(base64.StdEncoding.EncodeToString([]byte("partial")))})
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Final: true, ExitCode: 1})
		flusher.Flush()
	})

	_, err := c.ExportWorkspace(context.Background(), "sb-1")
	if err == nil {
		t.Fatal("ExportWorkspace() error = nil, want an error on a nonzero exit code")
	}
}

// TestClient_ExportWorkspace_RetriesOnCorruptionThenSucceeds proves a
// transient corruption on one attempt doesn't fail the whole export —
// exactly the intermittent, not-fully-root-caused failure mode found
// live (see exportWorkspaceAttempts's doc comment): the second attempt
// re-runs the export from scratch and succeeds.
func TestClient_ExportWorkspace_RetriesOnCorruptionThenSucceeds(t *testing.T) {
	good := buildTestTarGz(t, map[string]string{"main.go": "package main"})
	goodEncoded := base64.StdEncoding.EncodeToString(good)

	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		if calls == 1 {
			// Not valid base64 at all — the "illegal base64 data"
			// failure mode found live, not just a clean truncation.
			_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte("!!!not-base64!!!")})
		} else {
			_ = enc.Encode(ExecChunk{Stream: "stdout", Data: []byte(goodEncoded)})
		}
		flusher.Flush()
		_ = enc.Encode(ExecChunk{Final: true, ExitCode: 0})
		flusher.Flush()
	})

	r, err := c.ExportWorkspace(context.Background(), "sb-1")
	if err != nil {
		t.Fatalf("ExportWorkspace() error = %v, want the second attempt to succeed", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read exported workspace: %v", err)
	}
	if !bytes.Equal(got, good) {
		t.Error("decoded content did not match the good archive from the retried attempt")
	}
	if calls != 2 {
		t.Errorf("Exec called %d times, want 2 (one failure, one success)", calls)
	}
}
