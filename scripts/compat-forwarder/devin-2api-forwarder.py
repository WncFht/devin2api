#!/usr/bin/env python3
"""TCP forwarder :3003 -> archbox devin-2api.

Post-migration compat shim: keeps clients still pointing at the old Mac
gateway (loopback/tailnet/LAN :3003) working by forwarding to archbox.
Binds only when :3003 is free — no SO_REUSEPORT, so it can never co-bind
with a live devin-2api and split traffic.
"""
import asyncio
import socket
import time

TARGET_HOST = "100.121.76.120"
TARGET_PORT = 3033
BIND_RETRY_S = 0.3
DIAL_RETRY_S = 90  # bridge archbox restarts without dropping client conns


async def pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
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


async def handle(client_reader: asyncio.StreamReader, client_writer: asyncio.StreamWriter):
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


def make_listen_sock(family: int, v6only: bool | None) -> socket.socket:
    s = socket.socket(family, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if v6only is not None:
        s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1 if v6only else 0)
    addr = "::" if family == socket.AF_INET6 else "0.0.0.0"
    s.bind((addr, 3003))
    s.listen(512)
    s.setblocking(False)
    return s


async def main():
    servers = []
    while not servers:
        try:
            try:
                # single dual-stack socket covers IPv4 + IPv6 loopback/tailnet/LAN
                sock = make_listen_sock(socket.AF_INET6, v6only=False)
                servers = [await asyncio.start_server(handle, sock=sock)]
            except OSError:
                # fallback: separate v4 + v6-only sockets
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
