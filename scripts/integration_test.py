#!/usr/bin/env python3
"""Black-box verification for docker-zero using sockets and public APIs only."""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
from typing import Any, Iterable


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path: str, timeout: float = 3.0) -> None:
        super().__init__("localhost", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.socket_path)


def docker_request(
    socket_path: str,
    method: str,
    path: str,
    body: Any | None = None,
    *,
    timeout: float = 3.0,
) -> tuple[int, bytes, dict[str, str]]:
    connection = UnixHTTPConnection(socket_path, timeout=timeout)
    headers: dict[str, str] = {}
    payload: bytes | None = None
    if body is not None:
        payload = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    connection.request(method, path, body=payload, headers=headers)
    response = connection.getresponse()
    data = response.read()
    result_headers = {key.lower(): value for key, value in response.getheaders()}
    status = response.status
    connection.close()
    return status, data, result_headers


def free_port() -> int:
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return int(port)


def stat_is_socket(path: str) -> bool:
    import stat

    return stat.S_ISSOCK(os.stat(path).st_mode)


def wait_for_socket(path: str, process: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if process.poll() is not None:
            output = process.stdout.read() if process.stdout else ""
            raise AssertionError(f"docker-zero exited early ({process.returncode})\n{output}")
        if os.path.exists(path) and stat_is_socket(path):
            try:
                status, data, _ = docker_request(path, "GET", "/_ping")
            except (OSError, http.client.HTTPException):
                time.sleep(0.03)
                continue
            if status == 200 and data == b"OK":
                return
        time.sleep(0.03)
    raise AssertionError("Docker socket did not become ready")


def create_and_start(socket_path: str, name: str, image: str, host_port: int | None = None, *, restart_policy: str = "no") -> str:
    body: dict[str, Any] = {"Image": image}
    if host_port is not None:
        private_port = 80 if "nginx" in image else 6379
        body["HostConfig"] = {"PortBindings": {f"{private_port}/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(host_port)}]}}
    body.setdefault("HostConfig", {})["RestartPolicy"] = {"Name": restart_policy}
    status, data, _ = docker_request(socket_path, "POST", f"/v1.43/containers/create?name={name}", body)
    assert status == 201, (status, data)
    container_id = json.loads(data)["Id"]
    status, data, _ = docker_request(socket_path, "POST", f"/v1.43/containers/{name}/start")
    assert status == 204, (status, data)
    return str(container_id)


def inspect(socket_path: str, name: str) -> dict[str, Any]:
    status, data, _ = docker_request(socket_path, "GET", f"/v1.43/containers/{name}/json")
    assert status == 200, (status, data)
    return json.loads(data)


def list_containers(socket_path: str, *, all_containers: bool = False) -> list[dict[str, Any]]:
    suffix = "?all=1" if all_containers else ""
    status, data, _ = docker_request(socket_path, "GET", f"/v1.43/containers/json{suffix}")
    assert status == 200, (status, data)
    return list(json.loads(data))


def http_get(host: str, port: int, path: str) -> tuple[int, bytes, dict[str, str]]:
    connection = http.client.HTTPConnection(host, port, timeout=2)
    connection.request("GET", path)
    response = connection.getresponse()
    data = response.read()
    headers = {key.lower(): value for key, value in response.getheaders()}
    status = response.status
    connection.close()
    return status, data, headers


def expect_http_drop(host: str, port: int, path: str) -> None:
    try:
        http_get(host, port, path)
        raise AssertionError(f"expected {host}:{port}{path} connection to be dropped")
    except (http.client.RemoteDisconnected, ConnectionResetError, BrokenPipeError, ConnectionAbortedError):
        return


def redis_command(
    host: str,
    port: int,
    *parts: str,
    sock: socket.socket | None = None,
) -> tuple[bytes, socket.socket]:
    connection = sock or socket.create_connection((host, port), timeout=2)
    payload = f"*{len(parts)}\r\n".encode()
    for part in parts:
        encoded = part.encode()
        payload += f"${len(encoded)}\r\n".encode() + encoded + b"\r\n"
    connection.sendall(payload)
    reply = connection.recv(4096)
    return reply, connection


class RunningEngine:
    def __init__(
        self,
        binary: str,
        *,
        seed: int = 0,
        scenario: str = "",
        bootstrap: bool = False,
        container_endpoints: str = "off",
        cookbook_dir: str | None = None,
    ) -> None:
        self.root = Path(tempfile.mkdtemp(prefix=f"docker-zero-seed-{seed}-"))
        self.socket_path = str(self.root / "docker.sock")
        self.nginx_port = free_port()
        self.redis_port = free_port()
        self.command = [
            binary,
            "--socket",
            self.socket_path,
            "--seed",
            str(seed),
            "--scenario",
            scenario,
            "--ledger-dir",
            str(self.root / "runs"),
            "--nginx-address",
            f"127.0.0.1:{self.nginx_port}",
            "--redis-address",
            f"127.0.0.1:{self.redis_port}",
            "--container-endpoints",
            container_endpoints,
        ]
        if cookbook_dir is not None:
            self.command.extend(["--cookbook-dir", cookbook_dir])
        if bootstrap:
            self.command.append("--bootstrap")
        self.process: subprocess.Popen[str] | None = None
        self.output = ""

    def __enter__(self) -> "RunningEngine":
        self.process = subprocess.Popen(self.command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        wait_for_socket(self.socket_path, self.process)
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        assert self.process is not None
        self.process.send_signal(signal.SIGTERM)
        try:
            self.output, _ = self.process.communicate(timeout=6)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.output, _ = self.process.communicate(timeout=2)
            if exc is None:
                raise AssertionError("docker-zero did not terminate after SIGTERM")
        if self.process.returncode not in (0, -signal.SIGTERM) and exc is None:
            raise AssertionError(f"docker-zero exited {self.process.returncode}\n{self.output}")

    @property
    def run_dir(self) -> Path:
        matches = list((self.root / "runs").glob("seed-*"))
        assert len(matches) == 1, matches
        return matches[0]


def assert_docker_identity(engine: RunningEngine) -> None:
    status, data, headers = docker_request(engine.socket_path, "GET", "/version")
    assert status == 200
    version = json.loads(data)
    assert version["ApiVersion"] == "1.43"
    assert version["Platform"]["Name"] == "Docker Zero"
    assert headers["ostype"] == "linux"
    status, data, _ = docker_request(engine.socket_path, "HEAD", "/_ping")
    assert status == 200 and data == b""


def assert_ledgers(engine: RunningEngine, expected_scenarios: dict[str, int]) -> None:
    run_meta = json.loads((engine.run_dir / "run.json").read_text())
    assert run_meta["name"] == "docker-zero"
    assert "overrides" in run_meta

    global_lines = (engine.run_dir / "global.ledger.jsonl").read_text().splitlines()
    assert global_lines
    parsed = [json.loads(line) for line in global_lines]
    sequences = [entry["seq"] for entry in parsed]
    assert sequences == list(range(1, len(sequences) + 1)), sequences
    assert any(entry["channel"] == "docker.http" for entry in parsed), parsed

    for name, scenario in expected_scenarios.items():
        path = engine.run_dir / "containers" / f"{name}.ledger.jsonl"
        assert path.exists(), path
        entries = [json.loads(line) for line in path.read_text().splitlines()]
        assert entries, path
        assert all(entry["container"] == name for entry in entries)
        assert all(entry["scenario"] == scenario for entry in entries), entries


def run_seed(binary: str, seed: int) -> None:
    # Seed tests use both published endpoints and the cookbook's direct
    # container IP:port endpoints. The two listeners share one state machine.
    with RunningEngine(binary, seed=seed, container_endpoints="on") as engine:
        assert_docker_identity(engine)
        create_and_start(engine.socket_path, "nginx-zero", "nginx:alpine", engine.nginx_port, restart_policy="always")
        create_and_start(engine.socket_path, "redis-zero", "redis:7-alpine", engine.redis_port, restart_policy="always")

        nginx_inspect = inspect(engine.socket_path, "nginx-zero")
        redis_inspect = inspect(engine.socket_path, "redis-zero")
        nginx_ip = nginx_inspect["NetworkSettings"]["IPAddress"]
        redis_ip = redis_inspect["NetworkSettings"]["IPAddress"]
        assert nginx_ip.startswith("127.") and redis_ip.startswith("127.") and nginx_ip != redis_ip
        assert nginx_inspect["NetworkSettings"]["Ports"]["80/tcp"][0]["HostPort"] == str(engine.nginx_port)
        assert redis_inspect["NetworkSettings"]["Ports"]["6379/tcp"][0]["HostPort"] == str(engine.redis_port)

        if seed == 0:
            assert http_get("127.0.0.1", engine.nginx_port, "/health")[0] == 200
            assert http_get(nginx_ip, 80, "/health")[0] == 200

            reply, redis_sock = redis_command("127.0.0.1", engine.redis_port, "PING")
            assert reply == b"+PONG\r\n", reply
            redis_sock.close()
            reply, redis_sock = redis_command(redis_ip, 6379, "PING")
            assert reply == b"+PONG\r\n", reply
            reply, redis_sock = redis_command(redis_ip, 6379, "SET", "alpha", "1", sock=redis_sock)
            assert reply == b"+OK\r\n", reply
            reply, redis_sock = redis_command(redis_ip, 6379, "GET", "alpha", sock=redis_sock)
            assert reply == b"$1\r\n1\r\n", reply
            redis_sock.close()

            first = inspect(engine.socket_path, "nginx-zero")
            first_pid = first["State"]["Pid"]
            status, _, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/nginx-zero/restart")
            assert status == 204
            restarted = inspect(engine.socket_path, "nginx-zero")
            assert restarted["State"]["Status"] == "running"
            assert restarted["State"]["Health"]["Status"] == "healthy"
            assert restarted["RestartCount"] == 1
            assert restarted["State"]["Pid"] != first_pid

            status, _, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/nginx-zero/stop")
            assert status == 204
            status, _, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/nginx-zero/stop")
            assert status == 304
            status, _, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/nginx-zero/start")
            assert status == 204
            started_again = inspect(engine.socket_path, "nginx-zero")
            assert started_again["RestartCount"] == 1
            status, _, _ = docker_request(engine.socket_path, "POST", "/v1.43/containers/nginx-zero/start")
            assert status == 304

            status, network_data, _ = docker_request(
                engine.socket_path,
                "POST",
                "/v1.43/networks/create",
                {"Name": "zero-net", "Driver": "bridge"},
            )
            assert status == 201, network_data
            status, volume_data, _ = docker_request(
                engine.socket_path,
                "POST",
                "/v1.43/volumes/create",
                {"Name": "zero-vol"},
            )
            assert status == 201 and json.loads(volume_data)["Name"] == "zero-vol"

        elif seed == 1:
            # Alternating published/direct calls verifies that both endpoints
            # drive the same per-container cookbook and counter.
            nginx_codes = [
                http_get("127.0.0.1", engine.nginx_port, "/health")[0],
                http_get(nginx_ip, 80, "/health")[0],
                http_get("127.0.0.1", engine.nginx_port, "/health")[0],
            ]
            assert nginx_codes == [503, 503, 200], nginx_codes
            assert http_get(nginx_ip, 80, "/nginx_status")[0] == 200

            replies: list[bytes] = []
            for host, port in (("127.0.0.1", engine.redis_port), (redis_ip, 6379), ("127.0.0.1", engine.redis_port)):
                reply, redis_sock = redis_command(host, port, "PING")
                redis_sock.close()
                replies.append(reply)
            assert replies == [
                b"-LOADING docker-zero is warming up\r\n",
                b"-LOADING docker-zero is warming up\r\n",
                b"+PONG\r\n",
            ], replies
            assert inspect(engine.socket_path, "nginx-zero")["State"]["Health"]["Status"] == "healthy"
            assert inspect(engine.socket_path, "redis-zero")["State"]["Health"]["Status"] == "healthy"

        elif seed == 2:
            assert http_get("127.0.0.1", engine.nginx_port, "/health")[0] == 200
            expect_http_drop(nginx_ip, 80, "/health")
            nginx_states = [inspect(engine.socket_path, "nginx-zero") for _ in range(3)]
            observed = [(item["State"]["Status"], item["State"]["Health"]["Status"]) for item in nginx_states]
            assert observed == [("restarting", "starting"), ("running", "starting"), ("running", "healthy")], observed
            assert nginx_states[-1]["RestartCount"] == 1
            assert http_get(nginx_ip, 80, "/health")[0] == 200

            reply, redis_sock = redis_command("127.0.0.1", engine.redis_port, "PING")
            assert reply == b"+PONG\r\n", reply
            redis_sock.close()
            reply, redis_sock = redis_command(redis_ip, 6379, "PING")
            assert reply == b"", reply
            redis_sock.close()
            redis_states = [inspect(engine.socket_path, "redis-zero") for _ in range(3)]
            observed = [(item["State"]["Status"], item["State"]["Health"]["Status"]) for item in redis_states]
            assert observed == [("restarting", "starting"), ("running", "starting"), ("running", "healthy")], observed
            reply, redis_sock = redis_command(redis_ip, 6379, "PING")
            assert reply == b"+PONG\r\n", reply
            redis_sock.close()

        status, state_data, _ = docker_request(engine.socket_path, "GET", "/__docker_zero/state")
        state = json.loads(state_data)
        assert status == 200 and len(state["containers"]) == 2
        assert {item["name"]: item["seed"] for item in state["containers"]} == {"nginx-zero": seed, "redis-zero": seed}
        assert_ledgers(engine, {"nginx-zero": seed, "redis-zero": seed})


def run_matrix(binary: str) -> None:
    """Verify all 3x3 per-container cookbook combinations."""
    for nginx_seed in range(3):
        for redis_seed in range(3):
            scenario = f"nginx-zero={nginx_seed},redis={redis_seed}"
            with RunningEngine(binary, seed=0, scenario=scenario, container_endpoints="off") as engine:
                create_and_start(engine.socket_path, "nginx-zero", "nginx:alpine")
                create_and_start(engine.socket_path, "redis-zero", "redis:7-alpine")
                status, data, _ = docker_request(engine.socket_path, "GET", "/__docker_zero/state")
                assert status == 200
                selected = {item["name"]: item["seed"] for item in json.loads(data)["containers"]}
                assert selected == {"nginx-zero": nginx_seed, "redis-zero": redis_seed}, (scenario, selected)
                expected_health = lambda seed: "starting" if seed == 1 else "healthy"
                # Admin state does not advance lifecycle edges.
                admin_state = {item["name"]: item["state"]["health"] for item in json.loads(data)["containers"]}
                assert admin_state == {
                    "nginx-zero": expected_health(nginx_seed),
                    "redis-zero": expected_health(redis_seed),
                }, (scenario, admin_state)
                assert_ledgers(engine, {"nginx-zero": nginx_seed, "redis-zero": redis_seed})


def run_list_driven_restart(binary: str) -> None:
    """A Docker list operation can observe/progress restart state, not only inspect."""
    with RunningEngine(binary, seed=2, container_endpoints="off") as engine:
        create_and_start(engine.socket_path, "nginx-zero", "nginx:alpine", engine.nginx_port, restart_policy="always")
        create_and_start(engine.socket_path, "redis-zero", "redis:7-alpine", engine.redis_port, restart_policy="always")
        assert http_get("127.0.0.1", engine.nginx_port, "/health")[0] == 200
        expect_http_drop("127.0.0.1", engine.nginx_port, "/health")
        states: list[tuple[str, str]] = []
        for _ in range(3):
            listed = list_containers(engine.socket_path)
            nginx = next(item for item in listed if item["Names"] == ["/nginx-zero"])
            state = inspect_state_from_list_status(nginx)
            states.append(state)
        assert states == [("restarting", "starting"), ("running", "starting"), ("running", "healthy")], states


def inspect_state_from_list_status(item: dict[str, Any]) -> tuple[str, str]:
    status_text = str(item["Status"])
    if "(" in status_text and status_text.endswith(")"):
        health = status_text.rsplit("(", 1)[1][:-1]
    elif item["State"] == "restarting":
        health = "starting"
    else:
        health = ""
    return str(item["State"]), health


def run_concurrency_stress(binary: str) -> None:
    with RunningEngine(binary, seed=0, container_endpoints="off") as engine:
        create_and_start(engine.socket_path, "nginx-zero", "nginx:alpine", engine.nginx_port, restart_policy="always")
        create_and_start(engine.socket_path, "redis-zero", "redis:7-alpine", engine.redis_port, restart_policy="always")

        def nginx_call(_: int) -> None:
            status, _, _ = http_get("127.0.0.1", engine.nginx_port, "/health")
            assert status == 200

        def redis_call(_: int) -> None:
            reply, redis_sock = redis_command("127.0.0.1", engine.redis_port, "PING")
            redis_sock.close()
            assert reply == b"+PONG\r\n"

        calls = 200
        with ThreadPoolExecutor(max_workers=32) as pool:
            list(pool.map(nginx_call, range(calls)))
            list(pool.map(redis_call, range(calls)))

        status, state_data, _ = docker_request(engine.socket_path, "GET", "/__docker_zero/state")
        assert status == 200
        containers = {item["name"]: item for item in json.loads(state_data)["containers"]}
        assert containers["nginx-zero"]["counters"]["http.GET /health"] == calls
        assert containers["redis-zero"]["counters"]["redis.PING"] == calls
        assert_ledgers(engine, {"nginx-zero": 0, "redis-zero": 0})


def run_bootstrap(binary: str) -> None:
    with RunningEngine(binary, seed=1, bootstrap=True, container_endpoints="off") as engine:
        status, data, _ = docker_request(engine.socket_path, "GET", "/__docker_zero/state")
        assert status == 200
        containers = json.loads(data)["containers"]
        assert {item["name"] for item in containers} == {"nginx-zero", "redis-zero"}
        assert all(item["state"]["status"] == "running" for item in containers)
        assert_ledgers(engine, {"nginx-zero": 1, "redis-zero": 1})



def run_external_cookbook(binary: str) -> None:
    cookbook_root = Path(tempfile.mkdtemp(prefix="docker-zero-cookbooks-"))
    project_root = Path(__file__).resolve().parents[1]
    for source in (project_root / "cookbooks").glob("*.json"):
        target = cookbook_root / source.name
        target.write_bytes(source.read_bytes())

    nginx_path = cookbook_root / "nginx.json"
    nginx = json.loads(nginx_path.read_text())
    custom_body = "{\"status\":\"custom-external-cookbook\"}\n"
    nginx["seeds"]["0"]["http"]["GET /health"][0]["body"] = custom_body
    nginx_path.write_text(json.dumps(nginx, indent=2) + "\n")

    with RunningEngine(binary, seed=0, cookbook_dir=str(cookbook_root), container_endpoints="off") as engine:
        create_and_start(engine.socket_path, "nginx-zero", "nginx:alpine", engine.nginx_port, restart_policy="always")
        status, body, _ = http_get("127.0.0.1", engine.nginx_port, "/health")
        assert status == 200
        assert body.decode() == custom_body


def run_unsupported_request_trace(binary: str) -> None:
    with RunningEngine(binary, seed=0, container_endpoints="off") as engine:
        status, data, headers = docker_request(engine.socket_path, "GET", "/v1.43/not-implemented")
        assert status == 404, (status, data)
        assert headers.get("x-docker-zero-unsupported") == "true", headers
        entries = [json.loads(line) for line in (engine.run_dir / "global.ledger.jsonl").read_text().splitlines()]
        matches = [entry for entry in entries if entry["channel"] == "docker.http" and entry["event"] == "GET /not-implemented"]
        assert matches, entries
        assert matches[-1]["response"]["status"] == 404
        assert matches[-1]["response"]["unsupported"] is True

def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("binary")
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())

    for seed in (0, 1, 2):
        run_seed(binary, seed)
        print(f"PASS full seed {seed}")
    run_matrix(binary)
    print("PASS all 9 per-container seed combinations")
    run_list_driven_restart(binary)
    print("PASS list-driven restart lifecycle")
    run_concurrency_stress(binary)
    print("PASS concurrent service stress")
    run_bootstrap(binary)
    print("PASS bootstrap and ledger verification")
    run_external_cookbook(binary)
    print("PASS external cookbook loading")
    run_unsupported_request_trace(binary)
    print("PASS unsupported Docker request tracing")
    print("PASS all docker-zero black-box tests")


if __name__ == "__main__":
    main()
