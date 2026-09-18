#!/usr/bin/env python3
"""Verify the flat file layout downloaded from releases and Actions artifacts."""
from __future__ import annotations

from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: checksums_test.py DIST_DIRECTORY")
    source = Path(sys.argv[1]).resolve()
    files = ("docker-zero-linux-amd64", "docker-zero-linux-arm64", "SHA256SUMS")
    with tempfile.TemporaryDirectory(prefix="docker-zero-package-") as directory:
        for name in files:
            shutil.copy2(source / name, Path(directory) / name)
        subprocess.run(["sha256sum", "--check", "SHA256SUMS"], cwd=directory, check=True)
    print("PASS checksums verify from an extracted flat artifact directory")


if __name__ == "__main__":
    main()
