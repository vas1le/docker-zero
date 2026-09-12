#!/usr/bin/env bash
set -euo pipefail

ENGINE=${1:-./dist/docker-zero-linux-amd64}
COMPOSE=${COMPOSE_BIN:-docker}
BASE=$(mktemp -d)
SOCK="$BASE/docker.sock"
LEDGER="$BASE/ledgers"
ENGINE_PID=
cleanup() {
  if [[ -n "${ENGINE_PID:-}" ]]; then
    kill "$ENGINE_PID" 2>/dev/null || true
    wait "$ENGINE_PID" 2>/dev/null || true
  fi
  rm -rf "$BASE"
}
trap cleanup EXIT

cat > "$BASE/compose.yaml" <<'YAML'
name: dockerzero-smoke
services:
  nginx:
    image: nginx:alpine
    ports: ["18080:80"]
  redis:
    image: redis:7-alpine
    ports: ["16379:6379"]
YAML

"$ENGINE" --socket "$SOCK" --seed 0 --ledger-dir "$LEDGER" --container-endpoints off >"$BASE/engine.log" 2>&1 &
ENGINE_PID=$!
for _ in $(seq 1 100); do
  [[ -S "$SOCK" ]] && break
  sleep 0.02
done
[[ -S "$SOCK" ]] || { cat "$BASE/engine.log" >&2; exit 1; }
export DOCKER_HOST="unix://$SOCK"

if [[ "$COMPOSE" == "docker" ]]; then
  C=(docker compose -f "$BASE/compose.yaml" -p dockerzero-smoke)
else
  C=("$COMPOSE" -f "$BASE/compose.yaml" -p dockerzero-smoke)
fi

"${C[@]}" up -d
"${C[@]}" ps
"${C[@]}" restart redis
"${C[@]}" stop nginx
"${C[@]}" start nginx
"${C[@]}" down

if grep -R -q '"unsupported":true' "$LEDGER"; then
  echo "FAIL: unsupported Docker API request recorded" >&2
  grep -R '"unsupported":true' "$LEDGER" >&2 || true
  exit 1
fi

echo "docker-zero Compose smoke: PASS"
