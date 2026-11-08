"""NAS-only deployment helper. Private configuration never leaves the NAS."""
import argparse
import copy
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import tarfile
import time
import urllib.request

BASE = Path("/vol1/1000/docker/hindsight-go")
SOURCE = BASE / "source-retain-fix-20261107"
BACKUP = BASE / "backup-retain-fix-20261107"
NAME = "hindsight-go"
CANDIDATE = NAME + "-candidate-retain-fix"
OLD = NAME + "-rollback-20261107"
IMAGE = "hindsight-go:retain-fix-20261107"


class DockerConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect("/var/run/docker.sock")


def docker(method, path, data=None, expected=(200, 201, 204)):
    connection = DockerConnection("localhost")
    body = json.dumps(data).encode() if data is not None else None
    connection.request(method, path, body, {"Content-Type": "application/json"})
    response = connection.getresponse()
    raw = response.read()
    connection.close()
    if response.status not in expected:
        raise RuntimeError("Docker request failed: %s %s status=%d" %
                           (method, path.split("?")[0], response.status))
    return json.loads(raw) if raw else None


def inspect(name):
    return docker("GET", "/containers/" + name + "/json")


def request(port, method, path, body=None, timeout=420):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        "http://127.0.0.1:%s%s" % (port, path), data=data, method=method,
        headers={"Content-Type": "application/json",
                 "Accept": "application/json, text/event-stream"})
    with urllib.request.urlopen(req, timeout=timeout) as response:
        raw = response.read().decode()
    if raw.lstrip().startswith("{"):
        return json.loads(raw)
    for line in raw.splitlines():
        if line.startswith("data:") and line[5:].strip():
            return json.loads(line[5:])
    raise RuntimeError("response was neither JSON nor JSON-RPC SSE")


def rpc(port, bank, name, arguments):
    envelope = request(port, "POST", "/mcp/" + bank + "/", {
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": name, "arguments": arguments},
    })
    assert "error" not in envelope, "MCP JSON-RPC error"
    result = envelope["result"]
    assert result.get("isError") is not True, "MCP tool error"
    structured = result.get("structuredContent")
    if structured is None:
        structured = json.loads(result["content"][0]["text"])
    assert "error" not in structured, "MCP backend error"
    return structured


def database(sql):
    return subprocess.check_output([
        "docker", "exec", "hindsight-postgres", "psql", "-X", "-U", "hindsight",
        "-d", "hindsight", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql,
    ], text=True).strip()


def data_signature(exclude_restored_memories=False):
    signatures = {}
    for table, key in (("memory_units", "id"), ("documents", "id"), ("banks", "bank_id")):
        condition = ""
        if exclude_restored_memories and table != "banks":
            column = "document_id" if table == "memory_units" else "id"
            condition = (
                " WHERE COALESCE(%s,'') NOT IN ('nas-muse2api-go-upstream-sync-20261107',"
                "'nas-hindsight-retain-fix-20261107')" % column)
        signatures[table] = database(
            "SELECT count(*)::text || ':' || md5(COALESCE(string_agg("
            "md5(to_jsonb(t)::text), '' ORDER BY bank_id, %s), '')) FROM %s t%s" %
            (key, table, condition))
    return signatures


def health(name, port):
    for _ in range(40):
        try:
            out = request(port, "GET", "/health", timeout=3)
            assert out["status"] == "healthy"
            assert inspect(name)["State"]["Running"]
            version = request(port, "GET", "/version")
            assert version["api_version"] == "1.2.0"
            actual = subprocess.check_output(
                ["docker", "exec", name, "sha256sum", "/bin/hindsight-go"],
                text=True).split()[0]
            expected = hashlib.sha256((SOURCE / "deploy/runtime/hindsight-go").read_bytes()).hexdigest()
            assert actual == expected, "runtime binary mismatch"
            return out
        except Exception:
            time.sleep(1)
    raise RuntimeError("health/version/binary verification failed")


def create(name, original, candidate=False):
    config = copy.deepcopy(original["Config"])
    host = copy.deepcopy(original["HostConfig"])
    config["Image"] = IMAGE
    config.pop("Hostname", None)
    env = dict(item.split("=", 1) for item in config["Env"] if "=" in item)
    env.update(HINDSIGHT_GO_RETAIN_MAX_TOKENS="8192",
               HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT="32768")
    if candidate:
        env.pop("HINDSIGHT_GO_DATABASE_URL", None)
        env.pop("HINDSIGHT_GO_UPSTREAM_SCHEMA", None)
        env["HINDSIGHT_GO_RETAIN_MAX_TOKENS"] = "2048"
        env["HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT"] = "8192"
        env["HINDSIGHT_GO_FILE_ROOT"] = "/tmp/retain-fix-files"
        host["Binds"] = []
        host["PortBindings"] = {"8890/tcp": [{"HostIp": "127.0.0.1", "HostPort": ""}]}
        host["RestartPolicy"] = {"Name": "no"}
    config["Env"] = [key + "=" + value for key, value in env.items()]
    config["HostConfig"] = host
    config["NetworkingConfig"] = {"EndpointsConfig": {
        network: {"Aliases": [name]} for network in original["NetworkSettings"]["Networks"]
    }}
    docker("POST", "/containers/create?name=" + name, config)
    docker("POST", "/containers/" + name + "/start")


def synthetic_roundtrip(name, port, production=False):
    bank = "retain-fix-canary-20261107"
    path = "/v1/default/banks/" + bank
    existing = request(port, "GET", "/v1/default/banks")["banks"]
    assert not any(b["bank_id"] == bank for b in existing), "canary bank already exists"
    before = data_signature() if production else None
    request(port, "PUT", path, {})
    cleaned = False
    try:
        result = rpc(port, bank, "sync_retain", {
            "content": (
                "Synthetic diagnostic for unique project RetainCanary20261107. "
                "On 2026-11-07 this fictional project upgraded from version 1.6.6 to 1.6.9. "
                "It accepts image, video, audio and document references, with ten attachments, "
                "forty MiB each, one hundred MiB total and a 120 second video limit. "
                "It uses a new composer, CDP uploads, temporary file cleanup, original attachment "
                "hash filtering, proxy credential decoding and short-lived exact-route test receipts. "
                "It preserved three accounts and their configuration, passed unit tests, race tests, "
                "static checks, 72 prompt comparisons and desktop/mobile browser tests. "
                "No real paid generation was performed. A stopped container and private backup "
                "remain available for rollback. Its source was archived but not committed or pushed."
            ),
            "context": "synthetic-diagnostic",
            "document_id": "canary-document-20261107",
        })
        assert result.get("success") is True and result["items_count"] > 0, "retain did not succeed"
        recalled = rpc(port, bank, "recall", {
            "query": "RetainCanary20261107 version upgrade attachments", "max_tokens": 4096,
        })
        assert len(recalled.get("results", [])) > 0, "recall returned no facts"
        print(json.dumps({"service": name, "mcp_retain": True,
                          "facts": result["items_count"], "mcp_recall": True}), flush=True)
    finally:
        # Guard against the separately audited cross-bank chunk ownership bug.
        if production:
            conflicts = int(database(
                "SELECT count(*) FROM memory_units m JOIN chunks c ON m.chunk_id=c.chunk_id "
                "WHERE (m.bank_id='%s' AND c.bank_id<>'%s') "
                "OR (m.bank_id<>'%s' AND c.bank_id='%s')" % (bank, bank, bank, bank)))
            assert conflicts == 0, "canary cleanup blocked by cross-bank chunk ownership"
        request(port, "DELETE", path)
        cleaned = True
        if production:
            assert data_signature() == before, "pre-existing data changed during canary"
        print(json.dumps({"canary_cleaned": cleaned,
                          "existing_data_unchanged": production or None}), flush=True)


parser = argparse.ArgumentParser()
parser.add_argument("action", choices=("backup", "candidate", "switch", "verify", "final", "cleanup"))
args = parser.parse_args()
os.umask(0o077)
if args.action == "backup":
    assert not BACKUP.exists(), "backup exists; refusing to overwrite"
    original = inspect(NAME)
    BACKUP.mkdir(mode=0o700)
    (BACKUP / "container-inspect.json").write_text(json.dumps(original, indent=2))
    (BACKUP / "data-signature.json").write_text(json.dumps(data_signature()))
    subprocess.run(["docker", "tag", original["Image"],
                    "hindsight-go:pre-retain-fix-20261107"], check=True)
    subprocess.run(["docker", "image", "save", "-o", str(BACKUP / "image.tar"),
                    original["Image"]], check=True)
    with (BACKUP / "database.dump").open("wb") as output:
        subprocess.run(["docker", "exec", "hindsight-postgres", "pg_dump", "-U", "hindsight",
                        "-d", "hindsight", "-Fc"], stdout=output, check=True)
    with tarfile.open(BACKUP / "source-and-deploy.tar.gz", "w:gz") as archive:
        for path in BASE.iterdir():
            if path.name in (BACKUP.name, SOURCE.name) or path.name.endswith(".tar.gz"):
                continue
            archive.add(path, arcname=path.name,
                        filter=lambda entry: None if entry.name.startswith("deploy/data/pg")
                        or entry.name.startswith("deploy/data/tei") else entry)
    print(json.dumps({"backup": str(BACKUP), "database_snapshot": True}))
elif args.action == "candidate":
    original = json.loads((BACKUP / "container-inspect.json").read_text())
    create(CANDIDATE, original, candidate=True)
    port = inspect(CANDIDATE)["NetworkSettings"]["Ports"]["8890/tcp"][0]["HostPort"]
    health(CANDIDATE, port)
    synthetic_roundtrip(CANDIDATE, port)
    logs = subprocess.check_output(["docker", "logs", CANDIDATE], stderr=subprocess.STDOUT, text=True)
    assert "result=truncated" in logs and "result=success" in logs, "real retry was not exercised"
    print(json.dumps({"candidate": "verified", "real_model_retry": True, "port": port}))
elif args.action == "switch":
    original = json.loads((BACKUP / "container-inspect.json").read_text())
    assert data_signature() == json.loads((BACKUP / "data-signature.json").read_text()), "data drift since backup"
    docker("POST", "/containers/" + NAME + "/stop?t=60", expected=(204, 304))
    docker("POST", "/containers/" + NAME + "/rename?name=" + OLD)
    try:
        create(NAME, original)
        assert health(NAME, 38462)["database"] == "connected"
        print(json.dumps({"production": "healthy", "rollback_container": OLD}))
    except Exception:
        docker("DELETE", "/containers/" + NAME + "?force=true", expected=(204, 404))
        docker("POST", "/containers/" + OLD + "/rename?name=" + NAME)
        docker("POST", "/containers/" + NAME + "/start")
        raise
elif args.action == "verify":
    original = json.loads((BACKUP / "container-inspect.json").read_text())
    current = inspect(NAME)
    expected_env = dict(item.split("=", 1) for item in original["Config"]["Env"] if "=" in item)
    expected_env.update(HINDSIGHT_GO_RETAIN_MAX_TOKENS="8192",
                        HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT="32768")
    actual_env = dict(item.split("=", 1) for item in current["Config"]["Env"] if "=" in item)
    assert expected_env == actual_env, "environment drift"
    assert current["Mounts"] == original["Mounts"], "mount drift"
    assert current["HostConfig"] == original["HostConfig"], "Docker resource configuration drift"
    assert set(current["NetworkSettings"]["Networks"]) == set(original["NetworkSettings"]["Networks"]), "network drift"
    assert data_signature() == json.loads((BACKUP / "data-signature.json").read_text()), "pre-existing data drift"
    health(NAME, 38462)
    synthetic_roundtrip(NAME, 38462, production=True)
    print(json.dumps({"production": "verified", "configuration_preserved": True,
                      "image": current["Config"]["Image"]}))
elif args.action == "final":
    assert data_signature(exclude_restored_memories=True) == json.loads(
        (BACKUP / "data-signature.json").read_text()), "pre-existing data changed"
    health(NAME, 38462)
    print(json.dumps({"production": "healthy", "pre_existing_data_unchanged": True,
                      "restored_documents": 2, "image": inspect(NAME)["Config"]["Image"]}))
else:
    docker("DELETE", "/containers/" + CANDIDATE + "?force=true", expected=(204, 404))
    print(json.dumps({"candidate_removed": True}))
