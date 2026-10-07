"""Coverage assert: the generated ogen Handler must implement every operation in
the normalized spec, 1:1. This is the CI guard against ogen silently skipping an
operation it cannot express (the failure mode found in the codegen probe).

Exits non-zero on any mismatch.
"""

from __future__ import annotations

import io
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SPEC = ROOT / "openapi.go.json"
SERVER = ROOT / "internal" / "api" / "oas_server_gen.go"

METHODS = ("get", "post", "put", "patch", "delete")


def pascal(oid: str) -> str:
    return "".join(part.capitalize() for part in re.split(r"[_\-]", oid) if part)


def main() -> int:
    spec = json.load(io.open(SPEC, encoding="utf-8"))
    spec_ops: dict[str, str] = {}
    for path, item in spec["paths"].items():
        for method, op in item.items():
            if method.lower() in METHODS and isinstance(op, dict):
                oid = op.get("operationId")
                if oid:
                    spec_ops[oid] = f"{method.upper()} {path}"

    text = io.open(SERVER, encoding="utf-8").read()
    start = text.index("type Handler interface {")
    end = text.index("\n}", start)
    block = text[start:end]
    have = set(re.findall(r"(?m)^\t([A-Za-z][A-Za-z0-9_]*)\(", block))

    missing = sorted(oid for oid in spec_ops if pascal(oid) not in have)
    extra = sorted(name for name in have if name not in {pascal(o) for o in spec_ops})

    print("spec operations      : %d" % len(spec_ops))
    print("handler methods      : %d" % len(have))

    if missing:
        print("\nMISSING in generated Handler (%d):" % len(missing))
        for oid in missing:
            print("  %-34s %s" % (oid, spec_ops[oid]))
    if extra:
        print("\nEXTRA in Handler, not in spec (%d):" % len(extra))
        for name in extra:
            print("  " + name)

    if missing or extra:
        print("\nFAIL: Handler surface does not match the spec 1:1")
        return 1
    print("\nOK: Handler surface matches the spec 1:1 (%d operations)" % len(spec_ops))
    return 0


if __name__ == "__main__":
    sys.exit(main())
