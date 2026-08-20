package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"
)

// buildArchive builds an in-memory gzipped tar containing files, keyed
// by entry name.
func buildArchive(t *testing.T, files map[string]string) []byte {
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

// archiveEntries reads back every entry name and content from a
// gzipped tar, for asserting what EnsureDockerfile produced.
func archiveEntries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open gzip stream: %v", err)
	}
	defer func() { _ = gz.Close() }()

	out := make(map[string]string)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar content: %v", err)
		}
		out[header.Name] = string(content)
	}
	return out
}

func TestEnsureDockerfile_PassesThroughExistingDockerfile(t *testing.T) {
	t.Parallel()
	original := buildArchive(t, map[string]string{"Dockerfile": "FROM scratch\n", "main.go": "package main"})

	got, err := EnsureDockerfile(original)
	if err != nil {
		t.Fatalf("EnsureDockerfile() error = %v", err)
	}

	entries := archiveEntries(t, got)
	if entries["Dockerfile"] != "FROM scratch\n" {
		t.Errorf("Dockerfile content = %q, want the original passed through unmodified", entries["Dockerfile"])
	}
}

func TestEnsureDockerfile_DetectsGoModAndGenerates(t *testing.T) {
	t.Parallel()
	original := buildArchive(t, map[string]string{"go.mod": "module example.com/app\n", "main.go": "package main"})

	got, err := EnsureDockerfile(original)
	if err != nil {
		t.Fatalf("EnsureDockerfile() error = %v", err)
	}

	entries := archiveEntries(t, got)
	if entries["Dockerfile"] != goDockerfile {
		t.Errorf("Dockerfile content = %q, want the Go template", entries["Dockerfile"])
	}
	if entries["main.go"] != "package main" {
		t.Error("original main.go entry was lost when appending the generated Dockerfile")
	}
}

func TestEnsureDockerfile_DetectsPackageJSON(t *testing.T) {
	t.Parallel()
	original := buildArchive(t, map[string]string{"package.json": "{}"})

	got, err := EnsureDockerfile(original)
	if err != nil {
		t.Fatalf("EnsureDockerfile() error = %v", err)
	}
	if archiveEntries(t, got)["Dockerfile"] != nodeDockerfile {
		t.Error("want the Node template for a package.json project")
	}
}

func TestEnsureDockerfile_DetectsRequirementsTxt(t *testing.T) {
	t.Parallel()
	original := buildArchive(t, map[string]string{"requirements.txt": "flask\n"})

	got, err := EnsureDockerfile(original)
	if err != nil {
		t.Fatalf("EnsureDockerfile() error = %v", err)
	}
	if archiveEntries(t, got)["Dockerfile"] != pythonDockerfile {
		t.Error("want the Python template for a requirements.txt project")
	}
}

func TestEnsureDockerfile_NoRecognizedProjectFails(t *testing.T) {
	t.Parallel()
	original := buildArchive(t, map[string]string{"README.md": "hello"})

	_, err := EnsureDockerfile(original)
	if !errors.Is(err, ErrNoRunnableProject) {
		t.Errorf("EnsureDockerfile() error = %v, want ErrNoRunnableProject", err)
	}
}

func TestEnsureDockerfile_LeadingDotSlashDockerfileDetected(t *testing.T) {
	t.Parallel()
	// ExportWorkspace's own `tar czf - -C /workspace .` produces
	// "./Dockerfile" rather than "Dockerfile" — this must still count
	// as "already has one."
	original := buildArchive(t, map[string]string{"./Dockerfile": "FROM scratch\n"})

	got, err := EnsureDockerfile(original)
	if err != nil {
		t.Fatalf("EnsureDockerfile() error = %v", err)
	}
	if string(got) != string(original) {
		t.Error("archive with a ./Dockerfile entry was modified, want it passed through unchanged")
	}
}
