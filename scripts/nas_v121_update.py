"""NAS-only v1.2.1 deployment helper. Secrets stay in Docker's local config."""
import argparse
import copy
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import time
import urllib.request

BASE = Path("/vol1/1000/docker/hindsight-go")
SOURCE = BASE / "source-v1.2.1-20261110"
BACKUP = Path("/vol1/1000/docker/hindsight-go-backup-v1.2.1-20261110")
NAME = "hindsight-go"
CANDIDATE = NAME + "-candidate-v121"
OLD = NAME + "-rollback-v121-20261110"
IMAGE = "hindsight-go:v1.2.1-20261110"
API_VERSION = "1.2.1"
SERVICE_VERSION = "0.1.4"


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


def request(port, method, path, body=None, timeout=30):
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


def rpc(port, bank, method, params=None):
    return request(port, "POST", "/mcp/" + bank + "/", {
        "jsonrpc": "2.0",
        "id": 1,
        "method": method,
        "params": params or {},
    })


def database(sql):
    return subprocess.check_output([
        "docker", "exec", "hindsight-postgres", "psql", "-X", "-U", "hindsight",
        "-d", "hindsight", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql,
    ], text=True).strip()


def data_signature():
    signatures = {}
    for table, key in (
        ("banks", "bank_id"),
        ("documents", "id"),
        ("memory_units", "id"),
        ("knowledge_pages", "id"),
        ("mental_models", "id"),
    ):
        signatures[table] = database(
            "SELECT count(*)::text || ':' || md5(COALESCE(string_agg("
            "md5(to_jsonb(t)::text), '' ORDER BY %s), '')) FROM %s t" % (key, table))
    return signatures


def image_binary_hash():
    output = subprocess.check_output(
        ["docker", "run", "--rm", "--entrypoint", "sha256sum", IMAGE, "/bin/hindsight-go"],
        text=True,
    )
    return output.split()[0]


def health(name, port):
    for _ in range(60):
        try:
            out = request(port, "GET", "/health", timeout=3)
            assert out["status"] == "healthy"
            assert out["version"] == SERVICE_VERSION
            assert inspect(name)["State"]["Running"]
            version = request(port, "GET", "/version", timeout=3)
            assert version["api_version"] == API_VERSION
            actual = subprocess.check_output(
                ["docker", "exec", name, "sha256sum", "/bin/hindsight-go"],
                text=True,
            ).split()[0]
            expected = hashlib.sha256(
                (SOURCE / "deploy/runtime/hindsight-go").read_bytes()
            ).hexdigest()
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
    if candidate:
        env.pop("HINDSIGHT_GO_DATABASE_URL", None)
        env.pop("HINDSIGHT_GO_UPSTREAM_SCHEMA", None)
        env["HINDSIGHT_GO_FILE_ROOT"] = "/tmp/hindsight-v121-files"
        host["Binds"] = []
        host["PortBindings"] = {"8890/tcp": [{"HostIp": "127.0.0.1", "HostPort": ""}]}
        host["RestartPolicy"] = {"Name": "no"}
    config["Env"] = [key + "=" + value for key, value in env.items()]
    config["HostConfig"] = host
    config["NetworkingConfig"] = {"EndpointsConfig": {
        network: {"Aliases": [name]}
        for network in original["NetworkSettings"]["Networks"]
    }}
    docker("POST", "/containers/create?name=" + name, config)
    docker("POST", "/containers/" + name + "/start")


def verify_mcp(port):
    init = rpc(port, "canary", "initialize", {
        "protocolVersion": "2025-03-26",
        "capabilities": {},
        "clientInfo": {"name": "nas-v121-check", "version": "1"},
    })
    assert "error" not in init, "MCP initialize failed"
    listed = rpc(port, "canary", "tools/list")
    tools = {
        tool["name"] for tool in listed["result"]["tools"]
    }
    required = {
        "get_page_tree",
        "search_pages",
        "get_page",
        "create_page",
        "update_page_node",
        "delete_page_node",
    }
    assert required <= tools, "v1.2.1 Pages MCP tools are missing"
    assert "get_knowledge_base_tree" in tools, "deprecated MCP alias disappeared"


def verify_pages(port):
    bank = "v121-pages-canary-20261110"
    base = "/v1/default/banks/" + bank
    existing = request(port, "GET", "/v1/default/banks")["banks"]
    assert not any(item["bank_id"] == bank for item in existing), "canary bank exists"
    before = data_signature()
    request(port, "PUT", base, {})
    try:
        created = request(port, "POST", base + "/pages", {
            "name": "Release canary",
            "source_query": "What changed in Hindsight v1.2.1?",
            "tags": ["canary"],
            "max_tokens": 512,
        })
        page_id = created["page_id"]
        tree = request(port, "GET", base + "/pages/tree?tags=canary&tags_match=all")
        assert any(node["id"] == page_id for node in tree["roots"]), "page tree missing canary"
        page = request(port, "GET", base + "/pages/" + page_id)
        assert page["id"] == page_id, "page read mismatch"
        legacy = request(port, "GET", base + "/knowledge-base/pages/" + page_id)
        assert legacy["id"] == page_id, "deprecated page alias mismatch"
    finally:
        request(port, "DELETE", base)
    assert data_signature() == before, "pre-existing production data changed"


parser = argparse.ArgumentParser()
parser.add_argument("action", choices=("snapshot", "candidate", "switch", "reswitch", "redeploy", "smoke", "verify", "cleanup"))
args = parser.parse_args()
os.umask(0o077)

if args.action == "snapshot":
    snapshot = BACKUP / "deployment-snapshot.json"
    assert not snapshot.exists(), "deployment snapshot exists"
    original = inspect(NAME)
    snapshot.write_text(json.dumps({
        "container": original,
        "data_signature": data_signature(),
        "image": original["Config"]["Image"],
    }, indent=2))
    print(json.dumps({"snapshot": str(snapshot), "data_tables": 5}))
elif args.action == "candidate":
    original = json.loads((BACKUP / "deployment-snapshot.json").read_text())["container"]
    docker("DELETE", "/containers/" + CANDIDATE + "?force=true", expected=(204, 404))
    create(CANDIDATE, original, candidate=True)
    try:
        port = inspect(CANDIDATE)["NetworkSettings"]["Ports"]["8890/tcp"][0]["HostPort"]
        health(CANDIDATE, port)
        verify_mcp(port)
        print(json.dumps({"candidate": "verified", "api_version": API_VERSION}))
    finally:
        docker("DELETE", "/containers/" + CANDIDATE + "?force=true", expected=(204, 404))
elif args.action == "switch":
    snapshot = json.loads((BACKUP / "deployment-snapshot.json").read_text())
    original = snapshot["container"]
    assert data_signature() == snapshot["data_signature"], "data drift since snapshot"
    docker("POST", "/containers/" + NAME + "/stop?t=60", expected=(204, 304))
    docker("POST", "/containers/" + NAME + "/rename?name=" + OLD)
    try:
        create(NAME, original)
        assert health(NAME, 38462)["database"] == "connected"
    except Exception:
        docker("DELETE", "/containers/" + NAME + "?force=true", expected=(204, 404))
        docker("POST", "/containers/" + OLD + "/rename?name=" + NAME)
        docker("POST", "/containers/" + NAME + "/start")
        raise
    print(json.dumps({"production": "healthy", "rollback_container": OLD}))
elif args.action == "reswitch":
    snapshot = json.loads((BACKUP / "deployment-snapshot.json").read_text())
    original = snapshot["container"]
    assert data_signature() == snapshot["data_signature"], "data drift since snapshot"
    old_exists = subprocess.run(
        ["docker", "container", "inspect", OLD],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0
    assert not old_exists, "rollback container already exists"
    docker("POST", "/containers/" + NAME + "/stop?t=60", expected=(204, 304))
    docker("POST", "/containers/" + NAME + "/rename?name=" + OLD)
    try:
        create(NAME, original)
        assert health(NAME, 38462)["database"] == "connected"
    except Exception:
        docker("DELETE", "/containers/" + NAME + "?force=true", expected=(204, 404))
        docker("POST", "/containers/" + OLD + "/rename?name=" + NAME)
        docker("POST", "/containers/" + NAME + "/start")
        raise
    print(json.dumps({"production": "healthy", "image": IMAGE, "rollback_container": OLD}))
elif args.action == "redeploy":
    previous = NAME + "-pre-014-20261110"
    previous_exists = subprocess.run(
        ["docker", "container", "inspect", previous],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0
    assert not previous_exists, "previous 0.1.3 container already exists"
    original = inspect(NAME)
    before = data_signature()
    docker("POST", "/containers/" + NAME + "/stop?t=60", expected=(204, 304))
    docker("POST", "/containers/" + NAME + "/rename?name=" + previous)
    try:
        create(NAME, original)
        assert health(NAME, 38462)["database"] == "connected"
    except Exception:
        docker("DELETE", "/containers/" + NAME + "?force=true", expected=(204, 404))
        docker("POST", "/containers/" + previous + "/rename?name=" + NAME)
        docker("POST", "/containers/" + NAME + "/start")
        raise
    assert data_signature() == before, "data changed during 0.1.4 redeploy"
    print(json.dumps({"production": "healthy", "image": IMAGE, "previous_container": previous}))
elif args.action == "smoke":
    health(NAME, 38462)
    verify_mcp(38462)
    verify_pages(38462)
    print(json.dumps({
        "production": "verified",
        "service_version": SERVICE_VERSION,
        "api_version": API_VERSION,
        "data_signature_unchanged": True,
    }))
elif args.action == "verify":
    snapshot = json.loads((BACKUP / "deployment-snapshot.json").read_text())
    original = snapshot["container"]
    current = inspect(NAME)
    assert current["Config"]["Image"] == IMAGE, "production image mismatch"
    expected_env = dict(item.split("=", 1) for item in original["Config"]["Env"] if "=" in item)
    actual_env = dict(item.split("=", 1) for item in current["Config"]["Env"] if "=" in item)
    assert actual_env == expected_env, "environment drift"
    assert current["Mounts"] == original["Mounts"], "mount drift"
    assert set(current["NetworkSettings"]["Networks"]) == set(original["NetworkSettings"]["Networks"]), "network drift"
    health(NAME, 38462)
    verify_mcp(38462)
    verify_pages(38462)
    assert data_signature() == snapshot["data_signature"], "pre-existing production data changed"
    print(json.dumps({
        "production": "verified",
        "api_version": API_VERSION,
        "pages_alias": True,
        "deprecated_alias": True,
        "pre_existing_data_unchanged": True,
    }))
else:
    print(json.dumps({"cleanup": "manual approval required"}))
