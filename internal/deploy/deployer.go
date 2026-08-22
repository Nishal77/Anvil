package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/telemetry"
)

// Deployer builds a job's finished workspace into a preview
// deployment and returns its URL (FR-060). The only production
// implementation is DockerDeployer; declared as an interface because
// PRD §9.8 documents extracting the Runner into a standalone service
// as a scaling path, at which point a second implementation could
// target it without any caller of Deployer changing.
type Deployer interface {
	// Deploy builds archive (a gzipped tar of the job's workspace) and
	// runs it as jobID's preview deployment, returning its URL.
	Deploy(ctx context.Context, jobID uuid.UUID, archive []byte) (previewURL string, err error)
	// Destroy tears down jobID's preview deployment, if one exists.
	Destroy(ctx context.Context, jobID uuid.UUID) error
}

// previewBuilder is the subset of *sandbox.Client DockerDeployer
// needs, declared at the consumer (CODE-STANDARDS §3.1).
type previewBuilder interface {
	BuildPreview(ctx context.Context, jobID uuid.UUID, buildContext io.Reader) (sandbox.BuildPreviewResponse, error)
	DestroyPreview(ctx context.Context, jobID uuid.UUID) error
}

// caddyRegistrar is the subset of *CaddyClient DockerDeployer needs.
type caddyRegistrar interface {
	RegisterRoute(ctx context.Context, hostname string, hostPort int) error
	RemoveRoute(ctx context.Context, hostname string) error
}

// Config configures a DockerDeployer.
type Config struct {
	Runner previewBuilder
	Caddy  caddyRegistrar
	// Domain is the base domain previews are served under — a preview
	// is reachable at https://{job_id}.<Domain> (FR-062).
	Domain string
}

func (c Config) validate() error {
	if c.Runner == nil {
		return errors.New("deploy: config: Runner is required")
	}
	if c.Caddy == nil {
		return errors.New("deploy: config: Caddy is required")
	}
	if c.Domain == "" {
		return errors.New("deploy: config: Domain is required")
	}
	return nil
}

// DockerDeployer is the v1 Deployer: local Docker on the same host as
// the Runner, fronted by Caddy for TLS and subdomain routing
// (FR-060).
type DockerDeployer struct {
	runner previewBuilder
	caddy  caddyRegistrar
	domain string
}

// New constructs a DockerDeployer from cfg, or returns an error if
// cfg is invalid.
func New(cfg Config) (*DockerDeployer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &DockerDeployer{runner: cfg.Runner, caddy: cfg.Caddy, domain: cfg.Domain}, nil
}

// Deploy implements Deployer.
func (d *DockerDeployer) Deploy(ctx context.Context, jobID uuid.UUID, archive []byte) (string, error) {
	ctx, span := telemetry.Tracer("deploy").Start(ctx, "deploy.preview", trace.WithAttributes(
		telemetry.AttrJobID.String(jobID.String()),
	))
	defer span.End()

	url, err := d.deploy(ctx, jobID, archive)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetAttributes(attribute.String("anvil.preview_url", url))
	return url, nil
}

// deploy is Deploy's actual body, split out so the span wrapper above
// stays a thin, uniform shape.
func (d *DockerDeployer) deploy(ctx context.Context, jobID uuid.UUID, archive []byte) (string, error) {
	buildContext, err := EnsureDockerfile(archive)
	if err != nil {
		return "", fmt.Errorf("deploy: %w", err)
	}

	result, err := d.runner.BuildPreview(ctx, jobID, bytes.NewReader(buildContext))
	if err != nil {
		return "", fmt.Errorf("deploy: build preview: %w", err)
	}

	hostname := jobID.String() + "." + d.domain
	if err := d.caddy.RegisterRoute(ctx, hostname, result.HostPort); err != nil {
		return "", fmt.Errorf("deploy: register route: %w", err)
	}

	return "https://" + hostname, nil
}

// Destroy implements Deployer. The Caddy route is removed before the
// container: a route left pointing at a container that's already gone
// would 502 on every request until removed anyway, so there's no
// ordering where doing it the other way round is better.
func (d *DockerDeployer) Destroy(ctx context.Context, jobID uuid.UUID) error {
	hostname := jobID.String() + "." + d.domain
	if err := d.caddy.RemoveRoute(ctx, hostname); err != nil {
		return fmt.Errorf("deploy: remove route: %w", err)
	}
	if err := d.runner.DestroyPreview(ctx, jobID); err != nil {
		return fmt.Errorf("deploy: destroy preview: %w", err)
	}
	return nil
}
