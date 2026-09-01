#!/usr/bin/env bash
set -euo pipefail

# Verify a CPA Plugin Store release bundle.
#
# Default (full) mode enforces the complete publishable contract:
#   - all five mandatory platform archives are present
#     (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64);
#   - every archive has exactly one root entry with the platform library name;
#   - every archive has a matching sha256sum-format .sha256 sidecar;
#   - checksums.txt exists with exactly one valid entry per archive and no
#     orphan entries;
#   - all archives share one X.Y.Z version.
#
# --partial mode relaxes only the "all five platforms" requirement so a
# single locally packaged platform can be verified with the same strictness.
#
# Usage: verify-release.sh [--partial] [dist-dir]

MODE="full"
if [[ "${1:-}" == "--partial" ]]; then
  MODE="partial"
  shift
fi
DIST_DIR="${1:-dist}"

python3 - "$MODE" "$DIST_DIR" <<'PY'
from __future__ import annotations

import hashlib
from pathlib import Path
import re
import sys
import zipfile

mode = sys.argv[1]
root = Path(sys.argv[2])

PLUGIN_ID = "account-health-pushover"
ARCHIVE_PATTERN = re.compile(
    r"^account-health-pushover_([0-9]+\.[0-9]+\.[0-9]+)_(linux|darwin|windows)_(amd64|arm64)\.zip$"
)
REQUIRED_PLATFORMS = {
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("darwin", "amd64"),
    ("darwin", "arm64"),
    ("windows", "amd64"),
}
EXTENSIONS = {"linux": "so", "darwin": "dylib", "windows": "dll"}
HEX_SHA256 = re.compile(r"^[0-9a-f]{64}$")


def fail(message: str) -> None:
    raise SystemExit(f"verify-release: {message}")


if not root.is_dir():
    fail(f"dist directory does not exist: {root}")

archives = sorted(root.glob(f"{PLUGIN_ID}_*.zip"))
if not archives:
    fail("no release ZIP archives found")

versions: set[str] = set()
platforms: set[tuple[str, str]] = set()
digests: dict[str, str] = {}

for archive in archives:
    match = ARCHIVE_PATTERN.fullmatch(archive.name)
    if not match:
        fail(f"invalid archive name: {archive.name}")
    version, goos, goarch = match.groups()
    if (goos, goarch) not in REQUIRED_PLATFORMS:
        fail(f"unsupported platform {goos}/{goarch}: {archive.name}")
    if (goos, goarch) in platforms:
        fail(f"duplicate platform {goos}/{goarch}: {archive.name}")
    versions.add(version)
    platforms.add((goos, goarch))

    expected_entry = f"{PLUGIN_ID}.{EXTENSIONS[goos]}"
    with zipfile.ZipFile(archive) as bundle:
        names = bundle.namelist()
        if names != [expected_entry]:
            fail(
                f"{archive.name}: expected only root entry {expected_entry!r}, got {names!r}"
            )

    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    digests[archive.name] = digest

    sidecar = archive.with_name(archive.name + ".sha256")
    if not sidecar.exists():
        fail(f"missing checksum sidecar: {sidecar.name}")
    expected_line = f"{digest}  {archive.name}"
    if sidecar.read_text(encoding="utf-8").strip() != expected_line:
        fail(f"checksum sidecar mismatch: {sidecar.name}")

if len(versions) != 1:
    fail(f"mixed release versions: {sorted(versions)}")

if mode == "full":
    missing = REQUIRED_PLATFORMS - platforms
    if missing:
        names = ", ".join(f"{goos}/{goarch}" for goos, goarch in sorted(missing))
        fail(f"missing required platform archive(s): {names}")

checksums = root / "checksums.txt"
if not checksums.exists():
    fail("missing checksums.txt")

listed: dict[str, str] = {}
for line_number, raw_line in enumerate(
    checksums.read_text(encoding="utf-8").splitlines(), start=1
):
    line = raw_line.strip()
    if not line:
        fail(f"checksums.txt line {line_number}: empty line")
    fields = line.split()
    if len(fields) != 2:
        fail(f"checksums.txt line {line_number}: invalid sha256sum entry")
    digest, name = fields
    name = name.lstrip("*")
    if not HEX_SHA256.fullmatch(digest):
        fail(f"checksums.txt line {line_number}: invalid lowercase sha256 digest")
    if name in listed:
        fail(f"checksums.txt line {line_number}: duplicate entry for {name}")
    listed[name] = digest

orphans = sorted(set(listed) - set(digests))
if orphans:
    fail(f"checksums.txt lists file(s) not present in dist: {orphans}")
unlisted = sorted(set(digests) - set(listed))
if unlisted:
    fail(f"checksums.txt is missing entry for: {unlisted}")
for name, digest in digests.items():
    if listed[name] != digest:
        fail(f"checksums.txt digest mismatch for {name}")

print(f"verified {len(archives)} CPA Plugin Store archive(s) in {mode} mode")
PY
