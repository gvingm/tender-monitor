"""TCP-мост 127.0.0.1:8787 (Windows) -> WSL:8787 для Tailscale Funnel.

Зачем: контейнер tender-monitor работает с --network host внутри WSL
(у rootless docker сломан NAT/DNS на bridge-сетях), а WSL localhostForwarding
нереалбельно пробрасывает порт на Windows. Этот мост слушает localhost
Windows и пересылает соединения в WSL. WSL-IP определяется динамически
на каждое соединение (он может меняться после перезагрузки WSL).

Автозапуск: задача Task Scheduler "TenderMonitorFunnelBridge" (ONLOGON).
"""
import asyncio
import subprocess
import sys

LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = 8787
TARGET_PORT = 8787
WSL_DISTRO = "Ubuntu-24.04"


def wsl_ip() -> str:
    out = subprocess.check_output(
        ["wsl", "-d", WSL_DISTRO, "--", "hostname", "-I"], text=True
    )
    return out.split()[0].strip()


async def pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.IncompleteReadError, OSError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def handle(client_r: asyncio.StreamReader, client_w: asyncio.StreamWriter):
    try:
        target = wsl_ip()
        up_r, up_w = await asyncio.open_connection(target, TARGET_PORT)
    except (OSError, subprocess.SubprocessError, IndexError):
        client_w.close()
        return
    await asyncio.gather(pipe(client_r, up_w), pipe(up_r, client_w))


async def main():
    srv = await asyncio.start_server(handle, LISTEN_HOST, LISTEN_PORT)
    async with srv:
        await srv.serve_forever()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
