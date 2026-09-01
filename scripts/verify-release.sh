#!/usr/bin/env bash
set -euo pipefail

DIST_DIR="${1:-dist}"
python3 - "$DIST_DIR" <<'PY'
from __future__ import annotations

import hashlib
from pathlib import Path
import re
import sys
import zipfile

root = Path(sys.argv[1])
pattern = re.compile(r"^account-health-pushover_[0-9]+\.[0-9]+\.[0-9]+_(linux|darwin|windows)_(amd64|arm64)\.zip$")
archives = sorted(root.glob("account-health-pushover_*.zip"))
if not archives:
    raise SystemExit("no release ZIP archives found")

for archive in archives:
    match = pattern.fullmatch(archive.name)
    if not match:
        raise SystemExit(f"invalid archive name: {archive.name}")
    goos = match.group(1)
    extension = {"linux": "so", "darwin": "dylib", "windows": "dll"}[goos]
    expected = f"account-health-pushover.{extension}"
    with zipfile.ZipFile(archive) as bundle:
        names = bundle.namelist()
        if names != [expected]:
            raise SystemExit(f"{archive.name}: expected only root entry {expected!r}, got {names!r}")
    sidecar = archive.with_suffix(archive.suffix + ".sha256")
    if sidecar.exists():
        expected_line = f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}"
        if sidecar.read_text(encoding="utf-8").strip() != expected_line:
            raise SystemExit(f"checksum sidecar mismatch: {sidecar.name}")

checksums = root / "checksums.txt"
if checksums.exists():
    listed = {}
    for line in checksums.read_text(encoding="utf-8").splitlines():
        digest, name = line.split(None, 1)
        listed[name.strip()] = digest
    for archive in archives:
        actual = hashlib.sha256(archive.read_bytes()).hexdigest()
        if listed.get(archive.name) != actual:
            raise SystemExit(f"checksums.txt mismatch or missing entry: {archive.name}")

print(f"verified {len(archives)} CPA Plugin Store archive(s)")
PY
