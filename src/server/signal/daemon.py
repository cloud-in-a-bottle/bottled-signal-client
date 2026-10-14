import asyncio
from pathlib import Path

from loguru import logger

DAEMON_HOST = "127.0.0.1"
DAEMON_PORT = 7583


class SignalDaemon:
    """Runs `signal-cli daemon` in multi-account mode (needed so `register`/`verify` work before an account exists) and
    restarts it if it exits.

    `--receive-mode on-connection` keeps messages queued on Signal's servers until our SSE reader is connected, instead
    of the daemon acking and dropping them.
    """

    def __init__(self, binary: Path, data_dir: Path) -> None:
        self._binary = binary
        self._data_dir = data_dir
        self._task: asyncio.Task[None] | None = None
        self._process: asyncio.subprocess.Process | None = None

    @property
    def base_url(self) -> str:
        return f"http://{DAEMON_HOST}:{DAEMON_PORT}"

    def start(self) -> None:
        assert self._task is None, "daemon already started"
        self._task = asyncio.create_task(self._run_forever())

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
        if self._process is not None and self._process.returncode is None:
            self._process.terminate()
            await self._process.wait()

    async def _run_forever(self) -> None:
        while True:
            self._process = await asyncio.create_subprocess_exec(
                str(self._binary),
                "--data-dir",
                str(self._data_dir),
                "daemon",
                "--http",
                f"{DAEMON_HOST}:{DAEMON_PORT}",
                "--receive-mode",
                "on-connection",
                "--no-receive-stdout",
                "--ignore-stories",
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.STDOUT,
            )
            assert self._process.stdout is not None
            async for line in self._process.stdout:
                logger.info("signal-cli: {}", line.decode(errors="replace").rstrip())
            code = await self._process.wait()
            logger.error("signal-cli daemon exited with code {}; restarting in 5s", code)
            await asyncio.sleep(5)
