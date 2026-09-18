#!/usr/bin/env python3
"""Exercise exec framing/exit results and wait with the real Docker CLI."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from integration_test import RunningEngine, create_and_start, docker_request


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("--docker", default=os.environ.get("DOCKER_BIN", "docker"))
    args = parser.parse_args()
    project = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="docker-zero-cli-") as tmp:
        root = Path(tmp)
        cookbooks = root / "cookbooks"
        cookbooks.mkdir()
        config = root / "docker-config"
        config.mkdir()
        for source in (project / "cookbooks").glob("*.json"):
            doc = json.loads(source.read_text(encoding="utf-8"))
            doc["seeds"]["0"]["exec"] = [{
                "cmd": ["sh", "-c", "exit 42"],
                "stdout": "stdout fixture\n",
                "stderr": "stderr fixture\n",
                "exit_code": 42,
            }]
            (cookbooks / source.name).write_text(json.dumps(doc), encoding="utf-8")
        with RunningEngine(str(args.binary.resolve()), seed=0, cookbook_dir=str(cookbooks), container_endpoints="off") as engine:
            create_and_start(engine.socket_path, "cli-contract", "nginx:alpine")
            env = os.environ.copy()
            env["DOCKER_HOST"] = "unix://" + engine.socket_path
            env["DOCKER_CONFIG"] = str(config)
            env.pop("DOCKER_CONTEXT", None)
            env.pop("DOCKER_TLS_VERIFY", None)
            result = subprocess.run([args.docker, "exec", "cli-contract", "sh", "-c", "exit 42"], env=env, capture_output=True, text=True, timeout=10, check=False)
            assert (result.returncode, result.stdout, result.stderr) == (42, "stdout fixture\n", "stderr fixture\n"), result
            print("PASS real Docker CLI exec: stdout/stderr framing and exit 42")
            unmatched = subprocess.run([args.docker, "exec", "cli-contract", "true"], env=env, capture_output=True, text=True, timeout=10, check=False)
            assert unmatched.returncode != 0 and "no explicit exec fixture" in unmatched.stderr, unmatched
            print("PASS real Docker CLI rejects unconfigured exec")
            waiter = subprocess.Popen([args.docker, "wait", "cli-contract"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                time.sleep(0.15)
                assert waiter.poll() is None, "docker wait completed while still running"
                status, body, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/cli-contract/kill")
                assert status == 204, (status, body)
                stdout, stderr = waiter.communicate(timeout=5)
                assert (waiter.returncode, stdout, stderr) == (0, "137\n", ""), (waiter.returncode, stdout, stderr)
                print("PASS real Docker CLI wait blocks and returns actual kill status")
            finally:
                if waiter.poll() is None:
                    waiter.kill()
                    waiter.communicate(timeout=5)


if __name__ == "__main__":
    main()
