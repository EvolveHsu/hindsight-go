#!/usr/bin/env bash
# Read-mostly verification after the Hindsight Go cutover.
set -Eeuo pipefail

API_BASE="${HINDSIGHT_API_BASE:-http://127.0.0.1:8890}"
CP_BASE="${HINDSIGHT_CP_BASE:-http://127.0.0.1:9999}"
BANK="${HINDSIGHT_BANK:-default}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${ROOT_DIR}/deploy/docker-compose.yml"

pass=0
fail=0
ok() { printf 'PASS  %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$*"; fail=$((fail + 1)); }

check_http() {
  local name="$1" url="$2" expected="$3"
  local code
  code="$(curl -sS --max-time 8 -o /dev/null -w '%{http_code}' "${url}" || true)"
  if [[ "${code}" =~ ^(${expected})$ ]]; then
    ok "${name} -> ${code}"
  else
    bad "${name} -> ${code} (expected ${expected})"
  fi
}

check_http "Go health" "${API_BASE}/health" '200'
check_http "Go readiness" "${API_BASE}/health/ready" '200'
check_http "Go version" "${API_BASE}/version" '200'
check_http "MCP GET probe" "${API_BASE}/mcp/${BANK}/" '200|400|405'
check_http "Control Plane root" "${CP_BASE}/" '200|307'
check_http "Control Plane dashboard" "${CP_BASE}/dashboard" '200|307'

health_body="$(curl -sS --max-time 8 "${API_BASE}/health" || true)"
if printf '%s' "${health_body}" | grep -q '"database"[[:space:]]*:[[:space:]]*"connected"'; then
  ok "Go health reports connected database"
else
  bad "Go health does not report a connected database: ${health_body}"
fi

mcp_body="$(
  curl -sS --max-time 15 \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"hindsight-cutover-check","version":"1.0"}}}' \
    "${API_BASE}/mcp/${BANK}/" || true
)"
if printf '%s' "${mcp_body}" | grep -q '"result"'; then
  ok "MCP initialize returned a JSON-RPC result"
else
  bad "MCP initialize did not return a result: ${mcp_body:0:500}"
fi

tools_body="$(
  curl -sS --max-time 15 \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
    "${API_BASE}/mcp/${BANK}/" || true
)"
for tool in retain sync_retain recall reflect; do
  if printf '%s' "${tools_body}" | grep -q "\"name\":\"${tool}\""; then
    ok "MCP tool exposed: ${tool}"
  else
    bad "MCP tool missing: ${tool}"
  fi
done

recall_ok=false
for _ in $(seq 1 30); do
  recall_body="$(curl -sS --max-time 30 \
    -H 'Content-Type: application/json' \
    -d '{"query":"cutover semantic probe","budget":"low"}' \
    "${API_BASE}/v1/default/banks/${BANK}/memories/recall" || true)"
  if printf '%s' "${recall_body}" | grep -q '"results"'; then
    recall_ok=true
    break
  fi
  sleep 2
done
if [[ "${recall_ok}" == "true" ]]; then
  ok "embedding-backed recall returned results"
else
  bad "embedding-backed recall did not return results: ${recall_body:0:500}"
fi

if docker inspect hindsight-embedding >/dev/null 2>&1; then
  if [[ "$(docker inspect -f '{{.State.Running}}' hindsight-embedding 2>/dev/null || true)" == "true" ]]; then
    ok "hindsight-embedding container is running"
  else
    bad "hindsight-embedding container exists but is not running"
  fi
else
  bad "hindsight-embedding container not found"
fi

if docker inspect hindsight-postgres >/dev/null 2>&1; then
  ok "hindsight-postgres container exists"
  for ext in vector pg_trgm btree_gin; do
    if docker exec hindsight-postgres psql -U hindsight -d hindsight -Atc "SELECT 1 FROM pg_extension WHERE extname='${ext}'" | grep -q '^1$'; then
      ok "PostgreSQL extension present: ${ext}"
    else
      bad "PostgreSQL extension missing: ${ext}"
    fi
  done
  counts="$(docker exec hindsight-postgres psql -U hindsight -d hindsight -Atc 'SELECT count(*) FROM banks; SELECT count(*) FROM documents; SELECT count(*) FROM memory_units;' | paste -sd '|' -)"
  ok "database counts banks|documents|memory_units = ${counts}"
else
  bad "hindsight-postgres container not found"
fi

printf '\n%d passed, %d failed\n' "${pass}" "${fail}"
[[ "${fail}" -eq 0 ]]
