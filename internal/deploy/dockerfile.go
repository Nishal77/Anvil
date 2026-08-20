package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// ErrNoRunnableProject means the workspace has no Dockerfile and none
// of the project markers detectProjectType recognizes — there is
// nothing to build a preview from.
var ErrNoRunnableProject = errors.New("deploy: no Dockerfile and no recognizable project type")

// dockerfileName is the exact entry name checked for and, if absent,
// generated — Docker's default build filename, and the only one
// buildPreviewImage on the Runner side looks for (its ImageBuild call
// hardcodes Dockerfile: "Dockerfile").
const dockerfileName = "Dockerfile"

// projectType is a project marker file detectProjectType recognizes,
// paired with the Dockerfile template to use when none is present.
type projectType struct {
	marker     string
	dockerfile string
}

// projectTypes is checked in order — first match wins. A project with
// both a go.mod and a package.json (a Go backend with a JS frontend
// build step, say) gets the Go template; PRD §9.7 doesn't specify
// multi-language detection, and guessing wrong in a specific way is
// more useful to a user debugging it than silently picking one with
// no explanation.
var projectTypes = []projectType{
	{"go.mod", goDockerfile},
	{"package.json", nodeDockerfile},
	{"requirements.txt", pythonDockerfile},
	{"pyproject.toml", pythonDockerfile},
}

// previewPort must match internal/sandbox/runner's previewExposedPort
// — every generated Dockerfile binds here. Duplicated as a literal
// rather than imported: internal/sandbox/runner must never be
// imported outside cmd/runner's own process (I-4's spirit — nothing
// else should even compile against Docker-adjacent internals), and
// the value is fixed and documented in both places for exactly this
// reason.
const previewPort = "8080"

const goDockerfile = `FROM golang:1.23-alpine
WORKDIR /app
COPY . .
RUN go build -o /app/server .
EXPOSE ` + previewPort + `
ENV PORT=` + previewPort + `
CMD ["/app/server"]
`

const nodeDockerfile = `FROM node:20-alpine
WORKDIR /app
COPY . .
RUN npm install
EXPOSE ` + previewPort + `
ENV PORT=` + previewPort + `
CMD ["npm", "start"]
`

const pythonDockerfile = `FROM python:3.12-slim
WORKDIR /app
COPY . .
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; else pip install --no-cache-dir .; fi
EXPOSE ` + previewPort + `
ENV PORT=` + previewPort + `
CMD ["python", "main.py"]
`

// EnsureDockerfile returns a build context tar.gz guaranteed to
// contain a Dockerfile at its root (FR-061): if archive (the job's
// exported workspace, gzipped tar) already has one, it's returned
// unchanged; otherwise one is generated from simple marker-file
// detection and appended. Returns ErrNoRunnableProject if archive has
// neither a Dockerfile nor a recognized project marker.
//
// The generated Dockerfiles are a best-effort default, not a
// guarantee — they assume the project's entry point listens on
// previewPort and builds/runs with the single obvious command for its
// ecosystem (go build ., npm start, python main.py). A project that
// doesn't fit that shape needs its own Dockerfile, which this
// function detects and passes through unmodified.
func EnsureDockerfile(archive []byte) ([]byte, error) {
	hasDockerfile, detected, err := inspectArchive(archive)
	if err != nil {
		return nil, fmt.Errorf("deploy: inspect archive: %w", err)
	}
	if hasDockerfile {
		return archive, nil
	}
	if detected == "" {
		return nil, ErrNoRunnableProject
	}

	out, err := appendDockerfile(archive, detected)
	if err != nil {
		return nil, fmt.Errorf("deploy: append generated dockerfile: %w", err)
	}
	return out, nil
}

// inspectArchive reads archive once, reporting whether it already has
// a root-level Dockerfile and, if not, the first recognized project
// template (empty if none matched).
func inspectArchive(archive []byte) (hasDockerfile bool, dockerfile string, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return false, "", fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	seen := make(map[string]bool)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, "", fmt.Errorf("read tar entry: %w", err)
		}
		name := normalizeEntryName(header.Name)
		if name == dockerfileName {
			return true, "", nil
		}
		seen[name] = true
	}

	for _, pt := range projectTypes {
		if seen[pt.marker] {
			return false, pt.dockerfile, nil
		}
	}
	return false, "", nil
}

// normalizeEntryName strips a single leading "./" — tar archives
// commonly store root-level entries as "./Dockerfile" rather than
// "Dockerfile" depending on how they were created (ExportWorkspace's
// own `tar czf - -C /workspace .` is one such case).
func normalizeEntryName(name string) string {
	if len(name) >= 2 && name[0] == '.' && name[1] == '/' {
		return name[2:]
	}
	return name
}

// copyTarEntries copies every entry from tr into tw unchanged. Split
// out of appendDockerfile purely to keep that function's branching
// within CLAUDE.md's cyclomatic-complexity limit.
func copyTarEntries(tw *tar.Writer, tr *tar.Reader) error {
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header for %s: %w", header.Name, err)
		}
		if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // reason: header.Size is the source tar's own declared size, not attacker-adjustable beyond what the archive already committed to
			return fmt.Errorf("copy tar content for %s: %w", header.Name, err)
		}
	}
}

// appendDockerfile returns archive with a new root-level Dockerfile
// entry containing dockerfile, re-gzipped. Every existing entry is
// copied through unchanged.
func appendDockerfile(archive []byte, dockerfile string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := copyTarEntries(tw, tar.NewReader(gz)); err != nil {
		return nil, err
	}

	if err := tw.WriteHeader(&tar.Header{Name: dockerfileName, Size: int64(len(dockerfile)), Mode: 0o644}); err != nil {
		return nil, fmt.Errorf("write dockerfile header: %w", err)
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		return nil, fmt.Errorf("write dockerfile content: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}
