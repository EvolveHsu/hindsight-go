"""Read the active Docker config and probe the configured LLM with synthetic text.

Only response structure and a synthetic prefix are printed; credentials and
user memory contents never leave the NAS host.
"""
import json
import os
import subprocess
import tempfile


SYSTEM = """You extract durable atomic memories from raw content.
Return ONLY a JSON array. Do not wrap it in markdown.
Each item must have:
  "text": a self-contained fact, preserving names, dates, quantities and concrete details,
  "fact_type": "world" or "experience",
  "entities": an array of entity names mentioned in the fact,
  "occurred_start": ISO-8601 date/time or null,
  "occurred_end": ISO-8601 date/time or null.
Split compound sentences into separate facts. Do not invent facts. Omit transient filler."""

USER = """Context: synthetic-diagnostic

Content:
Synthetic ExtractorProbe20261117. The service uses port 4567. Alice moved to Berlin on 2024-01-15."""


def main():
    info = json.loads(subprocess.check_output(["docker", "inspect", "hindsight-go"]))[0]
    env = dict(
        item.split("=", 1)
        for item in info["Config"]["Env"]
        if "=" in item
    )
    variants = {
        "current": {
            "messages": [
                {"role": "system", "content": SYSTEM},
                {"role": "user", "content": USER},
            ],
        },
        "json_object": {
            "messages": [
                {"role": "system", "content": (
                    "You are a JSON-only extraction API. Return one JSON object "
                    'with exactly one key, "facts", whose value is an array of '
                    "fact objects. No markdown, code fence, prose or explanation."
                )},
                {"role": "user", "content": USER},
            ],
            "response_format": {"type": "json_object"},
        },
        "user_only": {
            "messages": [
                {"role": "user", "content": (
                    "Return ONLY valid JSON, no prose or markdown. "
                    'The JSON shape is {"facts":[{"text":"...",'
                    '"fact_type":"world|experience","entities":[],'
                    '"occurred_start":null,"occurred_end":null}]}.\n\n' + USER
                )},
            ],
            "response_format": {"type": "json_object"},
        },
    }
    with tempfile.TemporaryDirectory() as tmp:
        env_path = os.path.join(tmp, "env")
        with open(env_path, "w", encoding="utf-8") as fh:
            fh.write("KEY=" + env["HINDSIGHT_GO_LLM_API_KEY"] + "\n")
            fh.write("BASE=" + env["HINDSIGHT_GO_LLM_BASE_URL"].rstrip("/") + "\n")
        os.chmod(env_path, 0o600)
        for name, extra in variants.items():
            payload = {
                "model": env["HINDSIGHT_GO_LLM_MODEL"],
                "temperature": 0,
                "max_tokens": 2048,
                **extra,
            }
            payload_path = os.path.join(tmp, "payload.json")
            with open(payload_path, "w", encoding="utf-8") as fh:
                json.dump(payload, fh)
            os.chmod(payload_path, 0o600)
            shell = (
                'wget -qO- --header="Content-Type: application/json" '
                '--header="Authorization: Bearer $KEY" '
                '--post-file=/payload.json "$BASE/chat/completions"'
            )
            raw = subprocess.check_output([
                "docker", "run", "--rm", "--network", "new-api_default",
                "--env-file", env_path,
                "-v", payload_path + ":/payload.json:ro",
                "alpine:3.20", "sh", "-lc", shell,
            ], timeout=180).decode()
            data = json.loads(raw)
            choice = data["choices"][0]
            content = choice.get("message", {}).get("content") or ""
            print(json.dumps({
                "variant": name,
                "finish": choice.get("finish_reason"),
                "content_len": len(content),
                "first_char": content.lstrip()[:1],
                "prefix": content[:280],
                "usage": data.get("usage"),
            }, ensure_ascii=False))


if __name__ == "__main__":
    main()
