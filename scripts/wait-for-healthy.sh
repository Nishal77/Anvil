#!/usr/bin/env bash
#
# wait-for-healthy.sh — block until the named docker-compose services report
# healthy, so `make dev` doesn't run migrations against a Postgres that
# hasn't finished starting yet.
#
# Usage: ./scripts/wait-for-healthy.sh postgres redis minio

set -euo pipefail

for service in "$@"; do
    echo "waiting for ${service}..."
    until [ "$(docker compose ps -q "${service}" | xargs -I{} docker inspect -f '{{.State.Health.Status}}' {} 2>/dev/null)" = "healthy" ]; do
        sleep 1
    done
    echo "${service} healthy"
done
