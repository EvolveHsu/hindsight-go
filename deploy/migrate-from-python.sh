#!/usr/bin/env bash
# One-time cutover from the all-in-one Python Hindsight container to:
#   hindsight-go + hindsight-control-plane + standalone PostgreSQL/pgvector
#
# This script does not delete the Python container, its image, or its data.
# The pre-cutover logical backup is kept under deploy/backups.
#
# Required before running:
#   cp deploy/.env.example deploy/.env
#   edit deploy/.env and set HINDSIGHT_DB_PASSWORD at minimum
#
# Run from the repository root on the NAS host or from an SSH session:
#   bash deploy/migrate-from-python.sh

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${ROOT_DIR}/deploy/docker-compose.yml"
ENV_FILE="${ROOT_DIR}/deploy/.env"
HINDSIGHT_GO_PORT="${HINDSIGHT_GO_PORT:-8890}"
HINDSIGHT_CP_PORT="${HINDSIGHT_CP_PORT:-9999}"
HINDSIGHT_MCP_BANK="${HINDSIGHT_MCP_BANK:-default}"
HINDSIGHT_SOURCE_NETWORK="${HINDSIGHT_SOURCE_NETWORK:-hindsight-go-llm}"
BACKUP_DIR="${ROOT_DIR}/deploy/backups"
OLD_CONTAINER="${HINDSIGHT_OLD_CONTAINER:-hindsight}"
OLD_COMPOSE="${HINDSIGHT_OLD_COMPOSE:-$ROOT_DIR/../hindsight/docker-compose.yml}"
PG_IMAGE="${HINDSIGHT_PG_IMAGE:-pgvector/pgvector:pg18}"
CP_IMAGE="${HINDSIGHT_CP_IMAGE:-ghcr.io/vectorize-io/hindsight-control-plane:0.10.2}"
STAMP="$(date +%Y%m%d-%H%M%S)"
DUMP_NAME="hindsight-pre-go-${STAMP}.dump"

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ -f "${ENV_FILE}" ]] || fail "missing ${ENV_FILE}; copy deploy/.env.example to deploy/.env and fill it in"
[[ -f "${COMPOSE_FILE}" ]] || fail "missing ${COMPOSE_FILE}"
[[ -f "${OLD_COMPOSE}" ]] || fail "missing old compose file ${OLD_COMPOSE}"
command -v docker >/dev/null || fail "docker is not on PATH"

# shellcheck disable=SC1090
set -a
. "${ENV_FILE}"
set +a
[[ -n "${HINDSIGHT_DB_PASSWORD:-}" ]] || fail "HINDSIGHT_DB_PASSWORD is empty in ${ENV_FILE}"

mkdir -p "${BACKUP_DIR}"
chmod 700 "${BACKUP_DIR}"

log "checking old container"
docker inspect "${OLD_CONTAINER}" >/dev/null 2>&1 || fail "container ${OLD_CONTAINER} not found"
[[ "$(docker inspect -f '{{.State.Running}}' "${OLD_CONTAINER}")" == "true" ]] || fail "container ${OLD_CONTAINER} is not running"

log "pulling runtime images"
docker pull "${PG_IMAGE}"
docker pull "${CP_IMAGE}"

log "reading source database password without printing it"
SRC_DB_PASSWORD="$(docker exec "${OLD_CONTAINER}" sh -lc 'sed -n "s/.*\"password\": *\"\([^\"]*\)\".*/\1/p" /home/hindsight/.pg0/instances/hindsight/instance.json')"
[[ -n "${SRC_DB_PASSWORD}" ]] || fail "could not read the embedded database password"

log "capturing source row counts"
SRC_COUNTS="$(
  docker exec "${OLD_CONTAINER}" sh -lc "
    PW=\$(sed -n 's/.*\"password\": *\"\\([^\"]*\\)\".*/\\1/p' /home/hindsight/.pg0/instances/hindsight/instance.json)
    PGPASSWORD=\"\$PW\" /home/hindsight/.pg0/installation/18.1.0/bin/psql -h 127.0.0.1 -U hindsight -d hindsight -Atc '
      SELECT count(*) FROM banks;
      SELECT count(*) FROM documents;
      SELECT count(*) FROM memory_units;'
  "
)"
SRC_COUNTS="$(printf '%s\n' "${SRC_COUNTS}" | paste -sd '|' -)"
[[ -n "${SRC_COUNTS}" ]] || fail "could not read source row counts"

log "writing logical backup ${DUMP_NAME}"
docker run --rm \
  --network "${HINDSIGHT_SOURCE_NETWORK}" \
  --user "$(id -u):$(id -g)" \
  -e PGPASSWORD="${SRC_DB_PASSWORD}" \
  -v "${BACKUP_DIR}:/backup" \
  "${PG_IMAGE}" \
  pg_dump -h hindsight -U hindsight -d hindsight -Fc -f "/backup/${DUMP_NAME}"

log "stopping old all-in-one container gracefully (data and compose are preserved)"
docker stop -t 45 "${OLD_CONTAINER}" >/dev/null

rollback() {
  log "cutover failed; attempting rollback"
  docker compose -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" down --remove-orphans >/dev/null 2>&1 || true
  docker compose -f "${OLD_COMPOSE}" up -d || true
  log "rollback attempted; inspect: docker logs hindsight --tail 100"
}
trap 'code=$?; if [[ $code -ne 0 ]]; then rollback; fi; exit $code' ERR

log "starting standalone PostgreSQL"
docker compose -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up -d hindsight-postgres

log "waiting for PostgreSQL health"
PG_HEALTHY=false
for _ in $(seq 1 60); do
  status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}starting{{end}}' hindsight-postgres 2>/dev/null || true)"
  if [[ "${status}" == "healthy" ]]; then
    PG_HEALTHY=true
    break
  fi
  sleep 2
done
[[ "${PG_HEALTHY}" == "true" ]] || fail "PostgreSQL did not become healthy"

log "restoring the logical backup"
docker run --rm \
  --network hindsight-go-internal \
  --user "$(id -u):$(id -g)" \
  -e PGPASSWORD="${HINDSIGHT_DB_PASSWORD}" \
  -v "${BACKUP_DIR}:/backup" \
  "${PG_IMAGE}" \
  pg_restore -h hindsight-postgres -U hindsight -d hindsight \
    --clean --if-exists --no-owner --no-privileges "/backup/${DUMP_NAME}"

log "checking restored row counts"
NEW_COUNTS="$(
  docker exec hindsight-postgres psql -U hindsight -d hindsight -Atc \
    "SELECT (SELECT count(*) FROM banks) || '|' ||
            (SELECT count(*) FROM documents) || '|' ||
            (SELECT count(*) FROM memory_units);"
)"
[[ "${SRC_COUNTS}" == "${NEW_COUNTS}" ]] || fail "row count mismatch: source=${SRC_COUNTS} restored=${NEW_COUNTS}"

log "starting hindsight-go and control plane"
if docker inspect hindsight-go >/dev/null 2>&1; then
  log "stopping the old direct-port Go container and keeping it for fallback"
  docker stop -t 10 hindsight-go >/dev/null 2>&1 || true
  docker rename hindsight-go "hindsight-go-legacy-${STAMP}" 2>/dev/null || true
fi
docker compose -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up -d hindsight-go hindsight-control-plane

log "waiting for the Go API health endpoint"
API_OK=false
for _ in $(seq 1 60); do
  if curl -fsS --max-time 3 "http://127.0.0.1:${HINDSIGHT_GO_PORT}/health" >/dev/null 2>&1; then
    API_OK=true
    break
  fi
  sleep 2
done
[[ "${API_OK}" == "true" ]] || fail "Go API did not answer on port ${HINDSIGHT_GO_PORT}"

log "checking the MCP endpoint"
MCP_OK=false
for _ in $(seq 1 30); do
  if curl -sS --max-time 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HINDSIGHT_GO_PORT}/mcp/${HINDSIGHT_MCP_BANK}/" | grep -qE '^(200|400|405)$'; then
    MCP_OK=true
    break
  fi
  sleep 2
done
[[ "${MCP_OK}" == "true" ]] || fail "MCP endpoint did not answer on ${HINDSIGHT_GO_PORT}/mcp/${HINDSIGHT_MCP_BANK}/"

trap - ERR
log "cutover complete"
log "API  http://<nas-ip>:${HINDSIGHT_GO_PORT}"
log "CP   http://<nas-ip>:${HINDSIGHT_CP_PORT}"
log "DB   hindsight-postgres:5432 (internal only)"
log "backup kept at ${BACKUP_DIR}/${DUMP_NAME}"
log "rollback: docker compose -f ${COMPOSE_FILE} --env-file ${ENV_FILE} down && docker compose -f ${OLD_COMPOSE} up -d"
