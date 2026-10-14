import os
from pathlib import Path

import attr


@attr.s(auto_attribs=True, frozen=True)
class Config:
    data_dir: Path
    temp_dir: Path
    db_path: Path
    signal_cli_bin: Path

    @property
    def signal_config_dir(self) -> Path:
        return self.data_dir / "signal-cli"

    @property
    def attachments_dir(self) -> Path:
        return self.data_dir / "attachments"

    @property
    def uploads_dir(self) -> Path:
        return self.temp_dir / "uploads"


def _require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"required environment variable {name} is not set")
    return value


def load_config() -> Config:
    data_dir = Path(_require_env("OPENHOST_APP_DATA_DIR"))
    temp_dir = Path(_require_env("OPENHOST_APP_TEMP_DIR"))
    config = Config(
        data_dir=data_dir,
        temp_dir=temp_dir,
        db_path=Path(_require_env("OPENHOST_SQLITE_MAIN")),
        signal_cli_bin=Path(_require_env("SIGNAL_CLI_BIN")),
    )
    for d in (config.signal_config_dir, config.attachments_dir, config.uploads_dir):
        d.mkdir(parents=True, exist_ok=True)
    return config
