package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// caddyServerName is the name of the HTTP server block in Caddy's
// config (ops/caddy/Caddyfile) that owns the wildcard preview
// listener — routes are registered under this server, never the
// control plane's own API server if Caddy happens to also front that.
const caddyServerName = "previews"

// CaddyClient registers and removes reverse-proxy routes against a
// running Caddy instance's admin API (PRD §9.7, FR-062) — Caddy's
// automatic HTTPS handles certificate issuance for whatever domain
// the base config (ops/caddy/Caddyfile) names; this client only ever
// adds or removes one route at a time, never touches TLS
// configuration directly.
type CaddyClient struct {
	adminAddr string // e.g. http://127.0.0.1:2019
	http      *http.Client
}

// CaddyConfig configures a CaddyClient.
type CaddyConfig struct {
	AdminAddr string
	// HTTPClient is nil defaults to http.DefaultClient.
	HTTPClient *http.Client
}

func (c CaddyConfig) validate() error {
	if c.AdminAddr == "" {
		return fmt.Errorf("deploy: caddy config: AdminAddr is required")
	}
	return nil
}

// NewCaddyClient constructs a CaddyClient from cfg, or returns an
// error if cfg is invalid.
func NewCaddyClient(cfg CaddyConfig) (*CaddyClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &CaddyClient{adminAddr: cfg.AdminAddr, http: httpClient}, nil
}

// caddyRoute is the minimal Caddy JSON config for reverse-proxying
// one subdomain to one upstream host:port. "@id" is what makes the
// route independently addressable for removal later — without it,
// deleting a single dynamically-added route means diffing the whole
// server's route list instead of one DELETE call.
type caddyRoute struct {
	ID     string             `json:"@id"`
	Match  []caddyRouteMatch  `json:"match"`
	Handle []caddyRouteHandle `json:"handle"`
}

type caddyRouteMatch struct {
	Host []string `json:"host"`
}

type caddyRouteHandle struct {
	Handler   string             `json:"handler"`
	Upstreams []caddyRouteStream `json:"upstreams"`
}

type caddyRouteStream struct {
	Dial string `json:"dial"`
}

func routeID(jobID string) string {
	return "preview-" + jobID
}

// RegisterRoute adds a route sending https://{subdomain}.<domain> (the
// hostname is supplied by the caller — see DockerDeployer.Deploy) to
// 127.0.0.1:hostPort, appending it to caddyServerName's route list.
func (c *CaddyClient) RegisterRoute(ctx context.Context, hostname string, hostPort int) error {
	route := caddyRoute{
		ID:    routeID(hostname),
		Match: []caddyRouteMatch{{Host: []string{hostname}}},
		Handle: []caddyRouteHandle{{
			Handler:   "reverse_proxy",
			Upstreams: []caddyRouteStream{{Dial: fmt.Sprintf("127.0.0.1:%d", hostPort)}},
		}},
	}
	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("deploy: caddy: encode route: %w", err)
	}

	url := fmt.Sprintf("%s/config/apps/http/servers/%s/routes", c.adminAddr, caddyServerName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("deploy: caddy: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deploy: caddy: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deploy: caddy: admin api returned %s", resp.Status)
	}
	return nil
}

// RemoveRoute deletes the route RegisterRoute added for hostname.
// Idempotent: removing an already-gone route is not an error — Caddy
// returns 404 for an unknown @id, which this treats as success.
func (c *CaddyClient) RemoveRoute(ctx context.Context, hostname string) error {
	url := fmt.Sprintf("%s/id/%s", c.adminAddr, routeID(hostname))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("deploy: caddy: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("deploy: caddy: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("deploy: caddy: admin api returned %s", resp.Status)
	}
	return nil
}
