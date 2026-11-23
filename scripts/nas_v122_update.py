"""NAS-only deployment helper for hindsight-go v1.2.2 / service 0.1.6."""
import argparse
import copy
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import time
import urllib.request

BASE = Path("/vol1/1000/docker/hindsight-go")
SOURCE = BASE / "source-v1.2.2-20261123"
BACKUP = Path("/vol1/1000/docker/hindsight-go-backup-v1.2.2-20261123")
NAME = "hindsight-go"
CANDIDATE = "hindsight-go-candidate-v122"
OLD = "hindsight-go-rollback-v122-20261123"
IMAGE = "hindsight-go:v1.2.2-20261123"
API_VERSION = "1.2.2"
SERVICE_VERSION = "0.1.6"


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


def request(port, method, path, body=None, timeout=180):
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
    env["HINDSIGHT_GO_LLM_CONTEXT_TOKENS"] = "1000000"
    env["HINDSIGHT_GO_RETAIN_MAX_TOKENS"] = "8192"
    env["HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT"] = "32768"
    if candidate:
        env.pop("HINDSIGHT_GO_DATABASE_URL", None)
        env.pop("HINDSIGHT_GO_UPSTREAM_SCHEMA", None)
        env["HINDSIGHT_GO_FILE_ROOT"] = "/tmp/hindsight-v122-files"
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


def canary_window():
    bank = "hindsight-v122-canary-20261123"
    base = "/v1/default/banks/" + bank
    before = data_signature()
    request(38462, "PUT", base, {})
    try:
        normal = request(38462, "POST", base + "/memories", {
            "items": [{
                "content": "SyntheticV122Canary. Hindsight uses port 38462. Alice moved to Berlin on 2024-01-15.",
                "document_id": "v122-normal-doc",
            }],
        })
        assert normal.get("success") is True and normal.get("items_count", 0) > 0, "normal retain failed"
        exact_text = "ExactV122Canary: preserve this text unchanged."
        exact = request(38462, "POST", base + "/memories", {
            "items": [{
                "content": exact_text,
                "document_id": "v122-exact-doc",
                "strategy": "exact",
            }],
        })
        assert exact.get("success") is True, "exact retain failed"
        exact_doc = request(38462, "GET", base + "/documents/v122-exact-doc")
        assert exact_doc.get("original_text") == exact_text, "exact document changed"

        tree = request(38462, "GET", "/v1/default/banks/codex/observations/tree")
        assert tree.get("title") == "Observations", "observation tree missing root title"
        assert isinstance(tree.get("files"), list), "observation tree missing files"
    finally:
        request(38462, "DELETE", base)
    assert data_signature() == before, "canary cleanup changed pre-existing data"


parser = argparse.ArgumentParser()
parser.add_argument("action", choices=("snapshot", "candidate", "switch", "verify", "delete-backup"))
args = parser.parse_args()
os.umask(0o077)

if args.action == "snapshot":
    assert not BACKUP.exists(), "backup exists"
    BACKUP.mkdir(mode=0o700)
    snapshot = BACKUP / "deployment-snapshot.json"
    original = inspect(NAME)
    (BACKUP / "container-inspect.json").write_text(json.dumps(original, indent=2))
    snapshot.write_text(json.dumps({
        "container": original,
        "data_signature": data_signature(),
        "image": original["Config"]["Image"],
    }, indent=2))
    subprocess.run([
        "tar", "-C", str(BASE.parent), "-czf", str(BACKUP / "project-config.tar.gz"),
        "--exclude=hindsight-go/deploy/data", "hindsight-go",
    ], check=True)
    subprocess.run([
        "tar", "-C", str(BASE), "-czf", str(BACKUP / "data-files.tar.gz"),
        "deploy/data/files",
    ], check=True)
    with (BACKUP / "hindsight-postgres.dump").open("wb") as output:
        subprocess.run([
            "docker", "exec", "hindsight-postgres", "pg_dump", "-U", "hindsight",
            "-d", "hindsight", "-Fc",
        ], stdout=output, check=True)
    subprocess.run([
        "docker", "image", "inspect", original["Config"]["Image"],
        "--format", "{{.Id}}",
    ], stdout=(BACKUP / "image-id.txt").open("w"), check=True)
    files = sorted(path for path in BACKUP.iterdir() if path.name != "SHA256SUMS")
    with (BACKUP / "SHA256SUMS").open("w") as output:
        subprocess.run(["sha256sum", *map(str, files)], stdout=output, check=True)
    for path in BACKUP.iterdir():
        path.chmod(0o600)
    print(json.dumps({"snapshot": str(snapshot), "postgres_dump": True}))
elif args.action == "candidate":
    original = json.loads((BACKUP / "deployment-snapshot.json").read_text())["container"]
    docker("DELETE", "/containers/" + CANDIDATE + "?force=true", expected=(204, 404))
    create(CANDIDATE, original, candidate=True)
    try:
        port = inspect(CANDIDATE)["NetworkSettings"]["Ports"]["8890/tcp"][0]["HostPort"]
        health(CANDIDATE, port)
        print(json.dumps({"candidate": "verified", "service_version": SERVICE_VERSION}))
    finally:
        docker("DELETE", "/containers/" + CANDIDATE + "?force=true", expected=(204, 404))
elif args.action == "switch":
    snapshot = json.loads((BACKUP / "deployment-snapshot.json").read_text())
    assert data_signature() == snapshot["data_signature"], "data drift since snapshot"
    old_exists = subprocess.run(
        ["docker", "container", "inspect", OLD],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0
    assert not old_exists, "rollback container exists"
    docker("POST", "/containers/" + NAME + "/stop?t=60", expected=(204, 304))
    docker("POST", "/containers/" + NAME + "/rename?name=" + OLD)
    try:
        create(NAME, snapshot["container"])
        assert health(NAME, 38462)["database"] == "connected"
    except Exception:
        docker("DELETE", "/containers/" + NAME + "?force=true", expected=(204, 404))
        docker("POST", "/containers/" + OLD + "/rename?name=" + NAME)
        docker("POST", "/containers/" + NAME + "/start")
        raise
    print(json.dumps({"production": "healthy", "image": IMAGE, "rollback_container": OLD}))
elif args.action == "verify":
    snapshot = json.loads((BACKUP / "deployment-snapshot.json").read_text())
    original = snapshot["container"]
    current = inspect(NAME)
    assert current["Config"]["Image"] == IMAGE, "production image mismatch"
    actual_env = dict(item.split("=", 1) for item in current["Config"]["Env"] if "=" in item)
    assert actual_env.get("HINDSIGHT_GO_LLM_CONTEXT_TOKENS") == "1000000"
    assert current["Mounts"] == original["Mounts"], "mount drift"
    assert set(current["NetworkSettings"]["Networks"]) == set(original["NetworkSettings"]["Networks"]), "network drift"
    health(NAME, 38462)
    canary_window()
    assert data_signature() == snapshot["data_signature"], "pre-existing data changed"
    print(json.dumps({
        "production": "verified",
        "service_version": SERVICE_VERSION,
        "api_version": API_VERSION,
        "retain": True,
        "exact_retain": True,
        "observation_tree": True,
        "pre_existing_data_unchanged": True,
    }))
else:
    target = Path("/vol1/1000/docker/hindsight-go-backup-v1.2.2-20261123")
    assert target == BACKUP
    if target.exists():
        shutil.rmtree(target)
    print(json.dumps({"backup_deleted": not target.exists()}))
