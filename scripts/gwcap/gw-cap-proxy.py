#!/usr/bin/env python3
"""本机网关并发闸：出向 tcp/3003 被 nftables REDIRECT 到本代理，按 model 名做过闸。

部署见 scripts/gwcap/install.sh（gwcap 系统用户 + systemd 双单元 + inet gwcap 表）。
代理一律转发 GWCAP_UPSTREAM；POST body JSON 的 ``model`` 命中 ``GWCAP_MODEL_PREFIX``
的请求先取全局信号量（``GWCAP_LIMIT``），SSE 流式响应持有到 body 传完。
``GET /__gwcap/healthz`` 本地应答不转发，供 ExecStartPre 就绪探测与人工观测。
"""

from __future__ import annotations

import http.client
import json
import logging
import os
import socket
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_LOG = logging.getLogger("gwcap")

_HOP_BY_HOP = frozenset(
    {
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailer",
        "transfer-encoding",
        "upgrade",
    }
)

_CLIENT_TIMEOUT = 620
_UPSTREAM_TIMEOUT = 620
_LOG_WAIT_S = 0.2


@dataclass(frozen=True)
class CapConfig:
    """运行配置，全部来自 GWCAP_* 环境变量。"""

    upstream_host: str = "100.105.212.52"
    upstream_port: int = 3003
    listen_port: int = 3399
    limit: int = 4
    model_prefix: str = "swe-2-medium"

    @classmethod
    def from_env(cls) -> CapConfig:
        """从 GWCAP_* 环境变量构造。"""
        host, _, port = os.environ.get(
            "GWCAP_UPSTREAM", "100.105.212.52:3003"
        ).partition(":")
        return cls(
            upstream_host=host or cls.upstream_host,
            upstream_port=int(port or cls.upstream_port),
            listen_port=int(os.environ.get("GWCAP_PORT", str(cls.listen_port))),
            limit=int(os.environ.get("GWCAP_LIMIT", str(cls.limit))),
            model_prefix=os.environ.get("GWCAP_MODEL_PREFIX", cls.model_prefix),
        )


@dataclass
class Gate:
    """全局请求闸：threading.Semaphore + inflight/queued 计数（healthz 可观测）。"""

    limit: int
    _sem: threading.Semaphore = field(init=False)
    _lock: threading.Lock = field(default_factory=threading.Lock, init=False)
    inflight: int = field(default=0, init=False)
    queued: int = field(default=0, init=False)

    def __post_init__(self) -> None:
        """按 limit 建信号量。"""
        self._sem = threading.Semaphore(self.limit)

    def acquire(self) -> float:
        """取一个槽位，返回排队等待秒数。"""
        with self._lock:
            self.queued += 1
        t0 = time.monotonic()
        self._sem.acquire()
        with self._lock:
            self.queued -= 1
            self.inflight += 1
        return time.monotonic() - t0

    def release(self) -> None:
        """归还槽位。"""
        with self._lock:
            self.inflight -= 1
        self._sem.release()


class CapHandler(BaseHTTPRequestHandler):
    """每连接一线程的转发器；一律 Connection: close，不做 keep-alive。"""

    protocol_version = "HTTP/1.1"
    config: CapConfig
    gate: Gate

    def setup(self) -> None:
        """给客户端 socket 加读超时。"""
        super().setup()
        self.connection.settimeout(_CLIENT_TIMEOUT)

    def _local_json(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def _read_chunked(self) -> bytes:
        buf = bytearray()
        while True:
            size_line = self.rfile.readline(65536).strip()
            size = int(size_line.split(b";", 1)[0] or b"0", 16)
            if size == 0:
                while True:
                    trailer = self.rfile.readline(65536)
                    if trailer in (b"\r\n", b"\n", b""):
                        return bytes(buf)
            buf += self.rfile.read(size)
            self.rfile.read(2)

    def _read_body(self) -> bytes:
        length = self.headers.get("Content-Length")
        if length is not None:
            return self.rfile.read(int(length))
        te = (self.headers.get("Transfer-Encoding") or "").lower()
        if "chunked" in te:
            return self._read_chunked()
        return b""

    def _is_gated(self, body: bytes) -> bool:
        if not body:
            return False
        try:
            model = json.loads(body).get("model")
        except (ValueError, AttributeError):
            return False
        return isinstance(model, str) and model.startswith(self.config.model_prefix)

    def _forward(self, body: bytes) -> None:
        cfg = self.config
        headers = {
            k: v
            for k, v in self.headers.items()
            if k.lower() not in _HOP_BY_HOP and k.lower() != "content-length"
        }
        headers["Content-Length"] = str(len(body))
        headers["Connection"] = "close"
        conn = http.client.HTTPConnection(
            cfg.upstream_host, cfg.upstream_port, timeout=_UPSTREAM_TIMEOUT
        )
        try:
            conn.request(self.command, self.path, body=body or None, headers=headers)
            resp = conn.getresponse()
        except (OSError, http.client.HTTPException) as e:
            conn.close()
            _LOG.warning("upstream fail %s %s: %.160r", self.command, self.path, e)
            self._local_json({"error": "upstream unreachable"}, status=502)
            return
        self.send_response(resp.status)
        for key, val in resp.getheaders():
            if key.lower() in _HOP_BY_HOP or key.lower() == "content-length":
                continue
            self.send_header(key, val)
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            while chunk := resp.read1(65536):
                self.wfile.write(chunk)
                self.wfile.flush()
        except (OSError, http.client.HTTPException) as e:
            _LOG.info("stream abort %s %s: %.120r", self.command, self.path, e)
        finally:
            conn.close()
            self.close_connection = True

    def _relay(self) -> None:
        if self.command == "GET" and self.path == "/__gwcap/healthz":
            gate = self.gate
            self._local_json(
                {
                    "ok": True,
                    "limit": self.config.limit,
                    "model_prefix": self.config.model_prefix,
                    "inflight": gate.inflight,
                    "queued": gate.queued,
                }
            )
            return
        try:
            body = self._read_body()
        except (OSError, ValueError) as e:
            _LOG.info("bad request body %s %s: %.120r", self.command, self.path, e)
            self._local_json({"error": "bad request body"}, status=400)
            return
        gated = self._is_gated(body)
        acquired = False
        try:
            if gated:
                waited = self.gate.acquire()
                acquired = True
                if waited > _LOG_WAIT_S:
                    _LOG.info("cap wait %.1fs %s %s", waited, self.command, self.path)
                else:
                    _LOG.info("cap pass %s %s", self.command, self.path)
            self._forward(body)
        finally:
            if acquired:
                self.gate.release()

    do_GET = _relay  # noqa: N815
    do_POST = _relay  # noqa: N815
    do_PUT = _relay  # noqa: N815
    do_DELETE = _relay  # noqa: N815
    do_PATCH = _relay  # noqa: N815
    do_HEAD = _relay  # noqa: N815
    do_OPTIONS = _relay  # noqa: N815

    def log_message(self, fmt: str, *args: object) -> None:
        """收进 journald。"""
        _LOG.debug("client %s %s", self.address_string(), fmt % args)


class _HTTPServerV4(ThreadingHTTPServer):
    daemon_threads = True
    address_family = socket.AF_INET
    request_queue_size = 256


class _HTTPServerV6(ThreadingHTTPServer):
    daemon_threads = True
    address_family = socket.AF_INET6
    request_queue_size = 256


def main() -> None:
    """双栈起监听，主线程驻留。"""
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s"
    )
    cfg = CapConfig.from_env()
    CapHandler.config = cfg
    CapHandler.gate = Gate(cfg.limit)
    servers: list[ThreadingHTTPServer] = [
        _HTTPServerV4(("127.0.0.1", cfg.listen_port), CapHandler)
    ]
    try:
        servers.append(_HTTPServerV6(("::1", cfg.listen_port), CapHandler))
    except OSError as e:
        _LOG.warning("ipv6 listen disabled: %r", e)
    for srv in servers:
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    _LOG.info(
        "listening :%d -> %s:%d cap=%d prefix=%s",
        cfg.listen_port,
        cfg.upstream_host,
        cfg.upstream_port,
        cfg.limit,
        cfg.model_prefix,
    )
    threading.Event().wait()


if __name__ == "__main__":
    main()
