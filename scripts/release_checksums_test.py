#!/usr/bin/env python3
"""Verify checksums from a flattened artifact, without the source dist/ path."""

from __future__ import annotations

import argparse
from pathlib import Path
import shutil
import subprocess
import tempfile


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, nargs="?", default=Path("dist"))
    args = parser.parse_args()
    files = ("docker-zero-linux-amd64", "docker-zero-linux-arm64", "SHA256SUMS")
    with tempfile.TemporaryDirectory(prefix="docker-zero-release-") as temp:
        root = Path(temp)
        for name in files:
            shutil.copyfile(args.directory / name, root / name)
        rows = (root / "SHA256SUMS").read_text(encoding="utf-8").splitlines()
        names = [row.split(maxsplit=1)[1].lstrip(" *") for row in rows]
        if sorted(names) != sorted(files[:2]):
            raise SystemExit(f"manifest must contain artifact-root filenames exactly once: {names}")
        subprocess.run(["sha256sum", "--strict", "--check", "SHA256SUMS"], cwd=root, check=True)
    print("PASS checksums validate in a clean extracted-artifact directory")


if __name__ == "__main__":
    main()
