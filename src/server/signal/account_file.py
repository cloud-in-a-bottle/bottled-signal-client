import base64
import json
import sqlite3
from pathlib import Path

import attr


@attr.s(auto_attribs=True, frozen=True)
class AccountSecrets:
    """Keys signal-cli keeps for the account that its JSON-RPC API doesn't expose, needed to write backups a phone can
    restore."""

    aci: str
    recovery_key: str  # the 64-char Account Entropy Pool
    profile_key: bytes
    media_root_backup_key: bytes | None


class AccountSecretsUnavailable(Exception):
    pass


def read_account_secrets(signal_data_dir: Path, number: str) -> AccountSecrets:
    """Read from signal-cli's own storage: `<data-dir>/data/accounts.json` maps the number to an account file in the
    same directory (see signal-cli's AccountsStore / SignalAccount.Storage)."""
    account = json.loads(_account_path(signal_data_dir, number).read_text())
    aep = account.get("accountEntropyPool")
    profile_key = account.get("profileKey")
    aci = (account.get("aciAccountData") or {}).get("serviceId")
    if not aep:
        raise AccountSecretsUnavailable(
            "signal-cli hasn't created a recovery key for this account yet (it does so on its first storage sync, shortly "
            "after registering); try again in a minute"
        )
    if not profile_key or not aci:
        raise AccountSecretsUnavailable("signal-cli's account file is missing the profile key or ACI")
    media_root = account.get("mediaRootBackupKey")
    return AccountSecrets(
        aci=aci,
        recovery_key=aep,
        profile_key=base64.b64decode(profile_key),
        media_root_backup_key=base64.b64decode(media_root) if media_root else None,
    )


def _account_path(signal_data_dir: Path, number: str) -> Path:
    data_path = signal_data_dir / "data"
    accounts = json.loads((data_path / "accounts.json").read_text())["accounts"]
    matches = [a for a in accounts if a.get("number") == number]
    if len(matches) != 1:
        raise AccountSecretsUnavailable(f"signal-cli has {len(matches)} accounts for {number}")
    return data_path / str(matches[0]["path"])


def read_group_master_keys(signal_data_dir: Path, number: str) -> dict[str, bytes]:
    """Group master keys by base64 group id, from signal-cli's account database (its API doesn't expose them)."""
    account_path = _account_path(signal_data_dir, number)
    db_path = account_path.with_name(account_path.name + ".d") / "account.db"
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        rows = conn.execute("SELECT group_id, master_key FROM group_v2").fetchall()
    finally:
        conn.close()
    return {base64.b64encode(group_id).decode(): bytes(master_key) for group_id, master_key in rows}
