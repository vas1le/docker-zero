#!/usr/bin/env python3
"""Portable release self-test for docker-zero.

This checks the binary, configuration diagnostics, Docker socket API, Nginx,
Redis, all three seeds, clean shutdown, and ledger readability without needing
Docker, redis-cli, curl, or third-party Python packages.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from integration_test import (
    RunningEngine,
    assert_docker_identity,
    assert_ledgers,
    docker_request,
    expect_http_drop,
    free_port,
    http_get,
    inspect,
    redis_command,
)


def check_configuration(binary: str, root: Path) -> None:
    nginx_port = free_port()
    redis_port = free_port()
    while redis_port == nginx_port:
        redis_port = free_port()
    command = [
        binary,
        "--check",
        "--socket",
        str(root / "check.sock"),
        "--ledger-dir",
        str(root / "check-runs"),
        "--nginx-address",
        f"127.0.0.1:{nginx_port}",
        "--redis-address",
        f"127.0.0.1:{redis_port}",
        "--container-endpoints",
        "off",
    ]
    completed = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=15, check=False)
    assert completed.returncode == 0, completed.stdout
    assert "CHECK OK" in completed.stdout, completed.stdout


def run_bad_cookbook_check(
    binary: str,
    project_root: Path,
    root: Path,
    directory_name: str,
    mutate,
    expected_markers: tuple[str, ...],
) -> None:
    cookbooks = root / directory_name
    cookbooks.mkdir()
    for source in (project_root / "cookbooks").glob("*.json"):
        shutil.copy2(source, cookbooks / source.name)
    nginx = cookbooks / "nginx.json"
    nginx.write_text(mutate(nginx.read_text(encoding="utf-8")), encoding="utf-8")
    completed = subprocess.run(
        [
            binary,
            "--check",
            "--cookbook-dir",
            str(cookbooks),
            "--socket",
            str(root / f"{directory_name}.sock"),
            "--ledger-dir",
            str(root / f"{directory_name}-runs"),
            "--nginx-address",
            f"127.0.0.1:{free_port()}",
            "--redis-address",
            f"127.0.0.1:{free_port()}",
            "--container-endpoints",
            "off",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=15,
        check=False,
    )
    assert completed.returncode == 2, completed.stdout
    for marker in expected_markers:
        assert marker in completed.stdout, completed.stdout


def check_diagnostics(binary: str, project_root: Path, root: Path) -> None:
    def make_syntax_error(text: str) -> str:
        return text.replace('"kind": "nginx",', '"kind": "nginx" "defaults":', 1)

    run_bad_cookbook_check(
        binary,
        project_root,
        root,
        "syntax-error-cookbooks",
        make_syntax_error,
        ("CONFIG_SYNTAX_ERROR", "line   :", "column :", "^"),
    )

    def make_unknown_field(text: str) -> str:
        return text.replace('"status": "running",', '"statuz": "running",', 1)

    run_bad_cookbook_check(
        binary,
        project_root,
        root,
        "unknown-field-cookbooks",
        make_unknown_field,
        ("CONFIG_UNKNOWN_FIELD", "/seeds/0/initial/statuz", "line   :", "column :", "^"),
    )


def check_seed(binary: str, seed: int) -> None:
    with RunningEngine(binary, seed=seed, bootstrap=True, container_endpoints="off") as engine:
        assert_docker_identity(engine)
        status, data, _ = docker_request(engine.socket_path, "GET", "/__docker_zero/state")
        assert status == 200, (status, data)
        state = json.loads(data)
        containers = {item["name"]: item for item in state["containers"]}
        assert set(containers) == {"nginx-zero", "redis-zero"}, containers
        assert all(item["seed"] == seed for item in containers.values()), containers

        if seed == 0:
            assert http_get("127.0.0.1", engine.nginx_port, "/health")[0] == 200
            reply, connection = redis_command("127.0.0.1", engine.redis_port, "PING")
            connection.close()
            assert reply == b"+PONG\r\n", reply
        elif seed == 1:
            codes = [http_get("127.0.0.1", engine.nginx_port, "/health")[0] for _ in range(3)]
            assert codes == [503, 503, 200], codes
            replies: list[bytes] = []
            for _ in range(3):
                reply, connection = redis_command("127.0.0.1", engine.redis_port, "PING")
                connection.close()
                replies.append(reply)
            assert replies == [
                b"-LOADING docker-zero is warming up\r\n",
                b"-LOADING docker-zero is warming up\r\n",
                b"+PONG\r\n",
            ], replies
        else:
            assert http_get("127.0.0.1", engine.nginx_port, "/health")[0] == 200
            expect_http_drop("127.0.0.1", engine.nginx_port, "/health")
            nginx_states = [inspect(engine.socket_path, "nginx-zero") for _ in range(3)]
            assert [(item["State"]["Status"], item["State"]["Health"]["Status"]) for item in nginx_states] == [
                ("restarting", "starting"),
                ("running", "starting"),
                ("running", "healthy"),
            ]
            reply, connection = redis_command("127.0.0.1", engine.redis_port, "PING")
            connection.close()
            assert reply == b"+PONG\r\n", reply
            reply, connection = redis_command("127.0.0.1", engine.redis_port, "PING")
            connection.close()
            assert reply == b"", reply
            redis_states = [inspect(engine.socket_path, "redis-zero") for _ in range(3)]
            assert [(item["State"]["Status"], item["State"]["Health"]["Status"]) for item in redis_states] == [
                ("restarting", "starting"),
                ("running", "starting"),
                ("running", "healthy"),
            ]
        assert_ledgers(engine, {"nginx-zero": seed, "redis-zero": seed})


def main() -> int:
    parser = argparse.ArgumentParser(description="Run docker-zero's binary self-test without Docker or third-party packages.")
    parser.add_argument("binary", type=Path)
    args = parser.parse_args()
    binary = args.binary.resolve()
    if not binary.is_file():
        print(f"FAIL: binary does not exist: {binary}", file=sys.stderr)
        return 2
    project_root = Path(__file__).resolve().parents[1]
    try:
        with tempfile.TemporaryDirectory(prefix="docker-zero-doctor-") as directory:
            root = Path(directory)
            check_configuration(str(binary), root)
            print("PASS preflight configuration and listener checks")
            check_diagnostics(str(binary), project_root, root)
            print("PASS source-level cookbook syntax and schema diagnostics")
            for seed in (0, 1, 2):
                check_seed(str(binary), seed)
                print(f"PASS Docker/Nginx/Redis/ledger seed {seed}")
    except (AssertionError, OSError, subprocess.SubprocessError) as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        return 1
    print("docker-zero doctor: ALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
