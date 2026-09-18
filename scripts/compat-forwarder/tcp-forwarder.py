#!/usr/bin/env python3
"""Generic TCP forwarder — BIND_HOST:BIND_PORT -> TARGET_HOST:TARGET_PORT.
Compat shim: binds only when the port is free (no SO_REUSEPORT), retries
backend dial for DIAL_RETRY_S so target restarts do not drop client conns.
"""
import asyncio
import os
import socket
import time

BIND_HOST = os.environ.get("BIND_HOST", "::")
BIND_PORT = int(os.environ.get("BIND_PORT", "3003"))
TARGET_HOST = os.environ.get("TARGET_HOST", "127.0.0.1")
TARGET_PORT = int(os.environ.get("TARGET_PORT", "3033"))
BIND_RETRY_S = float(os.environ.get("BIND_RETRY_S", "0.3"))
DIAL_RETRY_S = float(os.environ.get("DIAL_RETRY_S", "90"))


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(1 << 16)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except (ConnectionResetError, BrokenPipeError, OSError):
        pass
    finally:
        try:
            writer.close()
        except OSError:
            pass


async def handle(client_reader, client_writer):
    deadline = time.monotonic() + DIAL_RETRY_S
    backend_writer = None
    while backend_writer is None:
        try:
            backend_reader, backend_writer = await asyncio.open_connection(
                TARGET_HOST, TARGET_PORT
            )
        except OSError:
            if time.monotonic() > deadline:
                client_writer.close()
                return
            await asyncio.sleep(0.5)
    await asyncio.gather(
        pipe(client_reader, backend_writer),
        pipe(backend_reader, client_writer),
    )


def make_listen_sock(family, v6only):
    s = socket.socket(family, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if v6only is not None:
        s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1 if v6only else 0)
    addr = "::" if family == socket.AF_INET6 else "0.0.0.0"
    s.bind((addr, BIND_PORT))
    s.listen(512)
    s.setblocking(False)
    return s


async def main():
    servers = []
    while not servers:
        try:
            try:
                sock = make_listen_sock(socket.AF_INET6, v6only=False)
                servers = [await asyncio.start_server(handle, sock=sock)]
            except OSError:
                s4 = make_listen_sock(socket.AF_INET, None)
                s6 = make_listen_sock(socket.AF_INET6, v6only=True)
                servers = [
                    await asyncio.start_server(handle, sock=s4),
                    await asyncio.start_server(handle, sock=s6),
                ]
        except OSError:
            await asyncio.sleep(BIND_RETRY_S)
    await asyncio.gather(*(s.serve_forever() for s in servers))


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
