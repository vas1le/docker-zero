#!/usr/bin/env python3
"""Run an unchanged orchestrator against every docker-zero seed combination.

The target receives DOCKER_HOST pointing at a fresh fake Docker socket. Each
case preserves target output, engine output, Docker/API ledgers, and a machine-
readable result. Engine defects and unsupported mock operations are classified
separately so they are never reported as orchestrator failures.
"""

from __future__ import annotations

import argparse
import itertools
import json
import os
from pathlib import Path
import signal
import shutil
import socket
import stat
import subprocess
import sys
import time
from typing import Any, Sequence, TextIO


class UnixHTTPClient:
    def __init__(self, socket_path: Path, timeout: float = 2.0) -> None:
        self.socket_path = socket_path
        self.timeout = timeout

    def get(self, path: str) -> tuple[int, bytes]:
        request = (
            f"GET {path} HTTP/1.1\r\n"
            "Host: localhost\r\n"
            "Connection: close\r\n\r\n"
        ).encode()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(self.timeout)
            connection.connect(str(self.socket_path))
            connection.sendall(request)
            chunks: list[bytes] = []
            while True:
                block = connection.recv(65536)
                if not block:
                    break
                chunks.append(block)
        raw = b"".join(chunks)
        head, separator, body = raw.partition(b"\r\n\r\n")
        if not separator or not head.splitlines():
            raise RuntimeError("empty or malformed HTTP response from docker-zero")
        first = head.splitlines()[0].decode("ascii", errors="replace")
        try:
            status = int(first.split()[1])
        except (IndexError, ValueError) as exc:
            raise RuntimeError(f"invalid HTTP response from docker-zero: {first!r}") from exc
        return status, body


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def tail_text(path: Path, limit: int = 64 * 1024) -> str:
    try:
        with path.open("rb") as handle:
            handle.seek(0, os.SEEK_END)
            size = handle.tell()
            handle.seek(max(0, size - limit))
            data = handle.read()
        return data.decode("utf-8", errors="replace")
    except OSError as exc:
        return f"<unable to read {path}: {exc}>"


def wait_for_engine(
    socket_path: Path,
    process: subprocess.Popen[str],
    log_path: Path,
    timeout: float = 8.0,
) -> None:
    deadline = time.monotonic() + timeout
    client = UnixHTTPClient(socket_path)
    while time.monotonic() < deadline:
        return_code = process.poll()
        if return_code is not None:
            raise RuntimeError(
                f"docker-zero exited before readiness ({return_code})\n{tail_text(log_path)}"
            )
        try:
            if socket_path.exists() and stat.S_ISSOCK(socket_path.stat().st_mode):
                status, body = client.get("/_ping")
                if status == 200 and body == b"OK":
                    return
        except (OSError, RuntimeError):
            pass
        time.sleep(0.025)
    raise TimeoutError(
        f"docker-zero did not become ready at {socket_path}\n{tail_text(log_path)}"
    )


def signal_process_group(process: subprocess.Popen[str], sig: signal.Signals) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, sig)
    except ProcessLookupError:
        return


def stop_process_group(process: subprocess.Popen[str], grace: float = 5.0) -> int:
    if process.poll() is None:
        signal_process_group(process, signal.SIGTERM)
        try:
            process.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            signal_process_group(process, signal.SIGKILL)
            process.wait(timeout=grace)
    return int(process.returncode if process.returncode is not None else -999)


def kill_process_group(process: subprocess.Popen[str]) -> None:
    if process.poll() is None:
        signal_process_group(process, signal.SIGKILL)
        try:
            process.wait(timeout=5.0)
        except subprocess.TimeoutExpired as exc:
            raise RuntimeError(f"could not terminate process group {process.pid}") from exc


def parse_seed_list(value: str) -> list[int]:
    result: list[int] = []
    for item in value.split(","):
        item = item.strip()
        if not item:
            continue
        try:
            seed = int(item)
        except ValueError as exc:
            raise argparse.ArgumentTypeError(f"invalid seed {item!r}") from exc
        if seed not in (0, 1, 2):
            raise argparse.ArgumentTypeError("seeds must be selected from 0,1,2")
        if seed not in result:
            result.append(seed)
    if not result:
        raise argparse.ArgumentTypeError("at least one seed is required")
    return result


def latest_ledger_run(ledger_root: Path) -> Path | None:
    candidates = sorted(
        (path for path in ledger_root.glob("seed-*") if path.is_dir()),
        key=lambda path: path.stat().st_mtime_ns,
    )
    return candidates[-1].resolve() if candidates else None


def inspect_engine_ledger(run_dir: Path | None) -> tuple[list[dict[str, Any]], list[dict[str, Any]], str | None]:
    unsupported: list[dict[str, Any]] = []
    internal_errors: list[dict[str, Any]] = []
    parse_error: str | None = None
    if run_dir is None:
        return unsupported, internal_errors, None
    path = run_dir / "global.ledger.jsonl"
    if not path.is_file():
        return unsupported, internal_errors, f"global ledger is missing: {path}"
    try:
        with path.open("r", encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError as exc:
                    parse_error = f"invalid ledger JSON at {path}:{line_number}: {exc}"
                    break
                response = entry.get("response")
                if (
                    entry.get("channel") == "docker.http"
                    and isinstance(response, dict)
                    and response.get("unsupported") is True
                ):
                    unsupported.append(entry)
                if entry.get("channel") == "docker-zero.internal":
                    internal_errors.append(entry)
    except OSError as exc:
        parse_error = f"cannot read global ledger {path}: {exc}"
    return unsupported, internal_errors, parse_error


def preflight_engine(engine_command: Sequence[str], case_dir: Path) -> tuple[bool, str | None]:
    check_command = [*engine_command, "--check"]
    try:
        completed = subprocess.run(
            check_command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=15.0,
            check=False,
        )
        output = completed.stdout
    except subprocess.TimeoutExpired as exc:
        output = (exc.stdout or "") + (exc.stderr or "")
        (case_dir / "preflight.log").write_text(output, encoding="utf-8")
        return False, f"docker-zero preflight exceeded 15 seconds\n{output}"
    (case_dir / "preflight.log").write_text(output, encoding="utf-8")
    if completed.returncode != 0:
        return False, f"docker-zero preflight failed ({completed.returncode})\n{output}"
    return True, None


def monitor_target_and_engine(
    target: subprocess.Popen[str],
    engine: subprocess.Popen[str],
    timeout: float,
    engine_log_path: Path,
) -> tuple[str, int | None, str | None]:
    deadline = time.monotonic() + timeout
    while True:
        engine_code = engine.poll()
        if engine_code is not None:
            kill_process_group(target)
            return (
                "engine_error",
                None,
                f"docker-zero exited while the target was running ({engine_code})\n{tail_text(engine_log_path)}",
            )

        target_code = target.poll()
        if target_code is not None:
            # Check once more after target exit to avoid a race where both failed.
            engine_code = engine.poll()
            if engine_code is not None:
                return (
                    "engine_error",
                    target_code,
                    f"docker-zero exited while the target was completing ({engine_code})\n{tail_text(engine_log_path)}",
                )
            return ("passed" if target_code == 0 else "failed", target_code, None)

        if time.monotonic() >= deadline:
            kill_process_group(target)
            return "timeout", None, f"target exceeded {timeout:.3f}s"
        time.sleep(0.025)


def run_case(
    *,
    engine_binary: Path,
    command: Sequence[str],
    results_root: Path,
    nginx_seed: int,
    redis_seed: int,
    timeout: float,
    container_endpoints: str,
    extra_env: dict[str, str],
) -> dict[str, Any]:
    case_name = f"nginx-{nginx_seed}_redis-{redis_seed}"
    case_dir = results_root / case_name
    if case_dir.exists():
        shutil.rmtree(case_dir)
    case_dir.mkdir(parents=True, exist_ok=True)
    socket_path = case_dir / "docker.sock"
    ledger_root = case_dir / "engine-ledgers"
    nginx_port = free_port()
    redis_port = free_port()
    while redis_port == nginx_port:
        redis_port = free_port()

    engine_command = [
        str(engine_binary),
        "--socket",
        str(socket_path),
        "--seed",
        "0",
        "--scenario",
        f"nginx={nginx_seed},redis={redis_seed}",
        "--ledger-dir",
        str(ledger_root),
        "--nginx-address",
        f"127.0.0.1:{nginx_port}",
        "--redis-address",
        f"127.0.0.1:{redis_port}",
        "--container-endpoints",
        container_endpoints,
    ]

    started = time.monotonic()
    stdout_path = case_dir / "target.stdout.log"
    stderr_path = case_dir / "target.stderr.log"
    engine_log_path = case_dir / "engine.log"
    stdout_path.write_text("", encoding="utf-8")
    stderr_path.write_text("", encoding="utf-8")
    engine_log_path.write_text("", encoding="utf-8")

    status = "engine_error"
    exit_code: int | None = None
    engine_exit_code: int | None = None
    error: str | None = None
    engine: subprocess.Popen[str] | None = None

    preflight_ok, preflight_error = preflight_engine(engine_command, case_dir)
    if not preflight_ok:
        error = preflight_error
    else:
        with engine_log_path.open("w", encoding="utf-8") as engine_log:
            engine = subprocess.Popen(
                engine_command,
                stdout=engine_log,
                stderr=subprocess.STDOUT,
                text=True,
                start_new_session=True,
            )
            try:
                wait_for_engine(socket_path, engine, engine_log_path)
                environment = os.environ.copy()
                environment.update(extra_env)
                environment.update(
                    {
                        "DOCKER_HOST": f"unix://{socket_path}",
                        "DOCKER_ZERO_CASE": case_name,
                        "DOCKER_ZERO_NGINX_SEED": str(nginx_seed),
                        "DOCKER_ZERO_REDIS_SEED": str(redis_seed),
                        "DOCKER_ZERO_NGINX_ADDRESS": f"127.0.0.1:{nginx_port}",
                        "DOCKER_ZERO_REDIS_ADDRESS": f"127.0.0.1:{redis_port}",
                        "DOCKER_ZERO_RESULTS_DIR": str(case_dir.resolve()),
                    }
                )
                # Direct-to-file output avoids pipe backpressure and preserves
                # partial evidence when a process hangs or is killed.
                with stdout_path.open("w", encoding="utf-8") as stdout_file, stderr_path.open(
                    "w", encoding="utf-8"
                ) as stderr_file:
                    target = subprocess.Popen(
                        list(command),
                        env=environment,
                        stdout=stdout_file,
                        stderr=stderr_file,
                        text=True,
                        start_new_session=True,
                    )
                    status, exit_code, error = monitor_target_and_engine(
                        target, engine, timeout, engine_log_path
                    )
            except Exception as exc:  # preserve evidence for harness/engine failures
                error = f"{type(exc).__name__}: {exc}"
                status = "engine_error"
            finally:
                engine_exit_code = stop_process_group(engine)

    duration = time.monotonic() - started
    ledger_run = latest_ledger_run(ledger_root)
    unsupported, internal_errors, ledger_parse_error = inspect_engine_ledger(ledger_run)

    if engine is not None and ledger_run is None:
        status = "engine_error"
        error = error or f"docker-zero did not create a ledger under {ledger_root}"
    elif internal_errors:
        status = "engine_error"
        error = error or f"docker-zero recorded {len(internal_errors)} internal error(s)"
    elif ledger_parse_error:
        status = "engine_error"
        error = error or ledger_parse_error
    elif unsupported:
        status = "mock_incomplete"
        sample = unsupported[0]
        error = (
            f"docker-zero does not implement {len(unsupported)} Docker request(s); "
            f"first: {sample.get('event', '<unknown>')}"
        )

    if engine is not None and engine_exit_code not in (0, None) and status != "engine_error":
        status = "engine_error"
        error = f"docker-zero shutdown returned {engine_exit_code}\n{tail_text(engine_log_path)}"

    result: dict[str, Any] = {
        "case": case_name,
        "nginx_seed": nginx_seed,
        "redis_seed": redis_seed,
        "status": status,
        "exit_code": exit_code,
        "engine_exit_code": engine_exit_code,
        "duration_seconds": round(duration, 6),
        "docker_host": f"unix://{socket_path}",
        "engine_command": engine_command,
        "target_command": list(command),
        "ledger_run": str(ledger_run) if ledger_run else None,
        "unsupported_docker_requests": [entry.get("event") for entry in unsupported],
        "internal_engine_errors": len(internal_errors),
        "error": error,
    }
    (case_dir / "result.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run an unchanged orchestrator against docker-zero's Nginx/Redis scenario matrix."
    )
    parser.add_argument("--engine", required=True, type=Path, help="path to the docker-zero binary")
    parser.add_argument(
        "--results-dir",
        type=Path,
        default=Path("./matrix-runs"),
        help="directory for per-case evidence and summary.json",
    )
    parser.add_argument("--seeds", type=parse_seed_list, default=[0, 1, 2], help="comma-separated seed IDs")
    parser.add_argument("--timeout", type=float, default=300.0, help="target command timeout per case")
    parser.add_argument(
        "--container-endpoints",
        choices=("auto", "on", "off"),
        default="auto",
        help="whether docker-zero binds fake container IP:port endpoints",
    )
    parser.add_argument("--stop-on-failure", action="store_true", help="stop after the first non-passing case")
    parser.add_argument(
        "--env",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="extra environment variable for the target; may be repeated",
    )
    parser.add_argument("command", nargs=argparse.REMAINDER, help="target command after --")
    return parser


def parse_env(items: Sequence[str]) -> dict[str, str]:
    result: dict[str, str] = {}
    for item in items:
        if "=" not in item:
            raise ValueError(f"invalid --env {item!r}; expected KEY=VALUE")
        key, value = item.split("=", 1)
        if not key:
            raise ValueError("environment variable name cannot be empty")
        if "\x00" in key or "=" in key:
            raise ValueError(f"invalid environment variable name {key!r}")
        result[key] = value
    return result


def main() -> int:
    args = build_parser().parse_args()
    engine = args.engine.resolve()
    if not engine.is_file() or not os.access(engine, os.X_OK):
        print(f"error: engine is not an executable file: {engine}", file=sys.stderr)
        return 2
    command = list(args.command)
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        print("error: target command is required after --", file=sys.stderr)
        return 2
    if args.timeout <= 0:
        print("error: --timeout must be > 0", file=sys.stderr)
        return 2
    try:
        extra_env = parse_env(args.env)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    results_root = args.results_dir.resolve()
    results_root.mkdir(parents=True, exist_ok=True)
    started_at = time.time()
    results: list[dict[str, Any]] = []

    for nginx_seed, redis_seed in itertools.product(args.seeds, repeat=2):
        result = run_case(
            engine_binary=engine,
            command=command,
            results_root=results_root,
            nginx_seed=nginx_seed,
            redis_seed=redis_seed,
            timeout=args.timeout,
            container_endpoints=args.container_endpoints,
            extra_env=extra_env,
        )
        results.append(result)
        marker = "PASS" if result["status"] == "passed" else "FAIL"
        details = f"exit={result['exit_code']}" if result["exit_code"] is not None else (result["error"] or "")
        print(f"{marker} {result['case']} [{result['status']}] {details}".rstrip(), flush=True)
        if args.stop_on_failure and result["status"] != "passed":
            break

    counts: dict[str, int] = {}
    for result in results:
        counts[result["status"]] = counts.get(result["status"], 0) + 1
    summary = {
        "name": "docker-zero orchestrator matrix",
        "started_at_unix": started_at,
        "finished_at_unix": time.time(),
        "engine": str(engine),
        "target_command": command,
        "selected_seeds": args.seeds,
        "counts": counts,
        "results": results,
    }
    (results_root / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")

    print(f"Results: {results_root}")
    print("Summary: " + ", ".join(f"{key}={value}" for key, value in sorted(counts.items())))
    return 0 if results and all(result["status"] == "passed" for result in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
