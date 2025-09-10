#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$ROOT_DIR/test"
COMPOSE_FILE="$TEST_DIR/docker-compose.yml"
SPEC_FILE="$TEST_DIR/openapi.json"
CONFIG_FILE="$TEST_DIR/test_config.yml"
OUT_FILE="$TEST_DIR/output.jsonl"

# Determine how to invoke Docker
DOCKER=docker
if ! docker info >/dev/null 2>&1; then
  DOCKER="sudo docker"
fi

cd "$TEST_DIR"

echo "[1/4] Building and starting test API container..."
$DOCKER compose -f "$COMPOSE_FILE" up -d --build --quiet-pull

echo "[2/4] Waiting for health..."
for i in {1..60}; do
  if curl -sf http://localhost:8080/health >/dev/null; then
    echo "Service healthy."
    break
  fi
  sleep 1
  if [[ $i -eq 60 ]]; then
    echo "Service failed to become healthy in time" >&2
    $DOCKER compose -f "$COMPOSE_FILE" logs | tail -n 200 || true
    exit 1
  fi
done

echo "[3/4] Running aperture scanner..."
rm -f "$OUT_FILE"
GO111MODULE=on go run "$ROOT_DIR/cmd/main.go" -spec "$SPEC_FILE" -config "$CONFIG_FILE" -base-url "http://localhost:8080" -out "$OUT_FILE" -v

# Summarize results without external deps
echo "[3.5/4] Summarizing results..."
if [[ ! -f "$OUT_FILE" ]]; then
  echo "No output file found at $OUT_FILE" >&2
else
  SECURE=$(grep -o '"result":"SECURE"' "$OUT_FILE" | wc -l | tr -d ' ')
  IDOR=$(grep -o '"result":"IDOR FOUND"' "$OUT_FILE" | wc -l | tr -d ' ')
  POTENTIAL=$(grep -o '"result":"POTENTIAL"' "$OUT_FILE" | wc -l | tr -d ' ')
  CTRL_FAILED=$(grep -o '"result":"CONTROL_FAILED"' "$OUT_FILE" | wc -l | tr -d ' ')
  SKIPPED=$(grep -o '"result":"SKIPPED"' "$OUT_FILE" | wc -l | tr -d ' ')
  TESTED=$((SECURE + IDOR + POTENTIAL + CTRL_FAILED))
  FAILED=$((IDOR + POTENTIAL + CTRL_FAILED))
  echo "Results: tested=$TESTED, passed=$SECURE, failed=$FAILED (idor=$IDOR, potential=$POTENTIAL, control_failed=$CTRL_FAILED), skipped=$SKIPPED"
fi

echo "[4/4] Tearing down containers..."
$DOCKER compose -f "$COMPOSE_FILE" down -v

echo "Done. Output log: $OUT_FILE" 