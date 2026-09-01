#!/usr/bin/env python3
"""Create one CPA Plugin Store archive and its sha256sum-format sidecar."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path
import zipfile


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--library", required=True)
    parser.add_argument("--archive", required=True)
    parser.add_argument("--entry", required=True)
    args = parser.parse_args()

    library = Path(args.library)
    archive = Path(args.archive)
    if not library.is_file():
        raise SystemExit(f"library does not exist: {library}")
    if "/" in args.entry or "\\" in args.entry:
        raise SystemExit("ZIP entry must be a root filename")

    archive.parent.mkdir(parents=True, exist_ok=True)
    entry = zipfile.ZipInfo(args.entry, date_time=(1980, 1, 1, 0, 0, 0))
    entry.compress_type = zipfile.ZIP_DEFLATED
    entry.create_system = 3
    entry.external_attr = (0o100755 & 0xFFFF) << 16
    with zipfile.ZipFile(archive, "w") as bundle:
        bundle.writestr(entry, library.read_bytes())

    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix(archive.suffix + ".sha256").write_text(
        f"{digest}  {archive.name}\n", encoding="utf-8"
    )


if __name__ == "__main__":
    main()
