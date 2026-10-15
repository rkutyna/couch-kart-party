#!/usr/bin/env python3
"""Port 80 front for the wwfc NAS HTTP server (no root required).

Why this exists: on this Mac a non-root process gets EACCES binding
127.0.0.1:80, but binding the wildcard 0.0.0.0:80 succeeds. So this
listens on 0.0.0.0:80 and forwards to wwfc's NAS listener on
127.0.0.1:8080. Every peer that is not a loopback address is closed
immediately, so nothing from the LAN reaches wwfc through this port.
(The kernel still completes the TCP handshake on non-loopback addresses,
which is the residual exposure of the wildcard bind.)
"""
import asyncio
import ipaddress
import logging
import sys

LISTEN = ("0.0.0.0", 80)
TARGET = ("127.0.0.1", 8080)


def is_loopback(ip):
    try:
        return ipaddress.ip_address(ip).is_loopback
    except ValueError:
        return False


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except (ConnectionError, OSError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def handle(client_reader, client_writer):
    peer = client_writer.get_extra_info("peername")
    ip = peer[0] if peer else ""
    if not is_loopback(ip):
        logging.warning("refusing non-loopback peer %s", ip)
        client_writer.close()
        return
    try:
        up_reader, up_writer = await asyncio.open_connection(*TARGET)
    except OSError as e:
        logging.error("wwfc NAS %s:%d unavailable: %s", TARGET[0], TARGET[1], e)
        client_writer.close()
        return
    await asyncio.gather(
        pipe(client_reader, up_writer),
        pipe(up_reader, client_writer),
    )


async def main():
    server = await asyncio.start_server(handle, *LISTEN, reuse_address=True)
    logging.info("listening on %s:%d -> %s:%d (loopback peers only)", *LISTEN, *TARGET)
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        stream=sys.stdout,
    )
    try:
        asyncio.run(main())
    except OSError as e:
        logging.error("bind %s:%d failed: %s", LISTEN[0], LISTEN[1], e)
        sys.exit(1)
