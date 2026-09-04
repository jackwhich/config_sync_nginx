#!/usr/bin/env python3
"""Generate frontend-manifest.json in one site's already-built artifact directory."""
import hashlib
import json
from pathlib import Path
import re
import sys


def generate(site):
    site = Path(site)
    if not (site / "index.html").is_file():
        raise ValueError("index.html required")
    assets = {}
    for path in sorted(site.rglob("*")):
        if path.is_symlink():
            raise ValueError("symlinks prohibited: " + str(path))
        if path.is_dir():
            continue
        name = path.relative_to(site).as_posix()
        if name in {"index.html", "frontend-manifest.json"}:
            continue
        if not name.startswith("assets/") or not re.search(r"[.-][a-fA-F0-9]{8,64}(?:[.-]|$)", path.name):
            raise ValueError("every resource must have a hexadecimal content hash in its assets/ filename: " + name)
        assets[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    if not assets:
        raise ValueError("at least one hashed asset required")
    (site / "frontend-manifest.json").write_text(json.dumps({"assets": assets}, indent=2) + "\n")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: frontend-manifest.py <site-directory>")
    try:
        generate(sys.argv[1])
    except (OSError, ValueError) as error:
        raise SystemExit(str(error))
