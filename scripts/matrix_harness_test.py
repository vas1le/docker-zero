#!/usr/bin/env python3
"""Black-box tests for orchestrator_matrix.py classification and process control."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import time


def write_executable(path: Path, content: str) -> None:
    path.write_text(textwrap.dedent(content).lstrip(), encoding="utf-8")
    path.chmod(0o755)


def run_matrix(matrix: Path, engine: Path, target: list[str], root: Path, timeout: float = 5.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [
            sys.executable,
            str(matrix),
            "--engine",
            str(engine),
            "--results-dir",
            str(root),
            "--seeds",
            "0",
            "--timeout",
            str(timeout),
            "--container-endpoints",
            "off",
            "--",
            *target,
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=20.0,
        check=False,
    )


def load_only_result(root: Path) -> dict[str, object]:
    summary = json.loads((root / "summary.json").read_text(encoding="utf-8"))
    results = summary["results"]
    assert len(results) == 1, results
    return results[0]


def test_unsupported_classification(matrix: Path, engine: Path, temp: Path) -> None:
    target = temp / "unsupported_target.py"
    write_executable(
        target,
        r'''
        #!/usr/bin/env python3
        import os
        import socket

        path = os.environ["DOCKER_HOST"].removeprefix("unix://")
        request = b"GET /v1.43/not-implemented HTTP/1.1\r\nHost:x\r\nConnection:close\r\n\r\n"
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.connect(path)
            client.sendall(request)
            while client.recv(65536):
                pass
        ''',
    )
    root = temp / "unsupported-results"
    completed = run_matrix(matrix, engine, [sys.executable, str(target)], root)
    assert completed.returncode == 1, completed.stdout
    result = load_only_result(root)
    assert result["status"] == "mock_incomplete", (result, completed.stdout)
    assert result["unsupported_docker_requests"] == ["GET /not-implemented"], result


def test_metadata_only_classification(matrix: Path, engine: Path, temp: Path) -> None:
    target = temp / "metadata_target.py"
    write_executable(target, r'''
        #!/usr/bin/env python3
        import os
        import socket
        body = b'{"Image":"nginx:alpine","User":"1000"}'
        request = (b"POST /v1.43/containers/create HTTP/1.1\r\nHost:x\r\n"
                   b"Content-Type:application/json\r\nConnection:close\r\nContent-Length:"
                   + str(len(body)).encode() + b"\r\n\r\n" + body)
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.connect(os.environ["DOCKER_HOST"].removeprefix("unix://"))
            client.sendall(request)
            response = b""
            while data := client.recv(65536):
                response += data
        assert b"201 Created" in response, response
    ''')
    root = temp / "metadata-results"
    completed = run_matrix(matrix, engine, [sys.executable, str(target)], root)
    assert completed.returncode == 1, completed.stdout
    result = load_only_result(root)
    assert result["status"] == "mock_incomplete", (result, completed.stdout)
    assert result["unsupported_docker_requests"] == ["POST /containers/create"], result


def test_engine_death_and_log_flood(matrix: Path, temp: Path) -> None:
    fake_engine = temp / "fake_broken_engine.py"
    write_executable(
        fake_engine,
        r'''
        #!/usr/bin/env python3
        import argparse
        import os
        import socket
        import sys
        import time

        parser = argparse.ArgumentParser(add_help=False)
        parser.add_argument("--socket")
        parser.add_argument("--check", action="store_true")
        args, _ = parser.parse_known_args()
        if args.check:
            print("fake engine CHECK OK")
            raise SystemExit(0)

        try:
            os.unlink(args.socket)
        except FileNotFoundError:
            pass
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(args.socket)
        server.listen(4)
        server.settimeout(2)
        client, _ = server.accept()
        with client:
            client.recv(65536)
            client.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
        # More than a typical pipe buffer. The matrix must not deadlock because
        # engine output is connected directly to engine.log.
        sys.stdout.write("X" * (2 * 1024 * 1024))
        sys.stdout.flush()
        time.sleep(0.15)
        server.close()
        raise SystemExit(7)
        ''',
    )
    target = temp / "slow_target.py"
    write_executable(
        target,
        r'''
        #!/usr/bin/env python3
        import time
        time.sleep(30)
        ''',
    )
    root = temp / "engine-death-results"
    started = time.monotonic()
    completed = run_matrix(matrix, fake_engine, [sys.executable, str(target)], root, timeout=10.0)
    elapsed = time.monotonic() - started
    assert completed.returncode == 1, completed.stdout
    assert elapsed < 5.0, (elapsed, completed.stdout)
    result = load_only_result(root)
    assert result["status"] == "engine_error", (result, completed.stdout)
    assert result["engine_exit_code"] == 7, result
    engine_log = root / "nginx-0_redis-0" / "engine.log"
    assert engine_log.stat().st_size >= 2 * 1024 * 1024, engine_log.stat().st_size


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("binary", type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    matrix = root / "scripts" / "orchestrator_matrix.py"
    engine = args.binary.resolve()
    with tempfile.TemporaryDirectory(prefix="docker-zero-matrix-harness-") as directory:
        temp = Path(directory)
        test_unsupported_classification(matrix, engine, temp)
        print("PASS matrix classifies unsupported Docker calls as mock_incomplete")
        test_metadata_only_classification(matrix, engine, temp)
        print("PASS matrix rejects metadata-only success as mock_incomplete")
        test_engine_death_and_log_flood(matrix, temp)
        print("PASS matrix detects engine death and cannot deadlock on engine log output")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
