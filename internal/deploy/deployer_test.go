package deploy

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/sandbox"
)

type fakeRunner struct {
	buildResult sandbox.BuildPreviewResponse
	buildErr    error
	destroyErr  error

	gotBuildContext []byte
	destroyCalled   bool
}

func (f *fakeRunner) BuildPreview(_ context.Context, _ uuid.UUID, buildContext io.Reader) (sandbox.BuildPreviewResponse, error) {
	if f.buildErr != nil {
		return sandbox.BuildPreviewResponse{}, f.buildErr
	}
	data, _ := io.ReadAll(buildContext)
	f.gotBuildContext = data
	return f.buildResult, nil
}

func (f *fakeRunner) DestroyPreview(context.Context, uuid.UUID) error {
	f.destroyCalled = true
	return f.destroyErr
}

type fakeCaddy struct {
	registerErr error
	removeErr   error

	gotHostname string
	gotHostPort int
	removeCalls []string
}

func (f *fakeCaddy) RegisterRoute(_ context.Context, hostname string, hostPort int) error {
	f.gotHostname, f.gotHostPort = hostname, hostPort
	return f.registerErr
}

func (f *fakeCaddy) RemoveRoute(_ context.Context, hostname string) error {
	f.removeCalls = append(f.removeCalls, hostname)
	return f.removeErr
}

func testArchiveWithDockerfile(t *testing.T) []byte {
	t.Helper()
	return buildArchive(t, map[string]string{"Dockerfile": "FROM scratch\n"})
}

func TestDockerDeployer_Deploy_ReturnsPreviewURL(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{buildResult: sandbox.BuildPreviewResponse{ContainerID: "c1", HostPort: 45678}}
	caddy := &fakeCaddy{}
	d, err := New(Config{Runner: runner, Caddy: caddy, Domain: "preview.anvil.dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	jobID := uuid.New()
	url, err := d.Deploy(context.Background(), jobID, testArchiveWithDockerfile(t))
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantURL := "https://" + jobID.String() + ".preview.anvil.dev"
	if url != wantURL {
		t.Errorf("Deploy() = %q, want %q", url, wantURL)
	}
	if caddy.gotHostname != jobID.String()+".preview.anvil.dev" || caddy.gotHostPort != 45678 {
		t.Errorf("RegisterRoute called with (%q, %d), want (%q, 45678)", caddy.gotHostname, caddy.gotHostPort, jobID.String()+".preview.anvil.dev")
	}
}

func TestDockerDeployer_Deploy_NoDockerfileFailsBeforeBuilding(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	d, err := New(Config{Runner: runner, Caddy: &fakeCaddy{}, Domain: "preview.anvil.dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	archive := buildArchive(t, map[string]string{"README.md": "hello"})
	_, err = d.Deploy(context.Background(), uuid.New(), archive)
	if !errors.Is(err, ErrNoRunnableProject) {
		t.Errorf("Deploy() error = %v, want ErrNoRunnableProject", err)
	}
	if runner.gotBuildContext != nil {
		t.Error("BuildPreview was called despite Dockerfile detection failing first")
	}
}

func TestDockerDeployer_Deploy_BuildFailurePropagates(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{buildErr: errors.New("docker build failed")}
	d, err := New(Config{Runner: runner, Caddy: &fakeCaddy{}, Domain: "preview.anvil.dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = d.Deploy(context.Background(), uuid.New(), testArchiveWithDockerfile(t))
	if err == nil {
		t.Fatal("Deploy() error = nil, want the build failure to propagate")
	}
}

func TestDockerDeployer_Deploy_RouteRegistrationFailurePropagates(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{buildResult: sandbox.BuildPreviewResponse{ContainerID: "c1", HostPort: 1}}
	caddy := &fakeCaddy{registerErr: errors.New("caddy admin api unreachable")}
	d, err := New(Config{Runner: runner, Caddy: caddy, Domain: "preview.anvil.dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = d.Deploy(context.Background(), uuid.New(), testArchiveWithDockerfile(t))
	if err == nil {
		t.Fatal("Deploy() error = nil, want the route registration failure to propagate")
	}
}

func TestDockerDeployer_Destroy_RemovesRouteThenContainer(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	caddy := &fakeCaddy{}
	d, err := New(Config{Runner: runner, Caddy: caddy, Domain: "preview.anvil.dev"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	jobID := uuid.New()
	if err := d.Destroy(context.Background(), jobID); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}
	if len(caddy.removeCalls) != 1 || caddy.removeCalls[0] != jobID.String()+".preview.anvil.dev" {
		t.Errorf("RemoveRoute calls = %v, want exactly one for the job's hostname", caddy.removeCalls)
	}
	if !runner.destroyCalled {
		t.Error("DestroyPreview was not called")
	}
}

func TestConfig_ValidateRejectsMissingFields(t *testing.T) {
	t.Parallel()
	cases := []Config{
		{Caddy: &fakeCaddy{}, Domain: "d"},
		{Runner: &fakeRunner{}, Domain: "d"},
		{Runner: &fakeRunner{}, Caddy: &fakeCaddy{}},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: New() error = nil, want an error for an incomplete config", i)
		}
	}
}
