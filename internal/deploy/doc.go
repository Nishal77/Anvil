// Package deploy builds a job's finished workspace into a container
// image and runs it as a short-lived preview deployment, reachable at
// a per-job subdomain through Caddy (PRD §9.7). Its public entry point
// is Deployer, implemented by DockerDeployer — the v1, single-host
// Docker implementation FR-060 requires. The Docker socket itself is
// never touched here (CLAUDE.md I-4): DockerDeployer talks to the
// Runner over HTTP via internal/sandbox.Client, the same way the
// agent's sandbox tools do.
package deploy
