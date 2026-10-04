#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
# This dedicated database holds integration fixtures, not development accounts.
if ! docker exec lidza-deploy-postgres psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='lidza_deploy_test'" | grep -q 1; then
 docker exec lidza-deploy-postgres createdb -U postgres lidza_deploy_test
fi
export TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55432/lidza_deploy_test?sslmode=disable'
export TEST_DOCKER=1 TEST_CADDY=1
go test -race ./...
go vet ./...
