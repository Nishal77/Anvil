package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/deploy"
)

// caddyTestConfig is a minimal stand-in for ops/caddy/caddy.json: the
// same "previews" server name deploy.CaddyClient.RegisterRoute always
// targets, on plain HTTP instead of the real file's ":443" with
// automatic HTTPS. TLS is deliberately out of scope for this test —
// ops/caddy/caddy.json's own doc comment explains that a trusted
// certificate for the wildcard preview domain needs a real DNS
// provider and ACME credentials, neither of which exists in a test
// environment. What this test proves is the part that is this
// project's own code: building a preview image, running it, and
// wiring Caddy's admin API to route a job's subdomain to it. Caddy's
// automatic-HTTPS machinery is Caddy's own well-tested feature, not
// this project's.
const caddyTestConfig = `{
  "admin": {"listen": "127.0.0.1:%s"},
  "apps": {
    "http": {
      "servers": {
        "previews": {
          "listen": [":%s"],
          "automatic_https": {"disable": true},
          "routes": []
        }
      }
    }
  }
}`

// previewDockerfile is a minimal, self-contained image — no dependency
// on images/workspace/Dockerfile — serving a fixed page on 8080, the
// port every deploy.EnsureDockerfile-generated Dockerfile also binds
// (internal/deploy/dockerfile.go's previewPort). Duplicated as a
// literal rather than imported for the same reason that file
// documents its own duplication of the same value: nothing outside
// cmd/runner may import internal/sandbox/runner, where the sandbox
// side of that contract actually lives.
const previewDockerfile = `FROM busybox:latest
RUN mkdir -p /www && echo "gate 3 preview ok" > /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
`

// TestFR062_PreviewDeployedThroughCaddyReturns200 is Gate 3's second
// checklist item (docs/BUILD-PLAN.md, end of Week 9: "A completed job
// yields a working https://{job_id}.preview.<domain> that returns
// 200"). It runs the real chain deploy.DockerDeployer drives in
// production — a real Runner building and running a Docker image, a
// real Caddy instance registering the route over its admin API — and
// confirms a request for the job's hostname actually reaches the
// container and gets a 200.
//
// Caddy runs as a real subprocess of the `caddy` binary, not a Docker
// container: production runs it as a bare process on the same host as
// the Runner (ops/systemd, ops/caddy/caddy.json's own "caddy run"
// invocation comment) specifically so that Caddy's 127.0.0.1 is the
// same 127.0.0.1 Docker publishes preview containers' ports to.
// Containerizing Caddy here would put it behind a second network
// namespace where that address no longer means the same thing,
// silently invalidating the test's proof.
func TestFR062_PreviewDeployedThroughCaddyReturns200(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a real Docker daemon; skipped in -short")
	}
	if _, err := exec.LookPath("caddy"); err != nil {
		t.Skip("requires the caddy binary on PATH (matches production's ops/caddy deployment) — install with e.g. `brew install caddy`")
	}
	ctx := context.Background()

	sandboxClient := newTestSandboxClient(t)
	adminAddr, previewAddr := startTestCaddy(t)

	caddy, err := deploy.NewCaddyClient(deploy.CaddyConfig{AdminAddr: adminAddr})
	if err != nil {
		t.Fatalf("construct caddy client: %v", err)
	}
	deployer, err := deploy.New(deploy.Config{Runner: sandboxClient, Caddy: caddy, Domain: "preview.test"})
	if err != nil {
		t.Fatalf("construct deployer: %v", err)
	}

	jobID := uuid.New()
	previewURL, err := deployer.Deploy(ctx, jobID, buildPreviewArchive(t))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	t.Cleanup(func() {
		if err := deployer.Destroy(context.Background(), jobID); err != nil {
			t.Errorf("cleanup: Destroy: %v", err)
		}
	})

	wantHostname := jobID.String() + ".preview.test"
	if previewURL != "https://"+wantHostname {
		t.Fatalf("previewURL = %q, want %q", previewURL, "https://"+wantHostname)
	}

	body, status := fetchThroughCaddy(t, previewAddr, wantHostname)
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if body != "gate 3 preview ok\n" {
		t.Errorf("body = %q, want the preview container's fixed page", body)
	}
}

// startTestCaddy launches the real caddy binary against a config file
// shaped like caddyTestConfig, and returns its admin API address and
// the address its "previews" server listens on. Both ports are chosen
// freely — Caddy binds them itself, so unlike a Go http.Server there
// is no way to ask it to pick one and report back; two free ports are
// found up front instead and released just before Caddy claims them,
// the same short race every "reserve a free port" helper in this repo
// accepts (see test/integration's own newTestSandboxClient).
func startTestCaddy(t *testing.T) (adminAddr, previewAddr string) {
	t.Helper()
	ctx := context.Background()

	adminPort := freeTCPPort(t)
	previewPort := freeTCPPort(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "caddy.json")
	cfg := fmt.Sprintf(caddyTestConfig, adminPort, previewPort)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write caddy config: %v", err)
	}

	cmd := exec.CommandContext(ctx, "caddy", "run", "--config", cfgPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start caddy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	adminAddr = "http://127.0.0.1:" + adminPort
	waitForCaddyAdmin(t, adminAddr)
	return adminAddr, "127.0.0.1:" + previewPort
}

// freeTCPPort asks the OS for an unused loopback port and immediately
// releases it for a subprocess (Caddy) to bind instead — the same
// technique test/integration's newTestSandboxClient uses to hand a
// free port to the in-process Runner.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split free port: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return port
}

// waitForCaddyAdmin blocks until adminAddr's admin API answers —
// startup takes a moment after cmd.Start returns, and RegisterRoute's
// very first call must not race that.
func waitForCaddyAdmin(t *testing.T, adminAddr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminAddr+"/config/", nil)
		if err != nil {
			t.Fatalf("build readiness request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("caddy admin API never became reachable: %v", err)
		}
	}
}

// buildPreviewArchive gzip-tars previewDockerfile as the sole entry, at
// the root name deploy.EnsureDockerfile looks for — the shape
// Deploy(jobID, archive) expects a job's finished workspace to arrive
// in.
func buildPreviewArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(previewDockerfile)), Mode: 0o644}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write([]byte(previewDockerfile)); err != nil {
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

// fetchThroughCaddy polls previewAddr with hostname as the Host header
// — no DNS is involved; Caddy's host-match routing keys off that
// header alone, the same field a real HTTPS request's Host header (or
// SNI) would carry. Retried: Deploy just returned, and both "the
// container is actually listening" and "the admin API's route push
// has taken effect" still need a moment.
func fetchThroughCaddy(t *testing.T, previewAddr, hostname string) (body string, status int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	url := "http://" + previewAddr + "/"
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Host = hostname

		resp, err := http.DefaultClient.Do(req)
		switch {
		case err != nil:
			lastErr = err
		default:
			data, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read response body: %v", readErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(data), resp.StatusCode
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("never got a 200 from %s (Host: %s): %v", url, hostname, lastErr)
		}
	}
}
