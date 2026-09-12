#!/usr/bin/env python3
"""Real Docker Compose compatibility tests for docker-zero topology/runtime behavior.

This suite intentionally drives docker-zero through an upstream Docker Compose
binary. It does not parse Compose files itself.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import tempfile
import time
from typing import Any
from urllib.parse import quote


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path: str, timeout: float = 5.0) -> None:
        super().__init__("localhost", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.socket_path)


def docker_request(socket_path: str, method: str, path: str, body: Any | None = None) -> tuple[int, bytes, dict[str, str]]:
    conn = UnixHTTPConnection(socket_path)
    headers: dict[str, str] = {}
    payload: bytes | None = None
    if body is not None:
        payload = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    conn.request(method, path, body=payload, headers=headers)
    response = conn.getresponse()
    data = response.read()
    result_headers = {k.lower(): v for k, v in response.getheaders()}
    status = response.status
    conn.close()
    return status, data, result_headers


def wait_engine(socket_path: str, process: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError(f"docker-zero exited early with {process.returncode}")
        if Path(socket_path).exists():
            try:
                status, data, _ = docker_request(socket_path, "GET", "/_ping")
                if status == 200 and data == b"OK":
                    return
            except OSError:
                pass
        time.sleep(0.03)
    raise AssertionError("docker-zero socket never became ready")


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def http_get(port: int, path: str = "/health") -> tuple[int, bytes, dict[str, str]]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    conn.request("GET", path)
    response = conn.getresponse()
    data = response.read()
    headers = {k.lower(): v for k, v in response.getheaders()}
    status = response.status
    conn.close()
    return status, data, headers


def recv_redis(sock: socket.socket) -> bytes:
    sock.settimeout(3)
    data = bytearray()
    first = sock.recv(1)
    if not first:
        return b""
    data.extend(first)
    if first in (b"+", b"-", b":"):
        while not data.endswith(b"\r\n"):
            data.extend(sock.recv(4096))
        return bytes(data)
    if first == b"$":
        while b"\r\n" not in data:
            data.extend(sock.recv(4096))
        header, rest = bytes(data).split(b"\r\n", 1)
        length = int(header[1:])
        if length < 0:
            return bytes(data)
        need = length + 2
        while len(rest) < need:
            rest += sock.recv(4096)
        return header + b"\r\n" + rest[:need]
    if first == b"*":
        # ROLE is not required by this suite. Keep generic reads bounded.
        time.sleep(0.02)
        try:
            data.extend(sock.recv(65536))
        except socket.timeout:
            pass
        return bytes(data)
    return bytes(data)


def redis_command(port: int, *parts: str) -> bytes:
    payload = f"*{len(parts)}\r\n".encode()
    for part in parts:
        raw = part.encode()
        payload += f"${len(raw)}\r\n".encode() + raw + b"\r\n"
    with socket.create_connection(("127.0.0.1", port), timeout=3) as sock:
        sock.sendall(payload)
        return recv_redis(sock)


def redis_bulk_value(reply: bytes) -> str | None:
    if reply.startswith(b"$-1\r\n"):
        return None
    if not reply.startswith(b"$"):
        raise AssertionError(f"expected Redis bulk reply, got {reply!r}")
    header, payload = reply.split(b"\r\n", 1)
    length = int(header[1:])
    return payload[:length].decode()


def redis_info_text(port: int) -> str:
    reply = redis_command(port, "INFO", "replication")
    value = redis_bulk_value(reply)
    if value is None:
        raise AssertionError("INFO replication returned null")
    return value


def inspect(socket_path: str, container: str) -> dict[str, Any]:
    status, data, _ = docker_request(socket_path, "GET", f"/v1.43/containers/{quote(container, safe='')}/json")
    assert status == 200, (container, status, data)
    return json.loads(data)


def project_containers(socket_path: str, project: str) -> list[dict[str, Any]]:
    status, data, _ = docker_request(socket_path, "GET", "/v1.43/containers/json?all=1")
    assert status == 200, (status, data)
    return [item for item in json.loads(data) if item.get("Labels", {}).get("com.docker.compose.project") == project]


def service_containers(socket_path: str, project: str, service: str) -> list[dict[str, Any]]:
    items = [item for item in project_containers(socket_path, project) if item.get("Labels", {}).get("com.docker.compose.service") == service]
    return sorted(items, key=lambda item: item["Names"][0])


def container_name(item: dict[str, Any]) -> str:
    return str(item["Names"][0]).lstrip("/")


def primary_ip(inspected: dict[str, Any]) -> str:
    networks = inspected["NetworkSettings"]["Networks"]
    values = [value["IPAddress"] for value in networks.values() if value.get("IPAddress")]
    assert values, inspected
    return str(values[0])


def published_port(inspected: dict[str, Any], private_port: int) -> int:
    bindings = inspected["NetworkSettings"]["Ports"].get(f"{private_port}/tcp")
    assert bindings and len(bindings) == 1, inspected["NetworkSettings"]["Ports"]
    return int(bindings[0]["HostPort"])


class Suite:
    def __init__(self, engine: Path, compose: list[str]) -> None:
        self.engine = engine
        self.compose = compose
        self.root = Path(tempfile.mkdtemp(prefix="docker-zero-compose-topology-"))
        self.socket = str(self.root / "docker.sock")
        self.ledger = self.root / "ledgers"
        self.engine_log = self.root / "engine.log"
        self.engine_handle = self.engine_log.open("w", encoding="utf-8")
        self.process = subprocess.Popen(
            [str(engine), "--socket", self.socket, "--seed", "0", "--ledger-dir", str(self.ledger), "--container-endpoints", "off"],
            stdout=self.engine_handle,
            stderr=subprocess.STDOUT,
            text=True,
        )
        wait_engine(self.socket, self.process)
        self.env = os.environ.copy()
        self.env["DOCKER_HOST"] = f"unix://{self.socket}"
        self.env["COMPOSE_PROGRESS"] = "plain"
        self.env["COMPOSE_ANSI"] = "never"

    def close(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        self.engine_handle.close()

    def write_compose(self, project: str, content: str) -> Path:
        directory = self.root / project
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / "compose.yaml"
        path.write_text(content, encoding="utf-8")
        return path

    def run_compose(self, project: str, path: Path, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        cmd = [*self.compose, "-f", str(path), "-p", project, *args]
        result = subprocess.run(cmd, env=self.env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=45)
        if check and result.returncode != 0:
            raise AssertionError(f"Compose failed ({result.returncode}): {' '.join(cmd)}\n{result.stdout}")
        return result

    def down(self, project: str, path: Path, *extra: str) -> None:
        self.run_compose(project, path, "down", "--remove-orphans", *extra, check=False)

    def dns(self, requester: str, name: str) -> list[dict[str, Any]]:
        path = f"/__docker_zero/dns?from={quote(requester)}&name={quote(name)}"
        status, data, _ = docker_request(self.socket, "GET", path)
        assert status == 200, (status, data)
        return list(json.loads(data)["answers"])

    def test_scale_distinct_ips_and_dns(self) -> None:
        project = "toposcale"
        path = self.write_compose(project, """services:\n  nginx:\n    image: nginx:alpine\n""")
        try:
            self.run_compose(project, path, "up", "-d", "--scale", "nginx=3")
            items = service_containers(self.socket, project, "nginx")
            assert len(items) == 3, items
            names = [container_name(item) for item in items]
            ips = [primary_ip(inspect(self.socket, name)) for name in names]
            assert len(set(ips)) == 3, ips
            answers = self.dns(names[0], "nginx")
            assert {item["ip"] for item in answers} == set(ips), (answers, ips)
            print("PASS Compose scale: distinct replica IPs + service DNS")
        finally:
            self.down(project, path)

    def test_ephemeral_ports_route_per_replica(self) -> None:
        project = "topoports"
        path = self.write_compose(project, """services:\n  nginx:\n    image: nginx:alpine\n    ports:\n      - \"80\"\n""")
        try:
            self.run_compose(project, path, "up", "-d", "--scale", "nginx=3")
            items = service_containers(self.socket, project, "nginx")
            assert len(items) == 3
            ports: list[int] = []
            for item in items:
                name = container_name(item)
                port = published_port(inspect(self.socket, name), 80)
                ports.append(port)
                status, _, headers = http_get(port)
                assert status == 200
                assert headers.get("x-docker-zero-container") == name, (name, port, headers)
            assert len(set(ports)) == 3, ports

            # Explicit restart must be synchronous: the endpoint is immediately live.
            target = container_name(items[0])
            target_port = published_port(inspect(self.socket, target), 80)
            service = items[0]["Labels"]["com.docker.compose.service"]
            self.run_compose(project, path, "restart", service)
            assert published_port(inspect(self.socket, target), 80) == target_port
            assert http_get(target_port)[0] == 200
            print("PASS ephemeral ports: unique, replica-routed, restart-stable")
        finally:
            self.down(project, path)

    def test_fixed_port_conflicts(self) -> None:
        project = "topofixed"
        port = free_port()
        path = self.write_compose(project, f"""services:\n  nginx:\n    image: nginx:alpine\n    ports:\n      - \"{port}:80\"\n""")
        try:
            result = self.run_compose(project, path, "up", "-d", "--scale", "nginx=3", check=False)
            assert result.returncode != 0, result.stdout
            assert "port is already allocated" in result.stdout.lower(), result.stdout
        finally:
            self.down(project, path)

        # Also prove conflicts with a real host process are detected.
        project = "topoexternal"
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as occupied:
            occupied.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            occupied.bind(("127.0.0.1", 0))
            occupied.listen(1)
            port = int(occupied.getsockname()[1])
            path = self.write_compose(project, f"""services:\n  nginx:\n    image: nginx:alpine\n    ports:\n      - \"{port}:80\"\n""")
            result = self.run_compose(project, path, "up", "-d", check=False)
            assert result.returncode != 0, result.stdout
            assert "port is already allocated" in result.stdout.lower(), result.stdout
        self.down(project, path)
        print("PASS fixed ports: replica conflicts + real host conflicts rejected")

    def test_cross_project_collisions(self) -> None:
        # A fixed host port is a daemon-global resource: one Compose project
        # must not be able to steal it from another project.
        port = free_port()
        project_a = "collisiona"
        project_b = "collisionb"
        compose_text = f"""services:\n  nginx:\n    image: nginx:alpine\n    ports:\n      - \"{port}:80\"\n"""
        path_a = self.write_compose(project_a, compose_text)
        path_b = self.write_compose(project_b, compose_text)
        try:
            self.run_compose(project_a, path_a, "up", "-d")
            owner = container_name(service_containers(self.socket, project_a, "nginx")[0])
            assert http_get(port)[0] == 200

            contender = self.run_compose(project_b, path_b, "up", "-d", check=False)
            assert contender.returncode != 0, contender.stdout
            assert "port is already allocated" in contender.stdout.lower(), contender.stdout
            assert http_get(port)[0] == 200
            assert container_name(service_containers(self.socket, project_a, "nginx")[0]) == owner
        finally:
            self.down(project_b, path_b)
            self.down(project_a, path_a)

        # Container names are also daemon-global. A collision must reject the
        # second create without replacing the first project's container.
        project_a = "namecollisiona"
        project_b = "namecollisionb"
        compose_text = """services:
  nginx:
    image: nginx:alpine
    container_name: docker-zero-shared-name
"""
        path_a = self.write_compose(project_a, compose_text)
        path_b = self.write_compose(project_b, compose_text)
        try:
            self.run_compose(project_a, path_a, "up", "-d")
            first = inspect(self.socket, "docker-zero-shared-name")
            first_id = first["Id"]
            contender = self.run_compose(project_b, path_b, "up", "-d", check=False)
            assert contender.returncode != 0, contender.stdout
            assert "already in use" in contender.stdout.lower(), contender.stdout
            assert inspect(self.socket, "docker-zero-shared-name")["Id"] == first_id
        finally:
            self.down(project_b, path_b)
            self.down(project_a, path_a)
        print("PASS cross-project collisions: fixed port + global container name")

    def test_network_delete_with_active_endpoints(self) -> None:
        project_a = "activeneta"
        project_b = "activenetb"
        path_a = self.write_compose(project_a, """services:
  nginx:
    image: nginx:alpine
    ports:
      - "80"
""")
        path_b = self.write_compose(project_b, """services:
  redis:
    image: redis:7-alpine
""")
        network_a = f"{project_a}_default"
        try:
            self.run_compose(project_a, path_a, "up", "-d")
            self.run_compose(project_b, path_b, "up", "-d")
            nginx_name = container_name(service_containers(self.socket, project_a, "nginx")[0])
            redis_name = container_name(service_containers(self.socket, project_b, "redis")[0])
            nginx_port = published_port(inspect(self.socket, nginx_name), 80)

            status, data, _ = docker_request(self.socket, "DELETE", f"/v1.43/networks/{quote(network_a, safe='')}")
            assert status == 500, (status, data)
            assert b"has active endpoints" in data.lower(), data
            assert nginx_name.encode() in data, data
            assert http_get(nginx_port)[0] == 200
            status, _, _ = docker_request(self.socket, "GET", f"/v1.43/networks/{quote(network_a, safe='')}")
            assert status == 200

            # Attach a foreign running container. Compose may remove its own
            # container, but it must not be able to remove a network still used
            # by that foreign endpoint.
            status, data, _ = docker_request(
                self.socket,
                "POST",
                f"/v1.43/networks/{quote(network_a, safe='')}/connect",
                {"Container": redis_name, "EndpointConfig": {"Aliases": ["foreign-redis"]}},
            )
            assert status == 200, (status, data)

            down = self.run_compose(project_a, path_a, "down", check=False)
            # Compose v5.5.0 reports the failed network removal as a progress
            # resource error but currently exits 0. The daemon-level assertion
            # below is authoritative: the network must still exist and remain
            # attached to the foreign running container.
            lower = down.stdout.lower()
            assert "resource is still in use" in lower or "active endpoints" in lower, down.stdout
            status, data, _ = docker_request(self.socket, "GET", f"/v1.43/networks/{quote(network_a, safe='')}")
            assert status == 200, (status, data)
            redis_doc = inspect(self.socket, redis_name)
            assert redis_doc["State"]["Running"] is True
            assert network_a in redis_doc["NetworkSettings"]["Networks"]

            status, data, _ = docker_request(
                self.socket,
                "POST",
                f"/v1.43/networks/{quote(network_a, safe='')}/disconnect",
                {"Container": redis_name, "Force": True},
            )
            assert status == 200, (status, data)
            self.run_compose(project_a, path_a, "down")
            status, _, _ = docker_request(self.socket, "GET", f"/v1.43/networks/{quote(network_a, safe='')}")
            assert status == 404
        finally:
            self.down(project_a, path_a)
            self.down(project_b, path_b)
        print("PASS active-endpoint deletion: direct rm + Compose down collision preserve foreign running container")

    def test_network_isolation(self) -> None:
        project = "toponets"
        path = self.write_compose(project, """services:\n  left:\n    image: nginx:alpine\n    networks: [leftnet]\n  middle:\n    image: nginx:alpine\n    networks: [leftnet, rightnet]\n  right:\n    image: nginx:alpine\n    networks: [rightnet]\nnetworks:\n  leftnet: {}\n  rightnet: {}\n""")
        try:
            self.run_compose(project, path, "up", "-d")
            left = container_name(service_containers(self.socket, project, "left")[0])
            middle = container_name(service_containers(self.socket, project, "middle")[0])
            right = container_name(service_containers(self.socket, project, "right")[0])
            assert len(self.dns(left, "middle")) == 1
            assert self.dns(left, "right") == []
            assert len(self.dns(middle, "left")) == 1
            assert len(self.dns(middle, "right")) == 1
            assert self.dns(right, "left") == []
            assert len(self.dns(right, "middle")) == 1
            print("PASS network-scoped DNS isolation across two Compose networks")
        finally:
            self.down(project, path)

    def test_redis_replication(self) -> None:
        project = "toporedis"
        path = self.write_compose(project, """services:\n  redis-master:\n    image: redis:7-alpine\n    ports:\n      - \"6379\"\n  redis-replica:\n    image: redis:7-alpine\n    command: [\"redis-server\", \"--replicaof\", \"redis-master\", \"6379\"]\n    ports:\n      - \"6379\"\n""")
        try:
            self.run_compose(project, path, "up", "-d", "--scale", "redis-replica=2")
            master_item = service_containers(self.socket, project, "redis-master")[0]
            replica_items = service_containers(self.socket, project, "redis-replica")
            assert len(replica_items) == 2
            master_name = container_name(master_item)
            master_port = published_port(inspect(self.socket, master_name), 6379)
            replica_names = [container_name(item) for item in replica_items]
            replica_ports = [published_port(inspect(self.socket, name), 6379) for name in replica_names]

            info = redis_info_text(master_port)
            assert "role:master" in info and "connected_slaves:2" in info, info
            for port in replica_ports:
                info = redis_info_text(port)
                assert "role:slave" in info, info
                assert "master_host:redis-master" in info, info
                assert "master_link_status:up" in info, info

            assert redis_command(master_port, "SET", "docker-zero-key", "replicated") == b"+OK\r\n"
            for port in replica_ports:
                assert redis_bulk_value(redis_command(port, "GET", "docker-zero-key")) == "replicated"
                readonly = redis_command(port, "SET", "forbidden", "value")
                assert readonly.startswith(b"-READONLY"), readonly

            dns_answers = self.dns(replica_names[0], "redis-master")
            assert len(dns_answers) == 1 and dns_answers[0]["container"] == master_name, dns_answers

            self.run_compose(project, path, "stop", "redis-master")
            for port in replica_ports:
                assert "master_link_status:down" in redis_info_text(port)
            self.run_compose(project, path, "start", "redis-master")
            assert redis_command(master_port, "PING") == b"+PONG\r\n"
            for port in replica_ports:
                assert "master_link_status:up" in redis_info_text(port)
                assert redis_bulk_value(redis_command(port, "GET", "docker-zero-key")) == "replicated"
            print("PASS Redis replication: DNS link, data propagation, READONLY, down/up")
        finally:
            self.down(project, path)

    def test_scale_80(self) -> None:
        project = "topo80"
        path = self.write_compose(project, """services:\n  nginx:\n    image: nginx:alpine\n""")
        try:
            self.run_compose(project, path, "up", "-d", "--scale", "nginx=80")
            items = service_containers(self.socket, project, "nginx")
            assert len(items) == 80, len(items)
            names = [container_name(item) for item in items]
            ips = [primary_ip(inspect(self.socket, name)) for name in names]
            assert len(set(ips)) == 80
            answers = self.dns(names[0], "nginx")
            assert len(answers) == 80 and len({answer["ip"] for answer in answers}) == 80
            print("PASS 80-replica Compose topology + 80 DNS answers")
        finally:
            self.down(project, path)

    def assert_no_unsupported(self) -> None:
        unsupported: list[dict[str, Any]] = []
        for ledger in self.ledger.rglob("global.ledger.jsonl"):
            for line in ledger.read_text(encoding="utf-8").splitlines():
                entry = json.loads(line)
                if entry.get("response", {}).get("unsupported") is True:
                    unsupported.append(entry)
        assert not unsupported, unsupported[:10]
        print("PASS real Compose suite recorded zero unsupported Docker API calls")


def resolve_compose_command(value: str) -> list[str]:
    parts = shlex.split(value)
    if not parts:
        raise ValueError("empty Compose command")
    if Path(parts[0]).name == "docker" and (len(parts) == 1 or parts[1] != "compose"):
        parts.append("compose")
    return parts


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("engine", help="docker-zero Linux binary")
    parser.add_argument("--compose", default=os.environ.get("COMPOSE_BIN", "docker-compose"), help="Compose binary/command")
    parser.add_argument("--skip-scale-80", action="store_true")
    args = parser.parse_args()

    engine = Path(args.engine).resolve()
    compose = resolve_compose_command(args.compose)
    version = subprocess.run([*compose, "version"], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=10)
    if version.returncode != 0:
        raise SystemExit(f"Compose is not runnable: {' '.join(compose)}\n{version.stdout}")
    print(version.stdout.strip())

    suite = Suite(engine, compose)
    try:
        suite.test_scale_distinct_ips_and_dns()
        suite.test_ephemeral_ports_route_per_replica()
        suite.test_fixed_port_conflicts()
        suite.test_cross_project_collisions()
        suite.test_network_delete_with_active_endpoints()
        suite.test_network_isolation()
        suite.test_redis_replication()
        if not args.skip_scale_80:
            suite.test_scale_80()
        suite.assert_no_unsupported()
    finally:
        suite.close()
    print("docker-zero real Compose topology suite: ALL CHECKS PASSED")


if __name__ == "__main__":
    main()
