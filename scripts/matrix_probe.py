#!/usr/bin/env python3
"""Small target used to black-box test orchestrator_matrix.py."""

from __future__ import annotations

import http.client
import json
import os
import socket


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path: str) -> None:
        super().__init__("localhost", timeout=3)
        self.socket_path = socket_path

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.socket_path)


def request(method: str, path: str, body: object | None = None) -> tuple[int, bytes]:
    docker_host = os.environ["DOCKER_HOST"]
    if not docker_host.startswith("unix://"):
        raise RuntimeError(f"unsupported DOCKER_HOST: {docker_host}")
    connection = UnixHTTPConnection(docker_host.removeprefix("unix://"))
    payload = None if body is None else json.dumps(body).encode()
    headers = {} if body is None else {"Content-Type": "application/json"}
    connection.request(method, path, body=payload, headers=headers)
    response = connection.getresponse()
    data = response.read()
    status = response.status
    connection.close()
    return status, data


def create_and_start(name: str, image: str) -> None:
    status, data = request("POST", f"/v1.43/containers/create?name={name}", {"Image": image})
    assert status == 201, (status, data)
    status, data = request("POST", f"/v1.43/containers/{name}/start")
    assert status == 204, (status, data)


def main() -> None:
    status, data = request("GET", "/_ping")
    assert (status, data) == (200, b"OK")
    create_and_start("nginx-zero", "nginx:alpine")
    create_and_start("redis-zero", "redis:7-alpine")
    status, data = request("GET", "/__docker_zero/state")
    assert status == 200, (status, data)
    containers = {item["name"]: item for item in json.loads(data)["containers"]}
    assert containers["nginx-zero"]["seed"] == int(os.environ["DOCKER_ZERO_NGINX_SEED"])
    assert containers["redis-zero"]["seed"] == int(os.environ["DOCKER_ZERO_REDIS_SEED"])


if __name__ == "__main__":
    main()
